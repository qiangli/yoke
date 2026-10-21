// userland_run.go — the `bashy dks run`, `ps` and `stop` client verbs
// (contract §3): run a shipped or registered command as a scheduled workload
// in the selected workspace, list the workspace's runs, stop exactly one.
// Everything rides pkg/dagrun: Generate writes the run manifest,
// dagrun/cluster renders and server-side applies it, lists by label and
// stops by label. This file only resolves the command identity, builds the
// GenerateInput from the selected context, and waits.
//
// The fail-closed rule holds on every path: no selected workspace, an
// unknown command, an unreachable plane or no eligible executor exits
// non-zero having run NOTHING — never locally, never partly on the plane.
package dks

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/dagrun"
	"github.com/qiangli/yoke/pkg/dagrun/cluster"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/secrets"
)

// releaseIdentity is the running bashy release: its version, the sha256 of
// its own executable, the commit it was built from and the platform it runs
// on. A shipped command's identity is the release; its commit names the
// default executor image.
type releaseIdentity struct {
	Version string
	SHA256  string // hex
	Commit  string // hex; "" when the build carries no VCS identity
	GOOS    string // the platform these bytes are built for
	GOARCH  string
}

// String is the release as a manifest records it: version, then commit.
func (r releaseIdentity) String() string {
	if r.Commit == "" {
		return r.Version
	}
	return r.Version + " " + r.Commit
}

// runEnv extends the userland seams with what run/ps/stop need. Production
// fills it from the process; tests from fakes, so no test execs a binary,
// reads the real registry, or sleeps.
type runEnv struct {
	*userlandEnv
	// release reports the running bashy release identity.
	release func() (releaseIdentity, error)
	// shippedArtifacts returns the per-platform artifact digests the release
	// can PROVE for a shipped command, or fails closed naming the platform.
	shippedArtifacts func(name string, rel releaseIdentity) (map[string]string, error)
	// isShipped reports whether name is a builtin shipped in this binary.
	isShipped func(name string) bool
	// selfName is the base name this binary was invoked as ("" = unknown);
	// it, like "bashy", names the session shell itself — a shipped command.
	selfName func() string
	// lookupCommand resolves a registered command record by name or alias.
	lookupCommand func(name string) (fleet.Command, bool)
	// getenv reads one exported variable for a bare `--env K`.
	getenv func(key string) (string, bool)
	// recordRunKind is a registered record's run.kind ("" = absent =
	// one-shot).
	recordRunKind func(rec fleet.Command) dagrun.RunKind
	// stdinIsTerminal reports whether this process's stdin is a terminal,
	// which makes a --session run interactive.
	stdinIsTerminal func() bool
	// image reads $BASHY_DKS_IMAGE ("" = unset); the release's own image
	// is the fallback after it.
	image func() string
	// secretResolver resolves vault credential references into secret values on the client.
	secretResolver func(ctx context.Context, vaultName string) ([]byte, error)
	// listExposes lists the workspace's exposes (userland_expose.go); nil
	// means no expose env reaches a run, which is what a test that is not
	// about exposes wants.
	listExposes func(ctx context.Context, ctxRec dagrun.Context) ([]exposeRecord, error)
	// now and sleep drive the completion wait.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// runPollInterval is how often a waiting run re-lists its phase.
const runPollInterval = 2 * time.Second

// defaultRunTimeout bounds a waiting (non-detached) run.
const defaultRunTimeout = 30 * time.Minute

// defaultScheduleCheck bounds how long a --detach run waits to learn
// whether the scheduler can place it at all before returning its id.
const defaultScheduleCheck = 20 * time.Second

// imageEnvVar names the default Linux executor image.
const imageEnvVar = "BASHY_DKS_IMAGE"

// defaultImagePrefix is the repository of the release's own executor image,
// tagged with the first 8 hex of the bashy commit. One constant so the
// release pipeline can move it to a signed release ref.
const defaultImagePrefix = "localhost/bashy-scratch:"

// defaultImage is the executor image of rel: the client's own bashy build.
// "" when the build carries no commit.
func defaultImage(rel releaseIdentity) string {
	if len(rel.Commit) < 8 {
		return ""
	}
	return defaultImagePrefix + rel.Commit[:8]
}

// resolveImage picks the Linux executor image: --image, then
// $BASHY_DKS_IMAGE, then the release's own image. It returns the image and
// where it came from (flag, env or release), or fails closed.
func (env *runEnv) resolveImage(flag string) (image, from string, err error) {
	if image = strings.TrimSpace(flag); image != "" {
		return image, "flag", nil
	}
	if image = env.image(); image != "" {
		return image, "env", nil
	}
	if rel, rerr := env.release(); rerr == nil {
		if image = defaultImage(rel); image != "" {
			return image, "release", nil
		}
	}
	return "", "", fmt.Errorf("no Linux executor image: this bashy build carries no commit to name its own image — pass --image or set $%s — nothing ran", imageEnvVar)
}

func defaultRunEnv(env *userlandEnv) *runEnv {
	renv := &runEnv{
		userlandEnv:      env,
		release:          selfRelease,
		shippedArtifacts: provenShippedArtifacts,
		isShipped:        func(name string) bool { return tool.Lookup(name) != nil },
		selfName:         selfBaseName,
		lookupCommand:    func(name string) (fleet.Command, bool) { return fleet.New().Command(name) },
		getenv:           os.LookupEnv,
		image:            func() string { return strings.TrimSpace(os.Getenv(imageEnvVar)) },
		secretResolver:   defaultSecretResolver,
		recordRunKind:    fleetRecordRunKind,
		stdinIsTerminal:  func() bool { return term.IsTerminal(int(os.Stdin.Fd())) },
		now:              time.Now,
		sleep:            sleepContext,
	}
	renv.listExposes = func(ctx context.Context, ctxRec dagrun.Context) ([]exposeRecord, error) {
		return planeExposes(ctx, renv, ctxRec)
	}
	return renv
}

// defaultSecretResolver resolves a vault credential by running `bashy secret show <vault-name>`.
func defaultSecretResolver(ctx context.Context, vaultName string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, "bashy", "secret", "show", vaultName)
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return nil, fmt.Errorf("bashy secret show %s: %s", vaultName, bytes.TrimSpace(exitErr.Stderr))
		}
		return nil, fmt.Errorf("bashy secret show %s: %w", vaultName, err)
	}
	return bytes.TrimRight(out, "\r\n"), nil
}

