// userland_use.go — the `bashy dks use` context selection and the
// `bashy dks local --` explicit escape: the two client verbs that decide
// WHERE the next command runs. Everything here rides pkg/dagrun: selection
// grammar, resolution and typed failures are dagrun's; this file only wires
// them to a plane probe (one kubectl-shaped runner call at a time), the
// per-client state store under $BASHY_HOME, and an injectable local exec.
//
// The fail-closed rule has teeth: an unreachable plane or a missing
// workspace exits non-zero with dagrun's actionable message, having run
// NOTHING — the local executor is never a fallback, only an explicit escape.
package dks

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/dagrun"
	"github.com/qiangli/yoke/pkg/dagrun/cluster"
)

// userlandEnv is the seam every side effect of the userland verbs goes
// through. Production fills it from the process; tests from fakes, so no
// test ever touches the real state dir, the plane, or a process tree.
type userlandEnv struct {
	// stateDir resolves the per-client context state directory.
	stateDir func() string
	// newRunner builds the kubectl-shaped plane runner.
	newRunner func() cluster.Runner
	// mintID mints one workspace identity (ULID).
	mintID func() string
	// execLocal runs one explicit local command and returns its exit code.
	execLocal func(ctx context.Context, name string, args []string) int
}

// defaultUserlandEnv fills the environment from this process: state under
// $BASHY_HOME (fallback $HOME/.bashy), the self-exec kubectl runner, a
// crypto/rand ULID minter and a stdio-wired child process for `local --`.
func defaultUserlandEnv() *userlandEnv {
	return &userlandEnv{
		stateDir:  userlandStateDir,
		newRunner: newSelfRunner,
		mintID:    mintULID,
		execLocal: runLocalCommand,
	}
}

// userlandStateDir is the per-client context state directory:
// $BASHY_HOME/dks/context, falling back to $HOME/.bashy/dks/context.
func userlandStateDir() string {
	if home := strings.TrimSpace(os.Getenv("BASHY_HOME")); home != "" {
		return filepath.Join(home, "dks", "context")
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".bashy", "dks", "context")
	}
	return filepath.Join(home, ".bashy", "dks", "context")
}

// exitStatusError carries one command's own exit status out of cobra, so
// DispatchUserland can return it as the process exit code.
type exitStatusError struct{ code int }

func (e *exitStatusError) Error() string { return fmt.Sprintf("dks userland: exit status %d", e.code) }

