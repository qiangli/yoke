// Package cluster is the cluster-tier dag.Transport of the bashy run manifest:
// it server-side applies dagrun.Render output and selects runs by label for
// ps and stop. It links no Kubernetes client: every plane call is one kubectl
// argv handed to an injected Runner, which owns the binary and kubeconfig.
// Waiting for completion and streaming logs are the session transport's
// (S481.6), not this package's.
package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/dagrun"
)

// Runner runs one kubectl invocation against the plane. argv[0] is always
// "kubectl"; the real runner execs `bashy kubectl` with the rest. A non-nil
// err means the command did not run at all (no exit status); a non-zero exit
// with nil err is kubectl's own failure, explained on stderr.
type Runner interface {
	Run(ctx context.Context, argv []string, stdin []byte) (stdout, stderr []byte, exit int, err error)
}

var (
	// ErrInvalidSelector: an id is not a Kubernetes label value, so it cannot
	// be put in a -l selector as is. Nothing is run; ids are never escaped.
	ErrInvalidSelector = errors.New("cluster: id is not a label-safe selector value")
	// ErrPlaneRejected: the plane was reached and refused the request.
	ErrPlaneRejected = errors.New("cluster: plane rejected the request")
)

// stopKinds is everything Render can emit (the NetworkPolicy included) plus
// the service kinds, so stop leaves nothing of a run behind.
const stopKinds = "jobs,pods,services,deployments,networkpolicies"

// labelValueRE is a Kubernetes label value: at most 63 characters,
// alphanumeric at both ends, `-_.` inside.
var labelValueRE = regexp.MustCompile(`^[A-Za-z0-9]([-A-Za-z0-9_.]{0,61}[A-Za-z0-9])?$`)

// Client issues the plane calls. Context, when set, is passed through as an
// opaque `--context` value; kubeconfig resolution is the Runner's.
type Client struct {
	Runner  Runner
	Context string
}

// ObjectRef names one applied object, for reporting only: ps and stop never
// address an object by name.
type ObjectRef struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Name       string `json:"name"`
}

// Applied is what one Apply put on the plane.
type Applied struct {
	RunID     string      `json:"run_id"`
	Workspace string      `json:"workspace"`
	Objects   []ObjectRef `json:"objects"`
}

// Phase is a run's coarse lifecycle as ps reports it.
type Phase string

const (
	PhasePending   Phase = "pending"
	PhaseRunning   Phase = "running"
	PhaseSucceeded Phase = "succeeded"
	PhaseFailed    Phase = "failed"
	PhaseUnknown   Phase = "unknown" // a Pod the plane reports as Unknown
)

// RunStatus is one run as ps lists it: identity from its labels, phase from
// its workload's status.
type RunStatus struct {
	RunID     string `json:"run_id"`
	Workspace string `json:"workspace"`
	Command   string `json:"command"`
	Kind      string `json:"kind"` // Job or Pod: the run's workload
	Name      string `json:"name"`
	Phase     Phase  `json:"phase"`
	// Scheduled: a Pod of the run is bound to a node (PodScheduled=True,
	// or past Pending).
	Scheduled bool `json:"scheduled,omitempty"`
	// Unschedulable is the scheduler's message when a Pending Pod of the run
	// has PodScheduled=False with reason Unschedulable (FailedScheduling),
	// e.g. "0/5 nodes are available: Insufficient cpu"; "" otherwise.
	Unschedulable string `json:"unschedulable,omitempty"`
}

// UnsatisfiedError: no executor of the requested class can run the manifest
// (native macOS/Windows, services, credentials, …). Nothing was applied, and
// re-placing the attempt cannot help, so it classifies as no-eligible-worker,
// never as unreachable.
type UnsatisfiedError struct {
	Capability string // what is missing, e.g. "native darwin executor (Stage B)"
	Err        error
}

func (e *UnsatisfiedError) Error() string {
	return "cluster: no eligible executor: " + e.Capability
}

func (e *UnsatisfiedError) Unwrap() error { return e.Err }

func (e *UnsatisfiedError) FleetFailure() (dag.RunStatus, dag.FailureReason) {
	return dag.RunInfraFailed, dag.FailureReason{Code: dag.FailNoWorker}
}