// newUserlandRunCmds builds run, ps and stop over the production seams.
func newUserlandRunCmds(env *userlandEnv) []*cobra.Command {
	return newRunCmds(defaultRunEnv(env))
}

func newRunCmds(env *runEnv) []*cobra.Command {
	return []*cobra.Command{newUserlandRunCmd(env), newUserlandPsCmd(env), newUserlandStopCmd(env)}
}

// runOptions are the flags of one `dks run`.
type runOptions struct {
	detach      bool
	session     bool
	tty         bool
	emit        string
	cpu         int
	memory      string
	env         []string
	credentials []string
	image       string
	timeout     time.Duration
	// scheduleCheck bounds the --detach placement check (0 = none).
	scheduleCheck time.Duration
	verbose       bool
}

func newUserlandRunCmd(env *runEnv) *cobra.Command {
	var o runOptions
	cmd := &cobra.Command{
		Use:   "run [--detach | --session [--tty]] [--cpu N --memory Q] [--env K[=V]]... [--credential <vault-name>[=<ENV_NAME>]]... -- <cmd> [args...]",
		Short: "Run a command as a scheduled workload in the selected workspace",
		Long: `run schedules one shipped or registered command in the selected workspace
and waits for it to finish:

  bashy dks run -- make test        wait; exit 0 on success, 1 on failure
  bashy dks run --detach -- make    print the run id and return at once
  bashy dks run --env CI -- make    carry an exported variable (repeatable)
  bashy dks run --session -- agent  start a detachable session, print its id
                                    and return; 'bashy dks attach <id>' joins it

A registered command runs as its record's run.kind (absent = one-shot); a
shipped command runs one-shot. --session runs the command as a session:
interactive when stdin is a terminal or --tty is given, long-running
otherwise. A session is always detached at start.

Only variables named with --env travel; nothing else of this environment
does. Without a selected workspace, with an unknown command, an unreachable
plane or no eligible executor, run fails with the reason and runs nothing —
never here. --emit manifest prints the generated run manifest and applies
nothing.`,
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return env.run(cmd, o, args)
		},
	}
	f := cmd.Flags()
	f.SetInterspersed(false) // the first word is the command: its flags are its own
	f.BoolVar(&o.detach, "detach", false, "print the run id and return without waiting")
	f.BoolVar(&o.session, "session", false, "run as a detachable session; print 'session <id>' and return")
	f.BoolVar(&o.tty, "tty", false, "with --session: an interactive session even when stdin is not a terminal")
	f.StringVar(&o.emit, "emit", "", "print the run manifest (manifest) or the rendered objects instead of applying")
	_ = f.MarkHidden("emit") // the escape hatch stays off the zero-ceremony path
	f.IntVar(&o.cpu, "cpu", 0, "whole CPUs to request")
	f.StringVar(&o.memory, "memory", "", "memory to request, e.g. 4Gi")
	f.StringArrayVar(&o.env, "env", nil, "carry one variable: K (its exported value) or K=V; repeatable")
	f.StringArrayVar(&o.credentials, "credential", nil, "deliver one credential out-of-band: <vault-name>[=<ENV_NAME>]; repeatable")
	f.StringVar(&o.image, "image", "", "Linux executor image (default $"+imageEnvVar+", then "+defaultImagePrefix+"<bashy commit>)")
	f.BoolVar(&o.verbose, "verbose", false, "report the resolved executor image and where it came from")
	f.DurationVar(&o.timeout, "timeout", defaultRunTimeout, "how long to wait for a terminal phase")
	f.DurationVar(&o.scheduleCheck, "schedule-check", defaultScheduleCheck, "with --detach: how long to check the run can be placed before returning (0 = no check)")
	_ = f.MarkHidden("schedule-check")
	return cmd
}

