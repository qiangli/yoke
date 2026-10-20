// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package dagrun is the DKS cluster-userland execution layer shared by bashy
// and outpost: the selected workspace context (this file), the run manifest
// generator and the per-tier transports. The context owns WHERE the next
// command runs; execution stays there, and local execution happens only when
// the user asks for it explicitly.
package dagrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/qiangli/yoke/pkg/ref"
)

// Tier is the execution tier a context places work on. It mirrors the per-tier
// executors of the cluster-userland contract: userland (this host, in-process),
// sandbox (container), sphere (a named peer host) and cluster (the plane's
// scheduler).
type Tier string

const (
	TierUserland Tier = "userland"
	TierSandbox  Tier = "sandbox"
	TierSphere   Tier = "sphere"
	TierCluster  Tier = "cluster"
)

// Context is the selected persistent execution context of one client: which
// workspace, on which plane, at which tier, in which logical directory. It is
// the record `bashy dks use` persists and every later command reads.
type Context struct {
	// WorkspaceID is the workspace identity: a ULID owned by the user's
	// account, never a plane-local name. dagrun never mints it; the caller
	// supplies it (create-on-first-use mints a fresh ULID).
	WorkspaceID string `json:"workspace_id"`
	// Name is the workspace name the user selected ("" when reattaching by
	// id only).
	Name string `json:"name,omitempty"`
	// Plane is the opaque plane/context ref, e.g. the `k8s:<context>` scope
	// string of the selection or a bare host for the sphere tier. "" is the
	// default cluster plane this client is paired with.
	Plane string `json:"plane,omitempty"`
	// Tier is the execution tier. Never userland: local execution is
	// explicit only (`bashy dks local -- <cmd>`).
	Tier Tier `json:"tier"`
	// Cwd is the logical working directory under the workspace home: "",
	// "~", or a "~/"-prefixed or relative path that stays inside the home.
	Cwd string `json:"cwd,omitempty"`
}

// Validate fails closed on a context that must never be persisted or
// dispatched.
func (c Context) Validate() error {
	if !ValidWorkspaceID(c.WorkspaceID) {
		return fmt.Errorf("dagrun: workspace id %q is not a ULID: the caller must supply the workspace identity", c.WorkspaceID)
	}
	switch c.Tier {
	case TierSandbox, TierSphere, TierCluster:
	case TierUserland:
		return fmt.Errorf("dagrun: tier userland is never a selected context: local execution is explicit only ('bashy dks local -- <cmd>')")
	default:
		return fmt.Errorf("dagrun: unknown tier %q", string(c.Tier))
	}
	return validLogicalCwd(c.Cwd)
}

// Selection is one parsed `bashy dks use <workspace>[@<scope>]` argument.
// The locator grammar of pkg/ref is reused: 'name', 'name@k8s:<context>',
// 'name@<host>', 'name@sandbox:<host>', '@ws:<id>'.
type Selection struct {
	// Name is the selected workspace name ("" when selecting by id).
	Name string
	// WorkspaceID is set when the selection reattaches by identity (@ws:).
	WorkspaceID string
	// Plane is the opaque plane ref ("" = the default cluster plane).
	Plane string
	// Tier is the tier the scope implies (no scope and k8s:/cloud: are the
	// cluster tier, sandbox: the sandbox tier, a bare host the sphere tier).
	Tier Tier
}