// undelivered marks a failure where the body never ran but the plane was not
// unreachable either (rejected apply, bad manifest, foreign task): an infra
// failure with no verdict, never a conformance failure.
type undelivered struct{ err error }

func (e *undelivered) Error() string { return e.err.Error() }
func (e *undelivered) Unwrap() error { return e.err }
func (e *undelivered) FleetFailure() (dag.RunStatus, dag.FailureReason) {
	return dag.RunInfraFailed, dag.FailureReason{Code: dag.FailUnclassified}
}

// Apply renders the manifest for class and server-side applies the objects as
// one v1 List under the bashy field manager. The payload is a pure function
// of (manifest, class), so re-applying is idempotent by construction.
func (c *Client) Apply(ctx context.Context, manifest []byte, class dagrun.ExecutorClass) (Applied, error) {
	m, err := dagrun.ParseManifest(manifest)
	if err != nil {
		return Applied{}, err
	}
	objs, err := dagrun.Render(manifest, class)
	if err != nil {
		if errors.Is(err, dagrun.ErrUnsupportedExecutor) {
			capability := strings.TrimPrefix(err.Error(), dagrun.ErrUnsupportedExecutor.Error()+": ")
			return Applied{}, &UnsatisfiedError{Capability: capability, Err: err}
		}
		return Applied{}, err
	}
	payload, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": objs})
	if err != nil {
		return Applied{}, fmt.Errorf("cluster: encode objects: %w", err)
	}
	payload = append(payload, '\n')
	if _, err := c.run(ctx, []string{"apply", "--server-side", "--field-manager=" + dagrun.ManagedByBashy, "-f", "-"}, payload); err != nil {
		return Applied{}, err
	}
	applied := Applied{RunID: m.Meta.RunID, Workspace: m.Meta.Workspace}
	for _, o := range objs {
		md, _ := o["metadata"].(map[string]any)
		name, _ := md["name"].(string)
		apiVersion, _ := o["apiVersion"].(string)
		kind, _ := o["kind"].(string)
		applied.Objects = append(applied.Objects, ObjectRef{APIVersion: apiVersion, Kind: kind, Name: name})
	}
	return applied, nil
}

// List returns the workspace's runs, selected by the managed-by and workspace
// labels and sorted by run id. A run's Job wins over the Pods it owns.
func (c *Client) List(ctx context.Context, workspaceID string) ([]RunStatus, error) {
	sel, err := selector(dagrun.LabelWorkspace, workspaceID)
	if err != nil {
		return nil, err
	}
	out, err := c.run(ctx, []string{"get", "jobs,pods", "-l", sel, "-o", "json"}, nil)
	if err != nil {
		return nil, err
	}
	return parseRuns(out)
}

// Stop deletes exactly the objects labelled with the run id, without waiting.
// It selects by label only; no object is ever addressed by name.
func (c *Client) Stop(ctx context.Context, runID string) error {
	sel, err := selector(dagrun.LabelRun, runID)
	if err != nil {
		return err
	}
	_, err = c.run(ctx, []string{"delete", stopKinds, "-l", sel, "--wait=false"}, nil)
	return err
}

// selector is `managed-by=bashy,<key>=<value>`, refusing a value that is not
// a label value rather than escaping it.
func selector(key, value string) (string, error) {
	if !labelValueRE.MatchString(value) {
		return "", fmt.Errorf("%w: %s=%q", ErrInvalidSelector, key, value)
	}
	return dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + key + "=" + value, nil
}

// run issues one kubectl call and classifies its failure: a call that never
// ran, or one whose stderr says the server was not reached, wraps
// dag.ErrWorkerUnreachable; any other non-zero exit is ErrPlaneRejected.
func (c *Client) run(ctx context.Context, args []string, stdin []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if c == nil || c.Runner == nil {
		return nil, fmt.Errorf("cluster: no runner configured: %w", dag.ErrWorkerUnreachable)
	}
	argv := []string{"kubectl"}
	if c.Context != "" {
		argv = append(argv, "--context", c.Context)
	}
	argv = append(argv, args...)
	stdout, stderr, exit, err := c.Runner.Run(ctx, argv, stdin)
	if cerr := ctx.Err(); cerr != nil {
		return nil, cerr
	}
	reason := strings.TrimSpace(string(stderr))
	switch {
	case err != nil:
		return nil, fmt.Errorf("cluster: kubectl %s did not run (%v): %w", args[0], err, dag.ErrWorkerUnreachable)
	case exit != 0 && unreachable(reason):
		return nil, fmt.Errorf("cluster: kubectl %s: %s: %w", args[0], reason, dag.ErrWorkerUnreachable)
	case exit != 0:
		return nil, fmt.Errorf("%w: kubectl %s exited %d: %s", ErrPlaneRejected, args[0], exit, reason)
	}
	return stdout, nil
}