// run is `dks run`. Order is fail-closed: selection, command identity and
// manifest first (no plane call), then the plane, then apply, then wait.
func (env *runEnv) run(cmd *cobra.Command, o runOptions, args []string) error {
	var secretValues [][]byte
	redact := func(s string) string { return s }
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: %s\n", redact(err.Error()))
		return errUserlandHandled
	}
	switch o.emit {
	case "", "manifest", "yaml":
	default:
		return fail(fmt.Errorf("--emit %q: want manifest or yaml", o.emit))
	}
	if len(args) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "usage: bashy dks run [--detach] -- <cmd> [args...]")
		return errUserlandHandled
	}
	ctxRec, err := env.selected()
	if err != nil {
		return fail(err)
	}
	image, from, imgErr := env.resolveImage(o.image)
	ref, recordKind, err := env.resolveCommand(args[0], executorTarget{image: image, imageErr: imgErr})
	if err != nil {
		return fail(err)
	}
	kind, err := env.runKind(o, ref.Name, recordKind)
	if err != nil {
		return fail(err)
	}
	envVars, err := env.exportedEnv(o.env)
	if err != nil {
		return fail(err)
	}
	// Every LIVE expose of the workspace reaches the run as environment, so
	// a pod learns where the exposed services are without being told. An
	// explicit --env always wins over an expose's recorded entry.
	exposed, err := env.exposes(cmd.Context(), ctxRec)
	if err != nil {
		return fail(planeError(ctxRec, err))
	}
	if vars := exposeRunEnv(exposed, env.now()); len(vars) > 0 {
		if envVars == nil {
			envVars = make(map[string]string, len(vars))
		}
		for k, v := range vars {
			if _, taken := envVars[k]; !taken {
				envVars[k] = v
			}
		}
	}

	secretStringData := make(map[string]string)
	var credRefsForManifest []string
	for _, spec := range o.credentials {
		spec = strings.TrimSpace(spec)
		if spec == "" {
			return fail(errors.New("--credential: empty specification — nothing ran"))
		}
		vaultName, envName, hasEnv := strings.Cut(spec, "=")
		vaultName = strings.TrimSpace(vaultName)
		if vaultName == "" {
			return fail(fmt.Errorf("--credential %q: empty vault name — nothing ran", spec))
		}
		if err := dagrun.ValidateRef(vaultName); err != nil {
			return fail(err)
		}
		if hasEnv {
			envName = strings.TrimSpace(envName)
			if envName == "" {
				return fail(fmt.Errorf("--credential %q: empty environment variable name — nothing ran", spec))
			}
		} else {
			names := secrets.CredentialEnvNames(vaultName)
			if len(names) == 0 || names[0] == "" {
				return fail(fmt.Errorf("--credential %s: no environment variable name mapped; pass --credential %s=<ENV_NAME> explicitly — nothing ran", vaultName, vaultName))
			}
			envName = names[0]
		}
		if !validEnvName(envName) {
			return fail(fmt.Errorf("--credential %q: invalid environment variable name %q — nothing ran", spec, envName))
		}
		if env.secretResolver == nil {
			return fail(errors.New("no secret resolver configured — nothing ran"))
		}
		valBytes, err := env.secretResolver(cmd.Context(), vaultName)
		if err != nil {
			return fail(fmt.Errorf("resolve credential %q: %w — nothing ran", vaultName, err))
		}
		valStr := string(valBytes)
		if strings.TrimSpace(valStr) == "" {
			return fail(fmt.Errorf("credential %q has empty value — nothing ran", vaultName))
		}
		isSubscription := envName == "CLAUDE_CODE_OAUTH_TOKEN" || strings.Contains(strings.ToLower(vaultName), "subscription")
		if isSubscription && strings.HasPrefix(strings.TrimSpace(valStr), "sk-ant-api") {
			return fail(errors.New("metered API key refused for a subscription credential — nothing ran"))
		}
		secretValues = append(secretValues, valBytes)
		secretStringData[envName] = valStr
		credRefsForManifest = append(credRefsForManifest, vaultName+"="+envName)
	}
	if len(secretValues) > 0 {
		redact = func(s string) string {
			return string(dagrun.Redact([]byte(s), secretValues...))
		}
	}

	runID := env.mintID()
	manifest, err := dagrun.Generate(dagrun.GenerateInput{
		Command:     ref,
		Argv:        args,
		WorkspaceID: ctxRec.WorkspaceID,
		RunID:       runID,
		Cwd:         ctxRec.Cwd,
		Env:         envVars,
		Venue:       dag.VenueCluster,
		Run: dagrun.RunSpec{
			Kind:        kind,
			Resources:   dagrun.Resources{CPU: o.cpu, Memory: o.memory},
			Credentials: credRefsForManifest,
		},
	})
	if err != nil {
		return fail(err)
	}
	if o.emit == "manifest" {
		// A registered command's manifest is image-free: the image is
		// reported, not required. A shipped command's names its image.
		if imgErr != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "dks: executor image unresolved: pass --image or set $%s\n", imageEnvVar)
		} else {
			fmt.Fprintf(cmd.ErrOrStderr(), "dks: executor image %s (from %s)\n", image, from)
		}
		_, err := cmd.OutOrStdout().Write(manifest)
		return err
	}
	if imgErr != nil {
		return fail(imgErr)
	}
	if o.verbose || o.emit == "yaml" {
		fmt.Fprintf(cmd.ErrOrStderr(), "dks: executor image %s (from %s)\n", image, from)
	}
	class := dagrun.ExecutorClass{OS: "linux", Image: image}
	if o.emit == "yaml" {
		objs, err := dagrun.Render(manifest, class)
		if err != nil {
			return fail(unsatisfiedOr(err))
		}
		// Deterministic JSON (a YAML subset): map keys are sorted on encode.
		out, err := json.MarshalIndent(map[string]any{"apiVersion": "v1", "kind": "List", "items": objs}, "", "  ")
		if err != nil {
			return fail(err)
		}
		fmt.Fprintf(cmd.OutOrStdout(), "%s\n", out)
		return nil
	}

	client, err := env.planeClient(ctxRec)
	if err != nil {
		return fail(err)
	}
	if len(secretStringData) > 0 {
		if err := env.applySecret(cmd.Context(), client, ctxRec.WorkspaceID, runID, secretStringData); err != nil {
			return fail(planeError(ctxRec, err))
		}
	}
	if _, err := client.Apply(cmd.Context(), manifest, class); err != nil {
		return fail(planeError(ctxRec, err))
	}
	if isSession(kind) {
		// The session id is the run id the attach/logs verbs take.
		fmt.Fprintf(cmd.OutOrStdout(), "session %s\n", runID)
		return nil
	}
	if o.detach {
		if err := env.checkScheduled(cmd, client, ctxRec, runID, resourceRequest(o), o.scheduleCheck, redact); err != nil {
			return err
		}
		fmt.Fprintln(cmd.OutOrStdout(), runID)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "dks: run %s applied to workspace %s; waiting (output streaming and exact exit codes arrive with 'bashy dks logs')\n",
		runID, workspaceDisplay(ctxRec))
	return env.wait(cmd, client, ctxRec, runID, resourceRequest(o), o.timeout, redact)
}

