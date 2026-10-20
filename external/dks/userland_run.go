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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"runtime/debug"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/dag"
	"github.com/qiangli/yoke/pkg/dagrun"
	"github.com/qiangli/yoke/pkg/dagrun/cluster"
	"github.com/qiangli/yoke/pkg/fleet"
)

// releaseIdentity is the running bashy release: its version and the sha256
// of its own executable. A shipped command's identity is the release.
type releaseIdentity struct {
	Version string
	SHA256  string // hex
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
	// lookupCommand resolves a registered command record by name or alias.
	lookupCommand func(name string) (fleet.Command, bool)
	// getenv reads one exported variable for a bare `--env K`.
	getenv func(key string) (string, bool)
	// image is the default Linux executor image ("" = must be given).
	image func() string
	// now and sleep drive the completion wait.
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

// runPollInterval is how often a waiting run re-lists its phase.
const runPollInterval = 2 * time.Second

// defaultRunTimeout bounds a waiting (non-detached) run.
const defaultRunTimeout = 30 * time.Minute

// imageEnvVar names the default Linux executor image.
const imageEnvVar = "BASHY_DKS_IMAGE"

func defaultRunEnv(env *userlandEnv) *runEnv {
	return &runEnv{
		userlandEnv:      env,
		release:          selfRelease,
		shippedArtifacts: provenShippedArtifacts,
		isShipped:        func(name string) bool { return tool.Lookup(name) != nil },
		lookupCommand:    func(name string) (fleet.Command, bool) { return fleet.New().Command(name) },
		getenv:           os.LookupEnv,
		image:            func() string { return strings.TrimSpace(os.Getenv(imageEnvVar)) },
		now:              time.Now,
		sleep:            sleepContext,
	}
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
	detach  bool
	emit    string
	cpu     int
	memory  string
	env     []string
	image   string
	timeout time.Duration
}

func newUserlandRunCmd(env *runEnv) *cobra.Command {
	var o runOptions
	cmd := &cobra.Command{
		Use:   "run [--detach] [--cpu N --memory Q] [--env K[=V]]... -- <cmd> [args...]",
		Short: "Run a command as a scheduled workload in the selected workspace",
		Long: `run schedules one shipped or registered command in the selected workspace
and waits for it to finish:

  bashy dks run -- make test        wait; exit 0 on success, 1 on failure
  bashy dks run --detach -- make    print the run id and return at once
  bashy dks run --env CI -- make    carry an exported variable (repeatable)

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
	f.StringVar(&o.emit, "emit", "", "print the run manifest (manifest) or the rendered objects instead of applying")
	_ = f.MarkHidden("emit") // the escape hatch stays off the zero-ceremony path
	f.IntVar(&o.cpu, "cpu", 0, "whole CPUs to request")
	f.StringVar(&o.memory, "memory", "", "memory to request, e.g. 4Gi")
	f.StringArrayVar(&o.env, "env", nil, "carry one variable: K (its exported value) or K=V; repeatable")
	f.StringVar(&o.image, "image", "", "Linux executor image (default $"+imageEnvVar+")")
	f.DurationVar(&o.timeout, "timeout", defaultRunTimeout, "how long to wait for a terminal phase")
	return cmd
}

// run is `dks run`. Order is fail-closed: selection, command identity and
// manifest first (no plane call), then the plane, then apply, then wait.
func (env *runEnv) run(cmd *cobra.Command, o runOptions, args []string) error {
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: %v\n", err)
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
	ref, err := env.resolveCommand(args[0])
	if err != nil {
		return fail(err)
	}
	envVars, err := env.exportedEnv(o.env)
	if err != nil {
		return fail(err)
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
		Run:         dagrun.RunSpec{Resources: dagrun.Resources{CPU: o.cpu, Memory: o.memory}},
	})
	if err != nil {
		return fail(err)
	}
	if o.emit == "manifest" {
		_, err := cmd.OutOrStdout().Write(manifest)
		return err
	}

	image := strings.TrimSpace(o.image)
	if image == "" {
		image = env.image()
	}
	if image == "" {
		return fail(fmt.Errorf("no Linux executor image: pass --image or set $%s — nothing ran", imageEnvVar))
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
	if _, err := client.Apply(cmd.Context(), manifest, class); err != nil {
		return fail(planeError(ctxRec, err))
	}
	if o.detach {
		fmt.Fprintln(cmd.OutOrStdout(), runID)
		return nil
	}
	fmt.Fprintf(cmd.OutOrStdout(), "dks: run %s applied to workspace %s; waiting (output streaming and exact exit codes arrive with 'bashy dks logs')\n",
		runID, workspaceDisplay(ctxRec))
	return env.wait(cmd, client, ctxRec, runID, o.timeout)
}

// wait polls the workspace's runs until runID reaches a terminal phase or
// the timeout passes. Succeeded exits 0; failed and timeout exit 1.
func (env *runEnv) wait(cmd *cobra.Command, client *cluster.Client, ctxRec dagrun.Context, runID string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = defaultRunTimeout
	}
	deadline := env.now().Add(timeout)
	phase := cluster.PhasePending
	for {
		runs, err := client.List(cmd.Context(), ctxRec.WorkspaceID)
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: %v\n", planeError(ctxRec, err))
			return errUserlandHandled
		}
		for _, r := range runs {
			if r.RunID != runID {
				continue
			}
			phase = r.Phase
			switch r.Phase {
			case cluster.PhaseSucceeded:
				fmt.Fprintf(cmd.OutOrStdout(), "dks: run %s succeeded\n", runID)
				return nil
			case cluster.PhaseFailed:
				fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: run %s failed (%s/%s reports phase failed)\n", runID, r.Kind, r.Name)
				return &exitStatusError{code: 1}
			}
		}
		if !env.now().Before(deadline) {
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: run %s still %s after %s; it keeps running — 'bashy dks ps' to check, 'bashy dks stop %s' to stop it\n",
				runID, phase, timeout, runID)
			return &exitStatusError{code: 1}
		}
		if err := env.sleep(cmd.Context(), runPollInterval); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks run: stopped waiting for run %s: %v\n", runID, err)
			return errUserlandHandled
		}
	}
}

func newUserlandPsCmd(env *runEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "ps",
		Short: "List the selected workspace's runs",
		Long: `ps lists the runs of the selected workspace, oldest first:

  RUN  COMMAND  PHASE  AGE  EXECUTOR`,
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
			writeRuns(cmd.OutOrStdout(), runs, ctxRec.WorkspaceID, env.now())
			return nil
		},
	}
}

// writeRuns prints the runs of one workspace in run-id order (ULIDs, so
// oldest first). A run labelled for another workspace is never shown.
func writeRuns(w io.Writer, runs []cluster.RunStatus, workspaceID string, now time.Time) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RUN\tCOMMAND\tPHASE\tAGE\tEXECUTOR")
	for _, r := range runs {
		if r.Workspace != workspaceID {
			continue
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", r.RunID, orDash(r.Command), r.Phase, runAge(r.RunID, now), r.Kind+"/"+r.Name)
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
			if err := client.Stop(cmd.Context(), id); err != nil {
				return fail(planeError(ctxRec, err))
			}
			fmt.Fprintf(cmd.OutOrStdout(), "dks: stopped run %s\n", id)
			return nil
		},
	}
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

// resolveCommand maps argv[0] to its identity: a builtin shipped in this
// binary is the bashy release; a registered command is its record digest
// plus its pinned artifacts. Anything else is unknown and runs nowhere.
func (env *runEnv) resolveCommand(name string) (dagrun.CommandRef, error) {
	if env.isShipped(name) {
		rel, err := env.release()
		if err != nil {
			return dagrun.CommandRef{}, fmt.Errorf("command %s: bashy release identity: %w — nothing ran", name, err)
		}
		arts, err := env.shippedArtifacts(name, rel)
		if err != nil {
			return dagrun.CommandRef{}, fmt.Errorf("command %s: %w — nothing ran", name, err)
		}
		return dagrun.CommandRef{Name: name, Digest: rel.SHA256, Artifacts: arts}, nil
	}
	rec, ok := env.lookupCommand(name)
	if !ok {
		return dagrun.CommandRef{}, fmt.Errorf("unknown command %q: neither shipped with bashy nor registered ('bashy commands' lists them) — nothing ran", name)
	}
	return registeredRef(rec)
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

// selfRelease is the running binary's identity: its module version and the
// sha256 of os.Executable.
func selfRelease() (releaseIdentity, error) {
	self, err := os.Executable()
	if err != nil {
		return releaseIdentity{}, err
	}
	sum, err := fileSHA256(self)
	if err != nil {
		return releaseIdentity{}, err
	}
	version := "(devel)"
	if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
		version = bi.Main.Version
	}
	return releaseIdentity{Version: version, SHA256: sum}, nil
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

// provenShippedArtifacts returns only the artifact digests this process
// can prove: the running binary IS the linux/<arch> artifact when it runs
// on a Linux executor arch. Anything else fails closed, naming the platform
// a Linux executor needs.
func provenShippedArtifacts(_ string, rel releaseIdentity) (map[string]string, error) {
	if runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64") {
		return map[string]string{"linux/" + runtime.GOARCH: rel.SHA256}, nil
	}
	return nil, fmt.Errorf("no artifact digest for linux/amd64 or linux/arm64 (this bashy is %s/%s and proves only its own bytes)", runtime.GOOS, runtime.GOARCH)
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