// unreachableMarkers are kubectl's own spellings of "the API server was never
// reached". A server-side error ("Error from server …") is not among them.
var unreachableMarkers = []string{
	"Unable to connect to the server",
	"The connection to the server",
	"connection refused",
	"no such host",
	"i/o timeout",
	"TLS handshake timeout",
	"no route to host",
}

func unreachable(stderr string) bool {
	if strings.Contains(stderr, "Error from server") {
		return false
	}
	for _, m := range unreachableMarkers {
		if strings.Contains(stderr, m) {
			return true
		}
	}
	return false
}

// kubeObject is the slice of a Job or Pod that ps reads.
type kubeObject struct {
	Kind     string `json:"kind"`
	Metadata struct {
		Name   string            `json:"name"`
		Labels map[string]string `json:"labels"`
	} `json:"metadata"`
	Status struct {
		Phase      string `json:"phase"` // Pod
		Active     int    `json:"active"`
		Succeeded  int    `json:"succeeded"`
		Failed     int    `json:"failed"`
		Conditions []struct {
			Type    string `json:"type"`
			Status  string `json:"status"`
			Reason  string `json:"reason"`
			Message string `json:"message"`
		} `json:"conditions"`
	} `json:"status"`
}

func parseRuns(out []byte) ([]RunStatus, error) {
	var list struct {
		Items []kubeObject `json:"items"`
	}
	dec := json.NewDecoder(bytes.NewReader(out))
	if err := dec.Decode(&list); err != nil {
		return nil, fmt.Errorf("cluster: parse kubectl get output: %w", err)
	}
	// Jobs first, then by name, so a run's Job always wins over its Pods and
	// the result does not depend on the plane's item order.
	sort.SliceStable(list.Items, func(i, j int) bool {
		a, b := list.Items[i], list.Items[j]
		if (a.Kind == "Job") != (b.Kind == "Job") {
			return a.Kind == "Job"
		}
		return a.Metadata.Name < b.Metadata.Name
	})
	byRun := map[string]RunStatus{}
	for _, o := range list.Items {
		l := o.Metadata.Labels
		id := l[dagrun.LabelRun]
		if id == "" || l[dagrun.LabelManagedBy] != dagrun.ManagedByBashy {
			continue
		}
		if _, seen := byRun[id]; seen {
			continue
		}
		var phase Phase
		switch o.Kind {
		case "Job":
			phase = jobPhase(o)
		case "Pod":
			phase = podPhase(o.Status.Phase)
		default:
			continue
		}
		byRun[id] = RunStatus{
			RunID: id, Workspace: l[dagrun.LabelWorkspace], Command: l[dagrun.LabelCommand],
			Kind: o.Kind, Name: o.Metadata.Name, Phase: phase,
		}
	}
	// A run's Pods carry its scheduling state even when its Job wins.
	for _, o := range list.Items {
		r, ok := byRun[o.Metadata.Labels[dagrun.LabelRun]]
		if !ok || o.Kind != "Pod" {
			continue
		}
		scheduled, unsched := podScheduling(o)
		r.Scheduled = r.Scheduled || scheduled
		if r.Unschedulable == "" {
			r.Unschedulable = unsched
		}
		byRun[r.RunID] = r
	}
	runs := make([]RunStatus, 0, len(byRun))
	for _, r := range byRun {
		runs = append(runs, r)
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].RunID < runs[j].RunID })
	return runs, nil
}

