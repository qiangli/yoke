package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	_ "unsafe"

	"github.com/qiangli/yoke/pkg/ref"
)

//go:linkname refLocatorSchemes github.com/qiangli/yoke/pkg/ref.locatorSchemes
var refLocatorSchemes map[string]bool

func init() {
	if refLocatorSchemes != nil {
		refLocatorSchemes["env"] = true
	}
}

var _ Provider = (*Env)(nil)

// Env exposes environment variable names and presence as a read-only hierarchical tree.
// Values are never retained or printed.
type Env struct {
	Environ func() []string
}

// NewEnv returns a new read-only Env provider backed by os.Environ.
func NewEnv() *Env {
	return &Env{Environ: os.Environ}
}

// NewEnvWithEnviron returns a new Env provider backed by a static environment slice.
func NewEnvWithEnviron(environ []string) *Env {
	return &Env{
		Environ: func() []string {
			return environ
		},
	}
}

func (e *Env) fetch() []string {
	if e != nil && e.Environ != nil {
		return e.Environ()
	}
	return os.Environ()
}

func (e *Env) parseLocator(loc ref.Locator) (name string, isRoot bool, err error) {
	if loc.Scheme != "env" || len(loc.Inner) != 0 || loc.ScopeKind != "" {
		return "", false, ErrUnsupported
	}
	t := strings.TrimPrefix(loc.Target, "/")
	t = strings.TrimPrefix(t, "./")
	if t == "" || t == "." {
		return "", true, nil
	}
	if strings.Contains(t, "/") {
		return "", false, ErrUnsupported
	}
	return t, false, nil
}

func (e *Env) entries() (map[string]bool, []string) {
	raw := e.fetch()
	seen := make(map[string]bool)
	var names []string
	for _, kv := range raw {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if !seen[k] {
			seen[k] = true
			names = append(names, k)
		}
	}
	sort.Strings(names)
	return seen, names
}

func (e *Env) rootNode(loc ref.Locator, count uint64) Node {
	refStr := loc.String()
	if refStr == "" || refStr == "env:." {
		refStr = "env:"
	}
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           refStr,
		Path:          "",
		Name:          "env",
		Kind:          "directory",
		Type:          "env/tree",
		Children:      &count,
		Attrs:         map[string]any{},
	}
}

func (e *Env) variableNode(name string) Node {
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           "env:" + name,
		Path:          name,
		Name:          name,
		Kind:          "variable",
		Type:          "variable",
		Attrs: map[string]any{
			"name":    name,
			"present": true,
		},
	}
}

// Stat returns metadata for either the root environment container (env:) or a specific variable (env:<name>).
func (e *Env) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	name, isRoot, err := e.parseLocator(loc)
	if err != nil {
		return Node{}, err
	}
	seen, names := e.entries()
	if isRoot {
		return e.rootNode(loc, uint64(len(names))), nil
	}
	if !seen[name] {
		return Node{}, fmt.Errorf("provider: environment variable %q: %w", name, os.ErrNotExist)
	}
	return e.variableNode(name), nil
}

// List returns child nodes of either the root (env:) or a specific variable (env:<name>).
func (e *Env) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	name, isRoot, err := e.parseLocator(loc)
	if err != nil {
		return nil, err
	}
	seen, names := e.entries()
	if isRoot {
		nodes := make([]Node, 0, len(names))
		for _, n := range names {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			nodes = append(nodes, e.variableNode(n))
		}
		return nodes, nil
	}
	if !seen[name] {
		return nil, fmt.Errorf("provider: environment variable %q: %w", name, os.ErrNotExist)
	}
	return []Node{}, nil
}

// Edges returns contains edges from loc to each of its direct children.
func (e *Env) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	children, err := e.List(ctx, loc)
	if err != nil {
		return nil, err
	}
	src := loc.String()
	if src == "" || src == "env:." {
		src = "env:"
	}
	edges := make([]Edge, 0, len(children))
	for _, child := range children {
		edges = append(edges, Edge{
			SchemaVersion: EdgeSchema,
			Src:           src,
			Rel:           "contains",
			Dst:           child.Ref,
			Attrs:         map[string]any{},
		})
	}
	return edges, nil
}

// Open is unsupported for environment variables.
func (e *Env) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}