// resourceRequest names the run's resource request as its flags spelled
// it; "" when it requested none.
func resourceRequest(o runOptions) string {
	var parts []string
	if o.cpu > 0 {
		parts = append(parts, fmt.Sprintf("--cpu %d", o.cpu))
	}
	if o.memory != "" {
		parts = append(parts, "--memory "+o.memory)
	}
	return strings.Join(parts, " ")
}

// refuseUnschedulable is the unsatisfied-resource result (contract §2.4):
// the scheduler reported no node can take the run, so its objects are
// deleted by run label — nothing ran — and the run exits non-zero naming
// the scheduler's reason and the request.
func (env *runEnv) refuseUnschedulable(cmd *cobra.Command, client *cluster.Client, ctxRec dagrun.Context, runID, request, reason string, redact func(string) string) error {
	return env.refuseRun(cmd, client, ctxRec, runID, request, "unsatisfied resources: "+reason, redact)
}

// refuseCreate is the result for a Job whose controller cannot create its
// Pod (admission refused it: quota exceeded, request above a limit): no
// Pod will ever exist, so the run is removed like an unschedulable one.
func (env *runEnv) refuseCreate(cmd *cobra.Command, client *cluster.Client, ctxRec dagrun.Context, runID, request, reason string, redact func(string) string) error {
	return env.refuseRun(cmd, client, ctxRec, runID, request, "cannot create pod: "+reason, redact)
}

// refuseRun deletes a run that cannot run by its run label and exits
// non-zero naming msg and the request.
func (env *runEnv) refuseRun(cmd *cobra.Command, client *cluster.Client, ctxRec dagrun.Context, runID, request, msg string, redact func(string) string) error {
	if request != "" {
		msg += " (requested " + request + ")"
	}
	if redact != nil {
		msg = redact(msg)
	}
	if err := env.stopRun(cmd.Context(), client, runID); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: %s; removing run %s failed: %v — 'bashy dks stop %s' to remove it\n",
			msg, runID, planeError(ctxRec, err), runID)
		return &exitStatusError{code: 1}
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: %s — run %s removed, nothing ran\n", msg, runID)
	return &exitStatusError{code: 1}
}