// podScheduling reads one Pod's PodScheduled condition: whether it is bound
// to a node, and the scheduler's message when it is Pending and
// Unschedulable. A Pending Pod with no such condition is neither.
func podScheduling(o kubeObject) (scheduled bool, unschedulable string) {
	if p := o.Status.Phase; p != "" && p != "Pending" {
		return true, ""
	}
	for _, c := range o.Status.Conditions {
		if c.Type != "PodScheduled" {
			continue
		}
		switch {
		case c.Status == "True":
			return true, ""
		case c.Status == "False" && c.Reason == "Unschedulable":
			msg := strings.TrimSpace(c.Message)
			if msg == "" {
				msg = c.Reason
			}
			return false, msg
		}
	}
	return false, ""
}

func jobPhase(o kubeObject) Phase {
	for _, c := range o.Status.Conditions {
		if c.Status != "True" {
			continue
		}
		switch c.Type {
		case "Complete":
			return PhaseSucceeded
		case "Failed":
			return PhaseFailed
		}
	}
	switch {
	case o.Status.Active > 0:
		return PhaseRunning
	case o.Status.Failed > 0:
		return PhaseFailed // backoffLimit 0: one failed pod fails the run
	case o.Status.Succeeded > 0:
		return PhaseSucceeded
	}
	return PhasePending
}

func podPhase(p string) Phase {
	switch p {
	case "", "Pending":
		return PhasePending
	case "Running":
		return PhaseRunning
	case "Succeeded":
		return PhaseSucceeded
	case "Failed":
		return PhaseFailed
	}
	return PhaseUnknown
}

// Transport is the dag.Transport of Venue cluster for one run manifest: Exec
// renders and applies it and reports the submission. It returns once the
// objects are applied; completion and logs are not awaited here.
type Transport struct {
	Client   *Client
	Class    dagrun.ExecutorClass
	Manifest []byte // the run manifest whose `run` target Exec delivers

	closed atomic.Bool
}

var _ dag.Transport = (*Transport)(nil)

// Exec applies the manifest when t is its run target. Failures obey the
// dag EXEC CONTRACT: a plane never reached wraps dag.ErrWorkerUnreachable, a
// class no executor offers is an UnsatisfiedError (no-eligible-worker), and
// anything else that stopped delivery is an unclassified infra failure —
// never a conformance verdict against a body that did not run.
func (x *Transport) Exec(ctx context.Context, w *dag.Worker, t *dag.Task, tio dag.TaskIO) dag.TaskResult {
	start := time.Now()
	res := dag.TaskResult{Name: t.Name, Host: t.Host}
	worker := ""
	if w != nil {
		worker = w.ID
	}
	fail := func(err error) dag.TaskResult {
		res.Status, res.ExitCode, res.Err = dag.StatusFailed, 1, err
		res.Duration = time.Since(start)
		return res
	}
	if x.closed.Load() {
		return fail(fmt.Errorf("worker %q: cluster transport is closed: %w", worker, dag.ErrWorkerUnreachable))
	}
	m, err := dagrun.ParseManifest(x.Manifest)
	if err != nil {
		return fail(&undelivered{err})
	}
	if t.Name != dagrun.TargetName || t.Identity != m.Task.Identity {
		return fail(&undelivered{fmt.Errorf("cluster: task %q is not the run target of manifest %s", t.Name, m.Meta.RunID)})
	}
	applied, err := x.Client.Apply(ctx, x.Manifest, x.Class)
	var unsat *UnsatisfiedError
	switch {
	case err == nil:
	case errors.As(err, &unsat), errors.Is(err, dag.ErrWorkerUnreachable),
		errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return fail(fmt.Errorf("worker %q: %w", worker, err))
	default:
		return fail(&undelivered{fmt.Errorf("worker %q: %w", worker, err)})
	}
	if tio.Stdout != nil {
		refs := make([]string, len(applied.Objects))
		for i, o := range applied.Objects {
			refs[i] = o.Kind + "/" + o.Name
		}
		fmt.Fprintf(tio.Stdout, "run %s applied to workspace %s: %s\n", applied.RunID, applied.Workspace, strings.Join(refs, " "))
	}
	res.Status, res.Duration = dag.StatusDone, time.Since(start)
	return res
}

// Close marks the transport closed; idempotent. There is no connection to
// release: every call is one Runner invocation.
func (x *Transport) Close() error {
	x.closed.Store(true)
	return nil
}