// ParseSelection parses a `bashy dks use` argument with the pkg/ref locator
// grammar. The resource is read as a workspace NAME, never as the parser's
// default dir scheme; `run:` (a weave run ref) and every other vocabulary or
// recipe-scheme resource is rejected.
func ParseSelection(arg string) (Selection, error) {
	if arg == "" {
		return Selection{}, errors.New("dagrun: empty workspace selection: use 'name', 'name@k8s:<context>', 'name@<host>' or '@ws:<id>'")
	}
	if strings.TrimSpace(arg) != arg {
		return Selection{}, fmt.Errorf("dagrun: invalid workspace selection %q: surrounding whitespace", arg)
	}
	loc, err := ref.ParseLocator(arg)
	if err != nil {
		return Selection{}, fmt.Errorf("dagrun: invalid workspace selection %q: %v", arg, err)
	}
	if loc.URN || len(loc.Inner) > 0 {
		return Selection{}, fmt.Errorf("dagrun: invalid workspace selection %q: a workspace is 'name', 'name@k8s:<context>', 'name@<host>' or '@ws:<id>', not a URN or container descent", arg)
	}
	if ref.Known(ref.Kind(loc.Scheme)) {
		if loc.Scheme == string(ref.Run) {
			return Selection{}, fmt.Errorf("dagrun: %q selects a weave run, not a workspace: DKS workspaces are 'name', 'name@k8s:<context>', 'name@<host>' or '@ws:<id>'", arg)
		}
		return Selection{}, fmt.Errorf("dagrun: %q is a %s ref, not a workspace name", arg, loc.Scheme)
	}
	if loc.Scheme != "dir" {
		return Selection{}, fmt.Errorf("dagrun: %q names a %s resource, not a workspace name", arg, loc.Scheme)
	}

	sel := Selection{Name: loc.Target}
	switch loc.ScopeKind {
	case "":
		sel.Tier = TierCluster
	case "k8s":
		sel.Tier = TierCluster
		sel.Plane = "k8s:" + loc.ScopeValue
	case "cloud":
		sel.Tier = TierCluster
		sel.Plane = "cloud:" + loc.ScopeValue
	case "sandbox":
		sel.Tier = TierSandbox
		sel.Plane = "sandbox:" + loc.ScopeValue
	case "host":
		sel.Tier = TierSphere
		sel.Plane = loc.ScopeValue
	case "ws":
		if loc.Target != "" {
			return Selection{}, fmt.Errorf("dagrun: invalid workspace selection %q: '@ws:<id>' selects by workspace id and cannot carry a name", arg)
		}
		if !ValidWorkspaceID(loc.ScopeValue) {
			return Selection{}, fmt.Errorf("dagrun: invalid workspace selection %q: '@ws:' needs a workspace id (ULID)", arg)
		}
		sel = Selection{WorkspaceID: loc.ScopeValue, Tier: TierCluster}
	default:
		return Selection{}, fmt.Errorf("dagrun: invalid workspace selection %q: unknown scope %q", arg, loc.ScopeKind)
	}
	if sel.Name != "" && (strings.ContainsAny(sel.Name, `/\`) || strings.Contains(sel.Name, "~")) {
		return Selection{}, fmt.Errorf("dagrun: invalid workspace selection %q: %q is a name, not a path", arg, sel.Name)
	}
	return sel, nil
}

// NewContext builds the context record for a selection. workspaceID is the
// caller-supplied workspace identity (a freshly minted ULID on first use); it
// must be empty when the selection already carries its id. cwd is the logical
// working directory ("" or "~" = workspace home).
func NewContext(sel Selection, workspaceID, cwd string) (Context, error) {
	id := sel.WorkspaceID
	if id == "" {
		id = workspaceID
	} else if workspaceID != "" && workspaceID != sel.WorkspaceID {
		return Context{}, fmt.Errorf("dagrun: selection id %s and supplied id %q disagree", sel.WorkspaceID, workspaceID)
	}
	ctx := Context{WorkspaceID: id, Name: sel.Name, Plane: sel.Plane, Tier: sel.Tier, Cwd: cwd}
	if err := ctx.Validate(); err != nil {
		return Context{}, err
	}
	return ctx, nil
}

// ValidWorkspaceID reports whether id is ULID-shaped: 26 Crockford base32
// characters. Identities are globally unique ULIDs owned by the user's
// account, not plane-local names; dagrun validates them but never mints them.
func ValidWorkspaceID(id string) bool {
	if len(id) != 26 {
		return false
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		switch {
		case c >= '0' && c <= '9':
		case c >= 'A' && c <= 'H':
		case c == 'J', c == 'K':
		case c == 'M', c == 'N':
		case c >= 'P' && c <= 'T':
		case c >= 'V' && c <= 'Z':
		default:
			return false
		}
	}
	return true
}

func validLogicalCwd(cwd string) error {
	if cwd == "" || cwd == "~" {
		return nil
	}
	rest := cwd
	if strings.HasPrefix(cwd, "~") {
		if !strings.HasPrefix(cwd, "~/") {
			return fmt.Errorf("dagrun: cwd %q is not under the workspace home: only '~' and '~/...' are workspace paths", cwd)
		}
		rest = cwd[2:]
	}
	if rest == "" {
		return nil
	}
	if strings.HasPrefix(rest, "/") || strings.Contains(rest, `\`) || strings.Contains(rest, "~") {
		return fmt.Errorf("dagrun: cwd %q is not a logical path under the workspace home", cwd)
	}
	for _, seg := range strings.Split(rest, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("dagrun: cwd %q is not a clean path under the workspace home", cwd)
		}
	}
	return nil
}

// Store persists the selected context of one client as one JSON file in a
// caller-given state directory (one dir per client). It reads no environment
// and no shared state: which directory to use, and the record to persist, are
// the caller's decisions. Select is an atomic write (temp file + rename), so
// a crash mid-select leaves the previous selection intact.
type Store struct {
	path string
}

const stateFileName = "context.json"

// OpenStore opens (creating if needed) the per-client state directory dir.
func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, errors.New("dagrun: empty state dir")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("dagrun: state dir: %w", err)
	}
	return &Store{path: filepath.Join(dir, stateFileName)}, nil
}

// Select persists ctx as the current context. It fails closed on an invalid
// context; it never overwrites the previous selection with one.
func (s *Store) Select(ctx Context) error {
	if err := ctx.Validate(); err != nil {
		return err
	}
	b, err := json.MarshalIndent(ctx, "", "  ")
	if err != nil {
		return fmt.Errorf("dagrun: encode context: %w", err)
	}
	b = append(b, '\n')
	tmp, err := os.CreateTemp(filepath.Dir(s.path), stateFileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("dagrun: state dir: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return fmt.Errorf("dagrun: write context: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("dagrun: sync context: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("dagrun: write context: %w", err)
	}
	if err := os.Chmod(tmp.Name(), 0o600); err != nil {
		return fmt.Errorf("dagrun: state file: %w", err)
	}
	if err := os.Rename(tmp.Name(), s.path); err != nil {
		return fmt.Errorf("dagrun: commit context: %w", err)
	}
	return nil
}

// Current returns the selected context. ok=false means no selection: the
// client is local, by explicit absence rather than by fallback. A corrupt or
// invalid state file is an error, never ok=false.
func (s *Store) Current() (ctx Context, ok bool, err error) {
	b, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Context{}, false, nil
	}
	if err != nil {
		return Context{}, false, fmt.Errorf("dagrun: read context state: %w", err)
	}
	if err := json.Unmarshal(b, &ctx); err != nil {
		return Context{}, false, fmt.Errorf("dagrun: corrupt context state %s: %w", s.path, err)
	}
	if err := ctx.Validate(); err != nil {
		return Context{}, false, fmt.Errorf("dagrun: invalid context state %s: %w", s.path, err)
	}
	return ctx, true, nil
}

// Clear removes the selection (`bashy dks use --none`): later commands run
// locally, explicitly. A missing selection is not an error.
func (s *Store) Clear() error {
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("dagrun: clear context: %w", err)
	}
	return nil
}

// PlaneProbe answers the two questions dispatch needs before anything is
// scheduled. dagrun itself never links a cluster client; the caller (CLI,
// transport, test) provides the reachability implementation.
type PlaneProbe interface {
	// Reachable reports whether plane (opaque, e.g. "k8s:dev"; "" is the
	// default cluster plane) can be reached from this client.
	Reachable(plane string) error
	// WorkspaceExists reports whether workspace workspaceID has a home on
	// plane.
	WorkspaceExists(plane, workspaceID string) error
}

// Target is a resolved execution placement. A userland target exists only
// through LocalEscape; Resolve never produces one.
type Target struct {
	WorkspaceID string
	Name        string
	Plane       string
	Tier        Tier
	Cwd         string
}

// IsLocal reports whether the target is the explicit local escape.
func (t Target) IsLocal() bool { return t.Tier == TierUserland }

// LocalEscape is the ONLY way to obtain a local target: the explicit
// `bashy dks local -- <cmd>` escape for one command. It is never the result of
// a resolution, a fallback, or a persisted selection.
func LocalEscape() Target { return Target{Tier: TierUserland} }

// PlaneUnreachableError is the typed failure of an unreachable plane: the
// selected context stays selected, nothing executed, and the user decides how
// to proceed.
type PlaneUnreachableError struct {
	Plane string
	Err   error
}

func (e *PlaneUnreachableError) Error() string {
	return fmt.Sprintf("dagrun: plane %s unreachable: %v — nothing ran; run 'bashy dks use --none' to leave the cluster context, or 'bashy dks local -- <cmd>' to run one command locally", planeLabel(e.Plane), e.Err)
}

func (e *PlaneUnreachableError) Unwrap() error { return e.Err }

// WorkspaceMissingError is the typed failure of a selected workspace that has
// no home on its plane.
type WorkspaceMissingError struct {
	Plane       string
	WorkspaceID string
	Err         error
}

func (e *WorkspaceMissingError) Error() string {
	return fmt.Sprintf("dagrun: workspace %s missing on plane %s: %v — nothing ran; run 'bashy dks use <name>@<plane scope>' to re-create it, or 'bashy dks use --none' / 'bashy dks local -- <cmd>' to stay local", e.WorkspaceID, planeLabel(e.Plane), e.Err)
}

func (e *WorkspaceMissingError) Unwrap() error { return e.Err }

func planeLabel(plane string) string {
	if plane == "" {
		return "(default cluster plane)"
	}
	return fmt.Sprintf("%q", plane)
}

// Resolve validates the selected context against the plane and returns the
// execution target. An unreachable plane or a missing workspace fails closed
// with a typed, actionable error; the local executor is never a fallback: a
// local target exists only through LocalEscape.
func Resolve(ctx Context, probe PlaneProbe) (Target, error) {
	if probe == nil {
		return Target{}, errors.New("dagrun: resolve needs a plane probe: failing closed")
	}
	if err := ctx.Validate(); err != nil {
		return Target{}, err
	}
	if err := probe.Reachable(ctx.Plane); err != nil {
		return Target{}, &PlaneUnreachableError{Plane: ctx.Plane, Err: err}
	}
	if err := probe.WorkspaceExists(ctx.Plane, ctx.WorkspaceID); err != nil {
		return Target{}, &WorkspaceMissingError{Plane: ctx.Plane, WorkspaceID: ctx.WorkspaceID, Err: err}
	}
	return Target{WorkspaceID: ctx.WorkspaceID, Name: ctx.Name, Plane: ctx.Plane, Tier: ctx.Tier, Cwd: ctx.Cwd}, nil
}