// checkScheduled is the bounded placement check of a --detach run: it
// returns nil once a pod of the run is bound to a node or the check runs
// out (a slow start is not a refusal), and refuses the run when the
// scheduler reports it Unschedulable. A plane error here only warns: the
// run is applied, so its id is still the caller's to manage.
func (env *runEnv) checkScheduled(cmd *cobra.Command, client *cluster.Client, ctxRec dagrun.Context, runID, request string, check time.Duration, redact func(string) string) error {
	if check <= 0 {
		return nil
	}
	deadline := env.now().Add(check)
	for {
		runs, err := client.List(cmd.Context(), ctxRec.WorkspaceID)
		if err != nil {
			errText := planeError(ctxRec, err).Error()
			if redact != nil {
				errText = redact(errText)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: placement of run %s unchecked: %s\n", runID, errText)
			return nil
		}
		for _, r := range runs {
			if r.RunID != runID {
				continue
			}
			if r.Unschedulable != "" {
				return env.refuseUnschedulable(cmd, client, ctxRec, runID, request, r.Unschedulable, redact)
			}
			if reason := jobCreateFailure(cmd.Context(), client, r); reason != "" {
				return env.refuseCreate(cmd, client, ctxRec, runID, request, reason, redact)
			}
			if r.Scheduled || r.Phase == cluster.PhaseSucceeded || r.Phase == cluster.PhaseFailed || r.Phase == cluster.PhaseLost {
				return nil
			}
		}
		if !env.now().Before(deadline) {
			return nil
		}
		if err := env.sleep(cmd.Context(), runPollInterval); err != nil {
			return nil
		}
	}
}

// jobCreateFailure is the API server's message when the run's Job
// controller failed to create its Pod (a FailedCreate event on the Job),
// "" otherwise. Only a Pending Job with no bound Pod is asked: once a Pod
// exists, creation succeeded. The Job's events are read straight through
// the client's runner; an error reading them is not a finding — the next
// poll asks again.
func jobCreateFailure(ctx context.Context, client *cluster.Client, r cluster.RunStatus) string {
	if r.Kind != "Job" || r.Phase != cluster.PhasePending || r.Scheduled || client == nil || client.Runner == nil {
		return ""
	}
	argv := []string{"kubectl"}
	if client.Context != "" {
		argv = append(argv, "--context", client.Context)
	}
	argv = append(argv, "get", "events", "--field-selector",
		"involvedObject.kind=Job,involvedObject.name="+r.Name+",reason=FailedCreate", "-o", "json")
	out, _, exit, err := client.Runner.Run(ctx, argv, nil)
	if err != nil || exit != 0 {
		return ""
	}
	var list struct {
		Items []struct {
			InvolvedObject struct {
				Kind string `json:"kind"`
				Name string `json:"name"`
			} `json:"involvedObject"`
			Reason        string `json:"reason"`
			Message       string `json:"message"`
			LastTimestamp string `json:"lastTimestamp"`
			EventTime     string `json:"eventTime"`
		} `json:"items"`
	}
	if json.Unmarshal(out, &list) != nil {
		return ""
	}
	// The latest event wins; RFC 3339 timestamps order as strings.
	var msg, at string
	for _, e := range list.Items {
		if e.Reason != "FailedCreate" || e.InvolvedObject.Kind != "Job" || e.InvolvedObject.Name != r.Name {
			continue
		}
		ts := e.LastTimestamp
		if ts == "" {
			ts = e.EventTime
		}
		if m := strings.TrimSpace(strings.TrimPrefix(e.Message, "Error creating: ")); m != "" && (msg == "" || ts >= at) {
			msg, at = m, ts
		}
	}
	return msg
}

// runKind resolves the run kind: the record's run.kind (absent =
// one-shot), which --session turns into a session — interactive under a
// terminal or --tty, long-running otherwise. A service is never a session.
func (env *runEnv) runKind(o runOptions, name string, recordKind dagrun.RunKind) (dagrun.RunKind, error) {
	if o.tty && !o.session {
		return "", errors.New("--tty needs --session — nothing ran")
	}
	if !o.session {
		return recordKind, nil
	}
	switch recordKind {
	case dagrun.RunService:
		return "", fmt.Errorf("command %s has run.kind service, which is deployed, not run as a session — nothing ran", name)
	case dagrun.RunInteractive, dagrun.RunLongRunning:
		return recordKind, nil
	}
	if o.tty || env.stdinIsTerminal() {
		return dagrun.RunInteractive, nil
	}
	return dagrun.RunLongRunning, nil
}

func isSession(k dagrun.RunKind) bool {
	return k == dagrun.RunInteractive || k == dagrun.RunLongRunning
}

// fleetRecordRunKind is a registered record's run.kind. The command record
// carries no `run:` block yet, so every registered command is one-shot
// until the registry grows one.
func fleetRecordRunKind(fleet.Command) dagrun.RunKind { return "" }

// wait polls the workspace's runs until runID reaches a terminal phase or
// the timeout passes. Succeeded exits 0; failed and timeout exit 1; lost
// exits the dedicated lost code. A run the
// scheduler reports Unschedulable is refused and removed at once; a plain
// Pending (image pull, slow start) keeps waiting.
func (env *runEnv) wait(cmd *cobra.Command, client *cluster.Client, ctxRec dagrun.Context, runID, request string, timeout time.Duration, redact func(string) string) error {
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	deadline := env.now().Add(timeout)
	phase := cluster.PhasePending
	for {
		runs, err := client.List(cmd.Context(), ctxRec.WorkspaceID)
		if err != nil {
			errText := planeError(ctxRec, err).Error()
			if redact != nil {
				errText = redact(errText)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: %s\n", errText)
			return errUserlandHandled
		}
		for _, r := range runs {
			if r.RunID != runID {
				continue
			}
			phase = r.Phase
			if r.Unschedulable != "" {
				return env.refuseUnschedulable(cmd, client, ctxRec, runID, request, r.Unschedulable, redact)
			}
			if reason := jobCreateFailure(cmd.Context(), client, r); reason != "" {
				return env.refuseCreate(cmd, client, ctxRec, runID, request, reason, redact)
			}
			switch r.Phase {
			case cluster.PhaseSucceeded:
				fmt.Fprintf(cmd.OutOrStdout(), "dks: run %s succeeded\n", runID)
				return nil
			case cluster.PhaseFailed:
				fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: run %s failed (%s/%s reports phase failed)\n", runID, r.Kind, r.Name)
				return &exitStatusError{code: 1}
			case cluster.PhaseLost:
				fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: run %s lost: its executor went away under %s/%s; not re-run — re-run it explicitly\n", runID, r.Kind, r.Name)
				return &exitStatusError{code: sessionLostExitCode}
			}
		}
		if !env.now().Before(deadline) {
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: run %s still %s after %s; it keeps running — 'bashy dks ps' to check, 'bashy dks stop %s' to stop it\n",
				runID, phase, timeout, runID)
			return &exitStatusError{code: 1}
		}
		if err := env.sleep(cmd.Context(), runPollInterval); err != nil {
			errText := err.Error()
			if redact != nil {
				errText = redact(errText)
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: stopped waiting for run %s: %s\n", runID, errText)
			return errUserlandHandled
		}
	}
}

func newUserlandPsCmd(env *runEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "List the selected workspace's runs",
		Long: `ps lists the runs of the selected workspace, oldest first:

  RUN  KIND  COMMAND  PHASE  AGE  EXECUTOR

KIND is one-shot, interactive or long-running; a session's PHASE is its
pod's phase, or lost when the executor killed it before its session server
recorded an exit (a lost session is never re-run).`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			fail := func(err error) error {
				fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks ps: %v\n", err)
				return errUserlandHandled
			}
			ctxRec, err := env.selected()
			if err != nil {
				return fail(err)
			}
			client, err := env.planeClient(ctxRec)
			if err != nil {
				return fail(err)
			}
			runs, err := client.List(cmd.Context(), ctxRec.WorkspaceID)
			if err != nil {
				return fail(planeError(ctxRec, err))
			}
			kinds, err := sessionKinds(cmd.Context(), client, ctxRec.WorkspaceID, runs)
			if err != nil {
				return fail(planeError(ctxRec, err))
			}
			writeRuns(cmd.OutOrStdout(), runs, kinds, ctxRec.WorkspaceID, env.now())
			return nil
		},
	}
}