// newUserlandUseCmd builds `dks use`: select a workspace, show it, or leave
// the cluster context. No YAML, no kubectl, no kubeconfig on this path.
func newUserlandUseCmd(env *userlandEnv) *cobra.Command {
	var none, create bool
	cmd := &cobra.Command{
		Use:   "use [<name>[@k8s:<context>]] | use @ws:<id> | use --none",
		Short: "Select the workspace later commands run in",
		Long: `use selects the persistent workspace that later commands run in:

  bashy dks use skel             select workspace skel (created on first use)
  bashy dks use skel@k8s:dev     the same name on the plane of k8s context dev
  bashy dks use @ws:<id>         reattach by workspace identity
  bashy dks use --none           leave the cluster context; commands run locally
  bashy dks use                  show the current selection

A name selects the workspace of that name on the plane — created only when
the plane holds none, so every client of the account reaches the same
workspace; two workspaces of one name are an ambiguity that selects nothing.
An unreachable plane or a missing workspace fails with the reason and runs
nothing. --create re-creates a missing workspace explicitly; first use of a
new name creates it by itself.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if none {
				if create || len(args) > 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), "usage: bashy dks use --none takes no workspace")
					return errUserlandHandled
				}
				return env.clearSelection(cmd)
			}
			if len(args) == 0 {
				return env.showSelection(cmd)
			}
			if len(args) != 1 {
				fmt.Fprintln(cmd.ErrOrStderr(), "usage: bashy dks use <name>[@k8s:<context>]")
				return errUserlandHandled
			}
			return env.selectWorkspace(cmd, args[0], create)
		},
	}
	cmd.Flags().BoolVar(&none, "none", false, "leave the cluster context: later commands run locally")
	cmd.Flags().BoolVar(&create, "create", false, "create the workspace if missing (implicit on first use of a new name)")
	return cmd
}

// newUserlandLocalCmd builds `dks local -- <cmd> [args…]`: the ONLY local
// escape. It depends on no cluster state and never consults the selection;
// the explicit `--` separator is required, so nothing local ever runs by
// accident.
func newUserlandLocalCmd(env *userlandEnv) *cobra.Command {
	return &cobra.Command{
		Use:   "local -- <cmd> [args...]",
		Short: "Run one command on this machine, explicitly",
		Long: `local runs exactly the given command on this machine. It is the explicit
local escape — the one way a command runs here regardless of the selected
workspace — and it takes no cluster state into account.

  bashy dks local -- make test

The -- separator is required: without it nothing runs.`,
		DisableFlagParsing: true,
		SilenceUsage:       true,
		SilenceErrors:      true,
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) > 0 && (args[0] == "-h" || args[0] == "--help" || args[0] == "help") {
				fmt.Fprint(cmd.OutOrStdout(), cmd.Long, "\n")
				return nil
			}
			if len(args) < 2 || args[0] != "--" {
				fmt.Fprintln(cmd.ErrOrStderr(), "usage: bashy dks local -- <cmd> [args...]")
				return errUserlandHandled
			}
			code := env.execLocal(cmd.Context(), args[1], args[2:])
			if code != 0 {
				return &exitStatusError{code: code}
			}
			return nil
		},
	}
}

// selectWorkspace parses, resolves against the plane and persists one
// selection. Fail-closed order: parse, probe (reachability first), decide,
// persist only once the plane has answered. A selection BY ID (@ws:<id>)
// resolves strictly by that identity. A selection BY NAME asks the plane
// which workspace record holds the name — the dhnt.io/workspace-name label
// — and selects that record's id, so every client of the account reaches
// the SAME workspace (contract §3 "select (create on first use)"; §7:
// identities belong to the account, never to one client). Only when the
// plane holds NO record of the name does first use mint a fresh ULID and
// create one; two records of one name are an explicit ambiguity that
// selects nothing.
func (env *userlandEnv) selectWorkspace(cmd *cobra.Command, arg string, create bool) error {
	fail := func(err error) error {
		fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks use: %v\n", err)
		return errUserlandHandled
	}
	sel, err := dagrun.ParseSelection(arg)
	if err != nil {
		return fail(err)
	}
	store, err := dagrun.OpenStore(env.stateDir())
	if err != nil {
		return fail(err)
	}
	probe := &KubeProbe{Runner: env.newRunner()}

	if sel.WorkspaceID != "" {
		// By identity: the id is the whole address of the workspace.
		ctxRec, err := dagrun.NewContext(sel, "", "")
		if err != nil {
			return fail(err)
		}
		target, err := dagrun.Resolve(ctxRec, probe)
		var missing *dagrun.WorkspaceMissingError
		if errors.As(err, &missing) && create {
			// --create re-creates the missing record of this identity.
			if err := probe.EnsureWorkspace(cmd.Context(), ctxRec.Plane, ctxRec.WorkspaceID, sel.Name); err != nil {
				return fail(err)
			}
			target, err = dagrun.Target{
				WorkspaceID: ctxRec.WorkspaceID, Name: ctxRec.Name, Plane: ctxRec.Plane, Tier: ctxRec.Tier,
			}, nil
		}
		if err != nil {
			return fail(err)
		}
		return commitSelection(cmd, store, ctxRec, target)
	}

	// By name: reachability first — nothing else may run on an unreachable
	// plane — then the plane answers which workspace holds the name.
	if err := probe.Reachable(sel.Plane); err != nil {
		return fail(&dagrun.PlaneUnreachableError{Plane: sel.Plane, Err: err})
	}
	ids, err := probe.ListWorkspacesByName(sel.Plane, sel.Name)
	if err != nil {
		return fail(err)
	}
	var id string
	switch len(ids) {
	case 1:
		// The plane's record wins over any private mint: this is how a
		// second client reaches the first client's workspace.
		id = ids[0]
	case 0:
		// Create-on-first-use — but a name this client already selected
		// names a workspace the plane no longer holds: never re-mint what
		// a record may still own; fail missing instead (and --create
		// re-applies the known id, which also stamps the name label onto
		// records older than this fix).
		cur, ok, err := store.Current()
		if err != nil {
			return fail(err)
		}
		if ok && cur.Name == sel.Name {
			id = cur.WorkspaceID
			if !create {
				return fail(&dagrun.WorkspaceMissingError{
					Plane:       sel.Plane,
					WorkspaceID: id,
					Err:         fmt.Errorf("no ConfigMap labelled %s in namespace %s", LabelWorkspaceName+"="+sel.Name, userlandNamespace),
				})
			}
		} else {
			id = env.mintID()
		}
		// Create the workspace record (apply exit 0 = the plane has
		// answered), then the selection may persist.
		if err := probe.EnsureWorkspace(cmd.Context(), sel.Plane, id, sel.Name); err != nil {
			return fail(err)
		}
	default:
		return fail(fmt.Errorf("workspace name %q is ambiguous on plane %s: %d records hold it (%s) — nothing was selected; reattach by identity with 'bashy dks use @ws:<id>' after removing the extra records", sel.Name, planeDisplay(sel.Plane), len(ids), strings.Join(ids, ", ")))
	}
	ctxRec, err := dagrun.NewContext(sel, id, "")
	if err != nil {
		return fail(err)
	}
	return commitSelection(cmd, store, ctxRec, dagrun.Target{
		WorkspaceID: ctxRec.WorkspaceID, Name: ctxRec.Name, Plane: ctxRec.Plane, Tier: ctxRec.Tier,
	})
}

// commitSelection persists the resolved context and prints it: the one
// write, only after the plane has answered.
func commitSelection(cmd *cobra.Command, store *dagrun.Store, ctxRec dagrun.Context, target dagrun.Target) error {
	if err := store.Select(ctxRec); err != nil {
		return failUserland(cmd, err)
	}
	name := target.Name
	if name == "" {
		name = target.WorkspaceID
	}
	fmt.Fprintf(cmd.OutOrStdout(), "dks: workspace %s (%s) on plane %s\n", name, target.WorkspaceID, planeDisplay(target.Plane))
	return nil
}

// showSelection prints the current selection, or 'local' when none.
func (env *userlandEnv) showSelection(cmd *cobra.Command) error {
	store, err := dagrun.OpenStore(env.stateDir())
	if err != nil {
		return failUserland(cmd, err)
	}
	ctxRec, ok, err := store.Current()
	if err != nil {
		return failUserland(cmd, err)
	}
	if !ok {
		fmt.Fprintln(cmd.OutOrStdout(), "dks: local — no workspace selected ('bashy dks use <name>' to select one)")
		return nil
	}
	name := ctxRec.Name
	if name == "" {
		name = ctxRec.WorkspaceID
	}
	fmt.Fprintf(cmd.OutOrStdout(), "dks: workspace %s (%s) on plane %s\n", name, ctxRec.WorkspaceID, planeDisplay(ctxRec.Plane))
	return nil
}

// clearSelection is `use --none`: the explicit leave. Later commands run
// locally by explicit absence, not by fallback.
func (env *userlandEnv) clearSelection(cmd *cobra.Command) error {
	store, err := dagrun.OpenStore(env.stateDir())
	if err != nil {
		return failUserland(cmd, err)
	}
	if err := store.Clear(); err != nil {
		return failUserland(cmd, err)
	}
	fmt.Fprintln(cmd.OutOrStdout(), "dks: local — cluster context cleared")
	return nil
}

func failUserland(cmd *cobra.Command, err error) error {
	fmt.Fprintf(cmd.ErrOrStderr(), "bashy dks use: %v\n", err)
	return errUserlandHandled
}

func planeDisplay(plane string) string {
	if plane == "" {
		return "default cluster plane"
	}
	return plane
}

// KubeProbe is the real dagrun.PlaneProbe of the client verbs, built on the
// same Runner semantics as dagrun/cluster: every plane question is one
// kubectl-shaped runner invocation, and the runner owns the binary and
// kubeconfig. Reachability is `get namespace default`; a workspace is its
// labelled ConfigMap in the userland namespace.
type KubeProbe struct {
	Runner cluster.Runner
}

// userlandNamespace hosts the per-workspace records of the client verbs.
const userlandNamespace = "bashy-userland"

// LabelWorkspaceName is the plane label carrying the workspace NAME on the
// workspace ConfigMap. `use <name>` selects existing workspaces by this
// label (besides managed-by), so a second client of the same account
// reaches the first client's workspace instead of minting a sibling record
// of the same name; data.name stays on the record for humans and kubectl.
const LabelWorkspaceName = "dhnt.io/workspace-name"

// workspaceObjectName is the plane object of one workspace record. A ULID
// is uppercase Crockford base32, but an object name must be a lowercase
// RFC 1123 subdomain — the API rejects uppercase — so the NAME lowercases
// the id (as render does for run objects) while the dhnt.io/workspace
// LABEL keeps the id as minted: labels allow uppercase. Lookups go through
// here too, so they always address the same lowercased name.
func workspaceObjectName(workspaceID string) string {
	return "bashy-ws-" + strings.ToLower(workspaceID)
}

// planeContext maps an opaque plane ref to a kubectl --context value: "" is
// the default cluster plane (no flag), k8s:<context> its context. Any other
// plane fails closed, named, before a runner call.
func planeContext(plane string) (string, error) {
	switch {
	case plane == "":
		return "", nil
	case strings.HasPrefix(plane, "k8s:"):
		return strings.TrimPrefix(plane, "k8s:"), nil
	default:
		return "", fmt.Errorf("plane %q is not a Kubernetes plane: this slice reaches '' and 'k8s:<context>' planes only", plane)
	}
}

// probeArgv builds one runner argv: kubectl [--context <ctx>] <args…>.
func probeArgv(plane string, args ...string) ([]string, error) {
	ctxName, err := planeContext(plane)
	if err != nil {
		return nil, err
	}
	argv := []string{"kubectl"}
	if ctxName != "" {
		argv = append(argv, "--context", ctxName)
	}
	return append(argv, args...), nil
}

// Reachable answers whether the plane can be reached: one
// `kubectl get namespace default -o name` that must exit 0.
func (p *KubeProbe) Reachable(plane string) error {
	argv, err := probeArgv(plane, "get", "namespace", "default", "-o", "name")
	if err != nil {
		return err
	}
	_, stderr, exit, err := p.Runner.Run(context.Background(), argv, nil)
	switch {
	case err != nil:
		return fmt.Errorf("kubectl did not run: %w", err)
	case exit != 0:
		return fmt.Errorf("kubectl get namespace default exited %d: %s", exit, firstLine(stderr))
	}
	return nil
}

// WorkspaceExists answers whether the workspace record exists on the plane:
// a ConfigMap bashy-ws-<id> in namespace bashy-userland, labelled
// managed-by=bashy and dhnt.io/workspace=<id>. Selection is by label only;
// the name is matched in the answer, never guessed into the selector.
func (p *KubeProbe) WorkspaceExists(plane, workspaceID string) error {
	if !dagrun.ValidWorkspaceID(workspaceID) {
		return fmt.Errorf("workspace id %q is not a ULID", workspaceID)
	}
	sel := dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + dagrun.LabelWorkspace + "=" + workspaceID
	argv, err := probeArgv(plane, "get", "configmap", "-n", userlandNamespace, "-l", sel, "-o", "name")
	if err != nil {
		return err
	}
	stdout, stderr, exit, err := p.Runner.Run(context.Background(), argv, nil)
	switch {
	case err != nil:
		return fmt.Errorf("kubectl did not run: %w", err)
	case exit != 0:
		return fmt.Errorf("kubectl get configmap exited %d: %s", exit, firstLine(stderr))
	}
	if strings.Contains(string(stdout), workspaceObjectName(workspaceID)) {
		return nil
	}
	return fmt.Errorf("no ConfigMap %s labelled %s in namespace %s", workspaceObjectName(workspaceID), sel, userlandNamespace)
}

// ListWorkspacesByName asks the plane which workspace records hold name:
// ONE `kubectl get configmap -n bashy-userland -l managed-by=bashy,dhnt.io/workspace-name=<name> -o json`
// on the same runner path every plane question rides. The answer is the ids
// from the dhnt.io/workspace labels, in the plane's order. A record of the
// name whose id label is missing or not a ULID is a BROKEN record and fails
// closed rather than being skipped: skipping it could silently mint a
// second workspace of the same name — the very bug this list closes.
func (p *KubeProbe) ListWorkspacesByName(plane, name string) ([]string, error) {
	labelValue, err := workspaceNameLabelValue(name)
	if err != nil {
		return nil, err
	}
	sel := dagrun.LabelManagedBy + "=" + dagrun.ManagedByBashy + "," + LabelWorkspaceName + "=" + labelValue
	argv, err := probeArgv(plane, "get", "configmap", "-n", userlandNamespace, "-l", sel, "-o", "json")
	if err != nil {
		return nil, err
	}
	stdout, stderr, exit, err := p.Runner.Run(context.Background(), argv, nil)
	switch {
	case err != nil:
		return nil, fmt.Errorf("kubectl did not run: %w", err)
	case exit != 0:
		return nil, fmt.Errorf("kubectl get configmap exited %d: %s", exit, firstLine(stderr))
	}
	var list planeConfigmapList
	if err := json.Unmarshal(stdout, &list); err != nil {
		return nil, fmt.Errorf("decode the configmap list of workspace name %q: %w", name, err)
	}
	ids := make([]string, 0, len(list.Items))
	for _, item := range list.Items {
		id := item.Metadata.Labels[dagrun.LabelWorkspace]
		if !dagrun.ValidWorkspaceID(id) {
			return nil, fmt.Errorf("workspace record %s holds name %q but no valid %s label: a broken record must fail closed", item.Metadata.Name, name, dagrun.LabelWorkspace)
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// planeConfigmapList is the slice of `kubectl get configmap -o json` the
// by-name selection parses: only each item's name and labels matter here.
type planeConfigmapList struct {
	Items []struct {
		Metadata struct {
			Name   string            `json:"name"`
			Labels map[string]string `json:"labels"`
		} `json:"metadata"`
	} `json:"items"`
}

// workspaceNameLabelValue maps a workspace name to its LabelWorkspaceName
// value. A Kubernetes label value is 1-63 characters of alphanumerics, '-',
// '_' and '.', starting and ending alphanumeric; a name outside that set
// can neither be selected nor created on a plane and fails closed here —
// never mangled, because two mangled names could collide into one workspace.
func workspaceNameLabelValue(name string) (string, error) {
	edge := func(c byte) bool {
		return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
	}
	if name == "" || len(name) > 63 || !edge(name[0]) || !edge(name[len(name)-1]) {
		return "", fmt.Errorf("workspace name %q is not a usable %s value: 1-63 characters of letters, digits, '-', '_' and '.', starting and ending alphanumeric", name, LabelWorkspaceName)
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; !(c == '-' || c == '_' || c == '.' || edge(c)) {
			return "", fmt.Errorf("workspace name %q holds %q, which a %s value cannot carry: names are selectable when limited to letters, digits, '-', '_' and '.'", name, string(c), LabelWorkspaceName)
		}
	}
	return name, nil
}

// EnsureWorkspace creates the workspace record by server-side apply through
// the runner, one field manager bashy: re-applying is idempotent. The name
// rides BOTH as the dhnt.io/workspace-name label — the key `use <name>`
// selects by — and as data.name. The payload is one v1 List whose first
// item is the userland Namespace itself — labelled managed-by=bashy,
// cluster-scoped, so its re-apply is a no-op — followed by the workspace
// ConfigMap. kubectl applies list items in order, so a FRESH plane (no
// bashy-userland namespace yet) is bootstrapped by the very call that
// writes the record: one runner invocation, no prior setup step.
func (p *KubeProbe) EnsureWorkspace(ctx context.Context, plane, workspaceID, name string) error {
	if !dagrun.ValidWorkspaceID(workspaceID) {
		return fmt.Errorf("workspace id %q is not a ULID", workspaceID)
	}
	labels := map[string]string{
		dagrun.LabelManagedBy: dagrun.ManagedByBashy,
		dagrun.LabelWorkspace: workspaceID,
	}
	if name != "" {
		labelValue, err := workspaceNameLabelValue(name)
		if err != nil {
			return err
		}
		labels[LabelWorkspaceName] = labelValue
	}
	namespace := map[string]any{
		"apiVersion": "v1",
		"kind":       "Namespace",
		"metadata": map[string]any{
			"name":   userlandNamespace,
			"labels": map[string]string{dagrun.LabelManagedBy: dagrun.ManagedByBashy},
		},
	}
	obj := map[string]any{
		"apiVersion": "v1",
		"kind":       "ConfigMap",
		"metadata": map[string]any{
			"name":      workspaceObjectName(workspaceID),
			"namespace": userlandNamespace,
			"labels":    labels,
		},
	}
	if name != "" {
		obj["data"] = map[string]string{"name": name}
	}
	payload, err := json.Marshal(map[string]any{"apiVersion": "v1", "kind": "List", "items": []any{namespace, obj}})
	if err != nil {
		return fmt.Errorf("encode workspace record: %w", err)
	}
	payload = append(payload, '\n')
	argv, err := probeArgv(plane, "apply", "--server-side", "--field-manager="+dagrun.ManagedByBashy, "-f", "-")
	if err != nil {
		return err
	}
	_, stderr, exit, err := p.Runner.Run(ctx, argv, payload)
	switch {
	case err != nil:
		return fmt.Errorf("kubectl did not run: %w", err)
	case exit != 0:
		return fmt.Errorf("kubectl apply of %s exited %d: %s", workspaceObjectName(workspaceID), exit, firstLine(stderr))
	}
	return nil
}

func firstLine(b []byte) string {
	line, _, _ := strings.Cut(string(b), "\n")
	return strings.TrimSpace(line)
}

// execRunner runs one kubectl invocation by re-invoking the CURRENT
// executable (`bashy kubectl …`): kubectl is a managed client served by this
// very binary, never a host binary and never a linked library. A non-nil err
// means the invocation did not run at all; a kubectl failure is its exit.
type execRunner struct{}

func (execRunner) Run(ctx context.Context, argv []string, stdin []byte) ([]byte, []byte, int, error) {
	if len(argv) == 0 || argv[0] != "kubectl" {
		return nil, nil, 0, fmt.Errorf("dks userland: runner argv must start with %q, got %v", "kubectl", argv)
	}
	self, err := os.Executable()
	if err != nil {
		return nil, nil, 0, fmt.Errorf("dks userland: resolve own executable: %w", err)
	}
	c := exec.CommandContext(ctx, self, argv...)
	var stdout, stderr bytes.Buffer
	c.Stdout = &stdout
	c.Stderr = &stderr
	if len(stdin) > 0 {
		c.Stdin = bytes.NewReader(stdin)
	}
	runErr := c.Run()
	exit := 0
	if runErr != nil {
		var ee *exec.ExitError
		if !errors.As(runErr, &ee) {
			return stdout.Bytes(), stderr.Bytes(), 0, fmt.Errorf("dks userland: run %s: %w", self, runErr)
		}
		exit = ee.ExitCode()
	}
	return stdout.Bytes(), stderr.Bytes(), exit, nil
}

// newSelfRunner is the injectable constructor of the concrete exec runner.
var newSelfRunner = func() cluster.Runner { return execRunner{} }

// runLocalCommand runs one explicit local command wired to the process
// stdio and returns its exit status; a failure to start is status 1.
func runLocalCommand(ctx context.Context, name string, args []string) int {
	c := exec.CommandContext(ctx, name, args...)
	c.Stdin = os.Stdin
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	if err := c.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return ee.ExitCode()
		}
		fmt.Fprintf(os.Stderr, "bashy dks local: %v\n", err)
		return 127
	}
	return 0
}

// crockford32 is the ULID alphabet: Crockford base32 (no I, L, O, U).
const crockford32 = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

// mintULID mints one workspace identity: a ULID — 48-bit millisecond
// timestamp, 80 bits of crypto/rand, 26 Crockford base32 characters. The
// identity is owned by the user's account; dagrun validates but never mints
// it, and create-on-first-use comes here.
func mintULID() string {
	var b [16]byte
	ms := uint64(time.Now().UnixMilli())
	for i := 0; i < 6; i++ {
		b[5-i] = byte(ms >> (8 * uint(i)))
	}
	if _, err := rand.Read(b[6:]); err != nil {
		// A broken entropy source must never mint identities: fail loudly,
		// never degrade to a guessable id.
		panic("dks userland: entropy source failed: " + err.Error())
	}
	var out [26]byte
	win, bits := uint32(0), 2 // two leading zero bits: 26×5 = 2+128
	bi := 0
	for i := range out {
		for bits < 5 && bi < len(b) {
			win = win<<8 | uint32(b[bi])
			bi++
			bits += 8
		}
		out[i] = crockford32[(win>>(bits-5))&0x1f]
		bits -= 5
	}
	return string(out[:])
}

// interface guards: the probe is a dagrun.PlaneProbe, the exec runner a
// cluster.Runner.
var (
	_ dagrun.PlaneProbe = (*KubeProbe)(nil)
	_ cluster.Runner    = execRunner{}
)
