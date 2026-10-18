package lenscmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// S18 first slice: `@ws:<id>` and `@sandbox:<id>` read a local weave workspace
// or sandbox, read-only. The scope is only ever the one spelled on the locator —
// nothing infers it from the environment or the current checkout — and anything
// that cannot be resolved to exactly one existing target fails closed.
//
// For workspaces: <stateRoot>/<tag>/workspaces/<name>
// For sandboxes:  <stateRoot>/<tag>/sandboxes/<name>
//
// <id> is `<queue-tag>/<name>` (exact) or `<name>` (must be unique across
// the queues on this host). The layout is weave's own, with ~/.bashy/weave
// first and the pre-re-home roots after it (pkg/weave weaveStateRoot/weaveLegacyStateRoots).
// cmds/lens does not import pkg/weave: it would drag the orchestrator into
// cmds/all for a path rule.

// wsProviders are the schemes whose provider is a pure view of a directory
// tree and can therefore be rooted at a workspace or sandbox.
var wsProviders = map[string]func(root string) provider.Provider{
	"dir": func(root string) provider.Provider { return provider.NewDir(root, false) },
}

func weaveStateRoots() ([]string, error) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return nil, fmt.Errorf("no home directory to find weave state")
	}
	return []string{
		filepath.Join(home, ".bashy", "weave"),
		filepath.Join(home, ".agents", "weave"),
		filepath.Join(home, ".agents", "ycode", "weave"),
	}, nil
}

func wsElem(s string) bool {
	return s != "" && s != "." && s != ".." && !strings.ContainsAny(s, `/\:`) && filepath.Base(s) == s
}

func scopeNoun(kind string) string {
	if kind == "ws" {
		return "workspace"
	}
	return kind
}

// resolveScope resolves an id against <stateRoot>/<tag>/<subdir>/<name>.
func resolveScope(kind, subdir, id string) (string, error) {
	noun := scopeNoun(kind)
	tag, name, tagged := strings.Cut(id, "/")
	if !tagged {
		tag, name = "", id
	}
	if !wsElem(name) || tagged && !wsElem(tag) {
		return "", fmt.Errorf("%s:%s: %s id must be <name> or <queue-tag>/<name>", kind, id, noun)
	}
	roots, err := weaveStateRoots()
	if err != nil {
		return "", fmt.Errorf("%s: %w", kind, err)
	}
	var hits []string
	for _, root := range roots {
		var candidates []string
		if tagged {
			candidates = []string{filepath.Join(root, tag, subdir, name)}
		} else if entries, err := os.ReadDir(root); err == nil {
			for _, e := range entries {
				if e.IsDir() {
					candidates = append(candidates, filepath.Join(root, e.Name(), subdir, name))
				}
			}
		}
		for _, c := range candidates {
			// A target is a real directory; a symlink in its place could
			// point anywhere, so it is not one.
			if st, err := os.Lstat(c); err == nil && st.Mode().IsDir() && (st.Mode()&os.ModeSymlink == 0) {
				hits = append(hits, c)
			}
		}
	}
	sort.Strings(hits)
	switch len(hits) {
	case 0:
		return "", fmt.Errorf("%s:%s: no such weave %s", kind, id, noun)
	case 1:
		return hits[0], nil
	default:
		return "", fmt.Errorf("%s:%s: ambiguous %s, qualify with <queue-tag>/%s: %s", kind, id, noun, name, strings.Join(hits, ", "))
	}
}

// resolveWorkspace maps a ws scope value to the workspace directory.
func resolveWorkspace(id string) (string, error) {
	return resolveScope("ws", "workspaces", id)
}

// resolveSandbox maps a sandbox scope value to the sandbox directory.
func resolveSandbox(id string) (string, error) {
	return resolveScope("sandbox", "sandboxes", id)
}

// scopedDirProvider serves loc's scheme rooted at the resolved directory.
func scopedDirProvider(loc ref.Locator, resolver func(string) (string, error)) (provider.Provider, error) {
	mk, ok := wsProviders[loc.Scheme]
	if !ok {
		return nil, fmt.Errorf("provider %q does not support @%s scope", loc.Scheme, loc.ScopeKind)
	}
	noun := scopeNoun(loc.ScopeKind)
	if len(loc.Inner) != 0 || filepath.IsAbs(loc.Target) || strings.HasPrefix(filepath.ToSlash(loc.Target), "/") || filepath.VolumeName(loc.Target) != "" {
		return nil, fmt.Errorf("@%s scope takes a %s-relative path", loc.ScopeKind, noun)
	}
	root, err := resolver(loc.ScopeValue)
	if err != nil {
		return nil, err
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, err
	}
	return &wsScoped{inner: mk(real), root: real, kind: loc.ScopeKind, value: loc.ScopeValue}, nil
}