// sessionKinds reads the run kind of every bare-Pod run (a session) from
// the manifest annotation render wrote on it. A Job is one-shot and needs no
// read, so a workspace with no session costs no extra plane call.
func sessionKinds(ctx context.Context, client *cluster.Client, workspaceID string, runs []cluster.RunStatus) (map[string]dagrun.RunKind, error) {
	hasPod := false
	for _, r := range runs {
		if r.Workspace == workspaceID && r.Kind == "Pod" {
			hasPod = true
			break
		}
	}
	if !hasPod {
		return nil, nil
	}
	if !validLabelValue(workspaceID) {
		return nil, fmt.Errorf("workspace id %q is not a label-safe id", workspaceID)
	}
	argv := []string{"kubectl"}
	if client.Context != "" {
		argv = append(argv, "--context", client.Context)
	}
	sel := dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + dagrun.LabelWorkspace + "=" + workspaceID
	argv = append(argv, "get", "pods", "-l", sel, "-o", "json")
	stdout, stderr, exit, err := client.Runner.Run(ctx, argv, nil)
	switch {
	case err != nil:
		return nil, fmt.Errorf("kubectl did not run (%v): %w", err, dag.ErrWorkerUnreachable)
	case exit != 0:
		return nil, fmt.Errorf("%w: kubectl get pods exited %d: %s", cluster.ErrPlaneRejected, exit, strings.TrimSpace(string(stderr)))
	}
	var list podList
	if err := json.Unmarshal(stdout, &list); err != nil {
		return nil, fmt.Errorf("parse kubectl get pods output: %w", err)
	}
	kinds := map[string]dagrun.RunKind{}
	for _, it := range list.Items {
		id := it.Metadata.Labels[dagrun.LabelRun]
		text := it.Metadata.Annotations[dagrun.AnnotManifest]
		if id == "" || text == "" {
			continue
		}
		if m, err := dagrun.ParseManifest([]byte(text)); err == nil {
			kinds[id] = m.Meta.RunKind
		}
	}
	return kinds, nil
}

// runKindColumn is the KIND of one run: a Job is one-shot, a session Pod is
// the kind its manifest names, "-" when that is unreadable.
func runKindColumn(r cluster.RunStatus, kinds map[string]dagrun.RunKind) string {
	if r.Kind == "Job" {
		return string(dagrun.RunOneShot)
	}
	return orDash(string(kinds[r.RunID]))
}

// writeRuns prints the runs of one workspace in run-id order (ULIDs, so
// oldest first). A run labelled for another workspace is never shown.
func writeRuns(w io.Writer, runs []cluster.RunStatus, kinds map[string]dagrun.RunKind, workspaceID string, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tKIND\tCOMMAND\tPHASE\tAGE\tEXECUTOR")
	for _, r := range runs {
		if r.Workspace != workspaceID {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r.RunID, runKindColumn(r, kinds), orDash(r.Command), r.Phase, runAge(r.RunID, now), r.Kind+"/"+r.Name)
	}
	_ = tw.Flush()
}

func newUserlandStopCmd(env *runEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "stop <run-id>",
		Short: "Stop exactly one run",
		Long: `stop removes exactly the objects of one run of the selected workspace,
selected by its run id — never by name.

  bashy dks stop 01JBQ5E9P2WYZ5ZG4HJ3XRG7W8

A 'run:' reference names a weave run, not a dks run: stop it with weave.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			fail := func(err error) error {
				fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks stop: %v\n", err)
				return errUserlandHandled
			}
			if len(args) != 1 {
				fmt.Fprintln(cmd.ErrOrStderr(), "usage: bashy dks stop <run-id>")
				return errUserlandHandled
			}
			id := args[0]
			if strings.HasPrefix(id, "run:") {
				return fail(fmt.Errorf("%q is a weave run reference, not a dks run — dks run ids are bare ULIDs ('bashy dks ps'); stop a weave run with 'bashy weave'", id))
			}
			if !dagrun.ValidWorkspaceID(id) {
				return fail(fmt.Errorf("run id %q is not a ULID ('bashy dks ps' lists them)", id))
			}
			ctxRec, err := env.selected()
			if err != nil {
				return fail(err)
			}
			client, err := env.planeClient(ctxRec)
			if err != nil {
				return fail(err)
			}
			if err := env.stopRun(cmd.Context(), client, id); err != nil {
				return fail(planeError(ctxRec, err))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dks: stopped run %s\n", id)
			return nil
		},
	}
}

// dksStopKinds is everything Render can emit plus the credential Secret.
const dksStopKinds = "jobs,pods,services,deployments,networkpolicies,secrets"

// stopRun removes exactly the objects of one run from the plane, including
// the workload and its credential Secret, selecting by run label.
func (env *runEnv) stopRun(ctx context.Context, client *cluster.Client, runID string) error {
	if !dagrun.ValidWorkspaceID(runID) {
		return fmt.Errorf("%w: %s=%q", cluster.ErrInvalidSelector, dagrun.LabelRun, runID)
	}
	sel := dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + dagrun.LabelRun + "=" + runID
	argv := []string{"kubectl"}
	if client.Context != "" {
		argv = append(argv, "--context", client.Context)
	}
	argv = append(argv, "delete", dksStopKinds, "-l", sel, "--wait=false")
	_, stderr, exit, err := client.Runner.Run(ctx, argv, nil)
	switch {
	case err != nil:
		return fmt.Errorf("kubectl did not run (%v): %w", err, dag.ErrWorkerUnreachable)
	case exit != 0:
		return fmt.Errorf("%w: kubectl delete exited %d: %s", cluster.ErrPlaneRejected, exit, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// applySecret server-side applies a label-owned v1 Secret before the workload.
func (env *runEnv) applySecret(ctx context.Context, client *cluster.Client, workspaceID, runID string, stringData map[string]string) error {
	secretName := "bashy-cred-" + strings.ToLower(runID)
	secret := map[string]any{
		"apiVersion": "v1",
		"kind":       "Secret",
		"metadata": map[string]any{
			"name": secretName,
			"labels": map[string]any{
				dagrun.LabelManagedBy: dagrun.ManagedByBashy,
				dagrun.LabelRun:       runID,
				dagrun.LabelWorkspace: workspaceID,
			},
		},
		"stringData": stringData,
	}
	payload, err := json.Marshal(secret)
	if err != nil {
		return fmt.Errorf("cluster: encode secret: %w", err)
	}
	payload = append(payload, '\n')
	argv := []string{"kubectl"}
	if client.Context != "" {
		argv = append(argv, "--context", client.Context)
	}
	argv = append(argv, "apply", "--server-side", "--field-manager="+dagrun.ManagedByBashy, "-f", "-")
	_, stderr, exit, err := client.Runner.Run(ctx, argv, payload)
	switch {
	case err != nil:
		return fmt.Errorf("kubectl did not run (%v): %w", err, dag.ErrWorkerUnreachable)
	case exit != 0:
		return fmt.Errorf("%w: kubectl apply secret exited %d: %s", cluster.ErrPlaneRejected, exit, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// validEnvName checks that name is a valid environment variable name: ^[a-zA-Z_][a-zA-Z0-9_]*$
func validEnvName(name string) bool {
	if len(name) == 0 {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		if i == 0 {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_') {
				return false
			}
		} else {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_') {
				return false
			}
		}
	}
	return true
}

// errNoContext is the actionable failure of a cluster verb with no selected
// workspace: nothing runs, and the local escape is named, never taken.
var errNoContext = errors.New("no workspace selected — nothing ran; run 'bashy dks use <name>' to select one, or 'bashy dks local -- <cmd>' to run one command on this machine")

// selected returns the persisted workspace selection or errNoContext.
func (env *runEnv) selected() (dagrun.Context, error) {
	store, err := dagrun.OpenStore(env.stateDir())
	if err != nil {
		return dagrun.Context{}, err
	}
	ctxRec, ok, err := store.Current()
	if err != nil {
		return dagrun.Context{}, err
	}
	if !ok {
		return dagrun.Context{}, errNoContext
	}
	return ctxRec, nil
}

// planeClient is the label-selecting plane client of the selected context.
func (env *runEnv) planeClient(ctxRec dagrun.Context) (*cluster.Client, error) {
	kctx, err := planeContext(ctxRec.Plane)
	if err != nil {
		return nil, err
	}
	return &cluster.Client{Runner: env.newRunner(), Context: kctx}, nil
}

// executorTarget is how the chosen executor class gets a shipped command:
// a Linux executor runs the bashy-built image, which already carries every
// shipped command, so nothing is fetched; a fetching executor (vk-native,
// Stage B) downloads the per-platform bashy artifact.
type executorTarget struct {
	fetches  bool   // the executor fetches the command's artifact
	image    string // the image a Linux executor runs; "" = unresolved
	imageErr error  // why no image resolved
}

// isShippedCommand reports whether name is shipped with this bashy: a
// builtin in the registry, or bashy itself — the session shell, which the
// registry does not list.
func (env *runEnv) isShippedCommand(name string) bool {
	if name == "bashy" {
		return true
	}
	if env.selfName != nil {
		if self := env.selfName(); self != "" && name == self {
			return true
		}
	}
	return env.isShipped(name)
}

// resolveCommand maps argv[0] to its identity: a command shipped with
// bashy is the bashy release (version + commit) plus, on an image
// executor, the image that carries it; a registered command is its record
// digest plus its pinned artifacts. Anything else is unknown and runs
// nowhere. The kind is the registered record's run.kind; a shipped
// command's is "" (one-shot).
func (env *runEnv) resolveCommand(name string, exec executorTarget) (dagrun.CommandRef, dagrun.RunKind, error) {
	if env.isShippedCommand(name) {
		rel, err := env.release()
		if err != nil {
			return dagrun.CommandRef{}, "", fmt.Errorf("command %s: bashy release identity: %w — nothing ran", name, err)
		}
		ref := dagrun.CommandRef{Name: name, Digest: rel.SHA256, Release: rel.String()}
		if exec.fetches {
			// The executor downloads an artifact: its digest must be one
			// this bashy can prove, or nothing runs.
			arts, err := env.shippedArtifacts(name, rel)
			if err != nil {
				return dagrun.CommandRef{}, "", fmt.Errorf("command %s: %w — nothing ran", name, err)
			}
			ref.Artifacts = arts
			return ref, "", nil
		}
		// The executor runs the image, which already holds the command:
		// nothing is fetched, so the identity is the release plus the image.
		if exec.image == "" {
			if exec.imageErr != nil {
				return dagrun.CommandRef{}, "", exec.imageErr
			}
			return dagrun.CommandRef{}, "", fmt.Errorf("command %s: no executor image — nothing ran", name)
		}
		ref.Image = exec.image
		return ref, "", nil
	}
	rec, ok := env.lookupCommand(name)
	if !ok {
		return dagrun.CommandRef{}, "", fmt.Errorf("unknown command %q: neither shipped with bashy nor registered ('bashy commands' lists them) — nothing ran", name)
	}
	ref, err := registeredRef(rec)
	if err != nil {
		return dagrun.CommandRef{}, "", err
	}
	return ref, env.recordRunKind(rec), nil
}

// registeredRef is a registered command's identity: the sha256 of its
// canonical JSON record, plus the per-platform digests the record pins. An
// exec record names a host program and pins no portable artifact, so it is
// ineligible for a cluster run rather than silently unpinned.
func registeredRef(rec fleet.Command) (dagrun.CommandRef, error) {
	body, err := json.Marshal(rec)
	if err != nil {
		return dagrun.CommandRef{}, fmt.Errorf("command %s: encode record: %w", rec.Name, err)
	}
	sum := sha256.Sum256(body)
	ref := dagrun.CommandRef{Name: rec.Name, Digest: hex.EncodeToString(sum[:])}
	switch {
	case rec.Download != nil:
		ref.Artifacts = make(map[string]string, len(rec.Download.SHA256))
		for platform, d := range rec.Download.SHA256 {
			ref.Artifacts[platform] = d
		}
	case len(rec.Exec) > 0:
		return dagrun.CommandRef{}, fmt.Errorf("command %s runs host program %s, which pins no per-platform artifact digest — register it with a download (sha256 per goos/goarch) to run it on a cluster; nothing ran", rec.Name, rec.Exec[0])
	}
	return ref, nil
}

// exportedEnv builds the env that travels from the explicit --env list
// only: K=V as given, a bare K from this process's exported value. The rest
// of the environment never leaves this machine.
func (env *runEnv) exportedEnv(specs []string) (map[string]string, error) {
	if len(specs) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(specs))
	for _, s := range specs {
		k, v, hasValue := strings.Cut(s, "=")
		if k == "" {
			return nil, fmt.Errorf("--env %q: no variable name", s)
		}
		if !hasValue {
			val, ok := env.getenv(k)
			if !ok {
				return nil, fmt.Errorf("--env %s: not set in this environment — pass %s=<value>", k, k)
			}
			v = val
		}
		out[k] = v
	}
	return out, nil
}

// unsatisfiedOr names the capability of a placement no executor offers.
func unsatisfiedOr(err error) error {
	if errors.Is(err, dagrun.ErrUnsupportedExecutor) {
		return fmt.Errorf("no eligible executor: %s — nothing ran", strings.TrimPrefix(err.Error(), dagrun.ErrUnsupportedExecutor.Error()+": "))
	}
	return err
}

// planeError turns a plane-call failure into the actionable message: an
// unreachable plane is dagrun's PlaneUnreachableError, an unsatisfiable
// placement names the capability; never a hint to fall back locally.
func planeError(ctxRec dagrun.Context, err error) error {
	var unsat *cluster.UnsatisfiedError
	switch {
	case errors.As(err, &unsat):
		return fmt.Errorf("no eligible executor: %s — nothing ran", unsat.Capability)
	case errors.Is(err, dag.ErrWorkerUnreachable):
		return &dagrun.PlaneUnreachableError{Plane: ctxRec.Plane, Err: err}
	}
	return err
}

func workspaceDisplay(c dagrun.Context) string {
	if c.Name != "" {
		return c.Name
	}
	return c.WorkspaceID
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// runAge is the age of a run from its ULID timestamp, kubectl-style.
func runAge(runID string, now time.Time) string {
	t, ok := ulidTime(runID)
	if !ok {
		return "-"
	}
	d := now.Sub(t)
	switch {
	case d < 0:
		return "0s"
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d/time.Second))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d/time.Minute))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh", int(d/time.Hour))
	}
	return fmt.Sprintf("%dd", int(d/(24*time.Hour)))
}

// ulidTime decodes the 48-bit millisecond timestamp of a ULID.
func ulidTime(id string) (time.Time, bool) {
	if len(id) != 26 {
		return time.Time{}, false
	}
	var ms uint64
	for i := 0; i < 10; i++ {
		v := strings.IndexByte(crockford32, upperASCII(id[i]))
		if v < 0 {
			return time.Time{}, false
		}
		ms = ms<<5 | uint64(v)
	}
	return time.UnixMilli(int64(ms)), true
}

func upperASCII(b byte) byte {
	if b >= 'a' && b <= 'z' {
		return b - 'a' + 'A'
	}
	return b
}

// selfRelease is the running binary's identity: its module version, the
// sha256 of os.Executable, and the commit it was built from.
func selfRelease() (releaseIdentity, error) {
	self, err := os.Executable()
	if err != nil {
		return releaseIdentity{}, err
	}
	sum, err := fileSHA256(self)
	if err != nil {
		return releaseIdentity{}, err
	}
	version, commit := "(devel)", ""
	if bi, ok := debug.ReadBuildInfo(); ok {
		if bi.Main.Version != "" {
			version = bi.Main.Version
		}
		commit = commitFromBuildInfo(bi)
	}
	return releaseIdentity{Version: version, SHA256: sum, Commit: commit, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH}, nil
}

// selfBaseName is the base name of this executable, without a Windows
// .exe suffix; "" when it cannot be read.
func selfBaseName() string {
	self, err := os.Executable()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(filepath.Base(self), ".exe")
}

// commitFromBuildInfo is the commit a binary was built from: the full
// vcs.revision stamped by the go command, else the 12-hex suffix of a Go
// pseudo-version main module (vX.Y.Z-yyyymmddhhmmss-abcdefabcdef), else "".
func commitFromBuildInfo(bi *debug.BuildInfo) string {
	if bi == nil {
		return ""
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && isHex(s.Value) {
			return s.Value
		}
	}
	v, _, _ := strings.Cut(bi.Main.Version, "+")
	i := strings.LastIndexByte(v, '-')
	if i < 0 {
		return ""
	}
	rev, stamp := v[i+1:], v[:i]
	j := strings.LastIndexAny(stamp, "-.")
	if j < 0 || len(rev) != 12 || !isHex(rev) {
		return ""
	}
	if ts := stamp[j+1:]; len(ts) != 14 || strings.Trim(ts, "0123456789") != "" {
		return ""
	}
	return rev
}

func isHex(s string) bool {
	if s == "" {
		return false
	}
	_, err := hex.DecodeString(strings.Repeat("0", len(s)%2) + s)
	return err == nil
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// provenShippedArtifacts returns only the artifact digests this release
// can prove for an executor that fetches one: the running binary IS the
// linux/<arch> artifact when it is built for a Linux executor arch.
// Anything else fails closed, naming the platform a fetching executor
// needs. An image executor never asks: the image carries the command.
func provenShippedArtifacts(_ string, rel releaseIdentity) (map[string]string, error) {
	if rel.GOOS == "linux" && (rel.GOARCH == "amd64" || rel.GOARCH == "arm64") {
		return map[string]string{"linux/" + rel.GOARCH: rel.SHA256}, nil
	}
	return nil, fmt.Errorf("no artifact digest for linux/amd64 or linux/arm64 (this bashy is %s/%s and proves only its own bytes)", orDash(rel.GOOS), orDash(rel.GOARCH))
}

func sleepContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