// wsProvider serves loc's scheme rooted at the scoped workspace.
func wsProvider(loc ref.Locator) (provider.Provider, error) {
	return scopedDirProvider(loc, resolveWorkspace)
}

// sandboxProvider serves loc's scheme rooted at the scoped sandbox.
func sandboxProvider(loc ref.Locator) (provider.Provider, error) {
	return scopedDirProvider(loc, resolveSandbox)
}

// wsScoped strips the scope before the inner provider sees a locator and
// puts it back on every ref it emits, so a pulled stream names resources in
// the workspace or sandbox rather than on the host.
type wsScoped struct {
	inner       provider.Provider
	root        string
	kind, value string
}

func (w *wsScoped) local(loc ref.Locator) (ref.Locator, error) {
	if loc.ScopeKind != w.kind || loc.ScopeValue != w.value {
		return ref.Locator{}, provider.ErrUnsupported
	}
	loc.ScopeKind, loc.ScopeValue = "", ""
	noun := scopeNoun(w.kind)
	if filepath.IsAbs(loc.Target) || strings.HasPrefix(filepath.ToSlash(loc.Target), "/") || filepath.VolumeName(loc.Target) != "" {
		return ref.Locator{}, fmt.Errorf("@%s scope takes a %s-relative path", w.kind, noun)
	}
	// Lexical escapes and symlinks that leave the workspace or sandbox are refused.
	p := filepath.Join(w.root, filepath.FromSlash(loc.Target))
	real, err := filepath.EvalSymlinks(p)
	if err != nil {
		return ref.Locator{}, err
	}
	if rel, err := filepath.Rel(w.root, real); err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return ref.Locator{}, fmt.Errorf("%s: escapes %s %s", loc.Target, noun, w.value)
	}
	return loc, nil
}

func (w *wsScoped) scoped(s string) string {
	l, err := ref.ParseLocator(s)
	if err != nil {
		return s
	}
	l.ScopeKind, l.ScopeValue = w.kind, w.value
	return l.String()
}

func (w *wsScoped) node(n provider.Node) provider.Node { n.Ref = w.scoped(n.Ref); return n }

func (w *wsScoped) Stat(ctx context.Context, loc ref.Locator) (provider.Node, error) {
	l, err := w.local(loc)
	if err != nil {
		return provider.Node{}, err
	}
	n, err := w.inner.Stat(ctx, l)
	return w.node(n), err
}

func (w *wsScoped) List(ctx context.Context, loc ref.Locator) ([]provider.Node, error) {
	l, err := w.local(loc)
	if err != nil {
		return nil, err
	}
	nodes, err := w.inner.List(ctx, l)
	for i := range nodes {
		nodes[i] = w.node(nodes[i])
	}
	return nodes, err
}

func (w *wsScoped) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	l, err := w.local(loc)
	if err != nil {
		return nil, err
	}
	return w.inner.Open(ctx, l)
}

func (w *wsScoped) Edges(ctx context.Context, loc ref.Locator) ([]provider.Edge, error) {
	l, err := w.local(loc)
	if err != nil {
		return nil, err
	}
	edges, err := w.inner.Edges(ctx, l)
	for i := range edges {
		edges[i].Src, edges[i].Dst = w.scoped(edges[i].Src), w.scoped(edges[i].Dst)
	}
	return edges, err
}

// scopedProvider picks the provider for a locator that carries a scope. Only
// ws and sandbox are served; every other scope is refused rather than read locally.
func scopedProvider(_ *tool.RunContext, loc ref.Locator) (provider.Provider, error) {
	switch loc.ScopeKind {
	case "ws":
		return wsProvider(loc)
	case "sandbox":
		return sandboxProvider(loc)
	default:
		return nil, fmt.Errorf("location scope %q is not served locally", loc.ScopeKind)
	}
}
