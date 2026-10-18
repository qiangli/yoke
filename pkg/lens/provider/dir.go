package provider

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"time"

	"github.com/qiangli/coreutils/pkg/ignore"
	"github.com/qiangli/yoke/pkg/ref"
)

// Dir exposes an io/fs tree. Root also anchors optional agentic ignore rules.
type Dir struct {
	FS      fs.FS
	Root    string
	Agentic bool
	matcher *ignore.Matcher
}

func NewDir(root string, agentic bool) *Dir {
	if root == "" {
		root = "."
	}
	d := &Dir{FS: os.DirFS(root), Root: root, Agentic: agentic}
	if agentic {
		d.matcher = ignore.New(root)
	}
	return d
}

func (d *Dir) fsPath(loc ref.Locator) (string, error) {
	if loc.Scheme != "dir" || len(loc.Inner) != 0 || loc.ScopeKind != "" {
		return "", ErrUnsupported
	}
	p := filepath.ToSlash(loc.Target)
	if p == "" || p == "." {
		return ".", nil
	}
	if path.IsAbs(p) {
		rel, err := filepath.Rel(d.Root, loc.Target)
		if err != nil {
			return "", err
		}
		p = filepath.ToSlash(rel)
	}
	if !fs.ValidPath(p) {
		return "", fmt.Errorf("provider: invalid directory path %q", p)
	}
	return p, nil
}

func (d *Dir) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	if err := ctx.Err(); err != nil {
		return Node{}, err
	}
	p, err := d.fsPath(loc)
	if err != nil {
		return Node{}, err
	}
	info, err := fs.Stat(d.FS, p)
	if err != nil {
		return Node{}, err
	}
	return d.node(ctx, loc, p, info)
}

func (d *Dir) node(ctx context.Context, loc ref.Locator, p string, info fs.FileInfo) (Node, error) {
	kind, typ := "file", "application/octet-stream"
	if info.IsDir() {
		kind, typ = "directory", "inode/directory"
	}
	size := info.Size()
	n := Node{SchemaVersion: NodeSchema, Ref: loc.String(), Path: p, Name: info.Name(), Kind: kind, Type: typ, Size: &size, Mode: info.Mode().String(), Mtime: info.ModTime().UTC().Format(time.RFC3339Nano), Attrs: map[string]any{}}
	if info.IsDir() {
		entries, err := d.visible(ctx, p)
		if err != nil {
			return Node{}, err
		}
		count := uint64(len(entries))
		n.Children = &count
	}
	return n, nil
}

func (d *Dir) visible(ctx context.Context, p string) ([]fs.DirEntry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := fs.ReadDir(d.FS, p)
	if err != nil {
		return nil, err
	}
	var out []fs.DirEntry
	for _, e := range entries {
		if d.Agentic && d.matcher.Skip(filepath.Join(d.Root, filepath.FromSlash(path.Join(p, e.Name()))), e.IsDir()) {
			continue
		}
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

func (d *Dir) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	p, err := d.fsPath(loc)
	if err != nil {
		return nil, err
	}
	entries, err := d.visible(ctx, p)
	if err != nil {
		return nil, err
	}
	out := make([]Node, 0, len(entries))
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		child := path.Join(p, e.Name())
		info, err := e.Info()
		if err != nil {
			return nil, err
		}
		cl := loc
		cl.Target = filepath.Join(loc.Target, e.Name())
		if loc.Target == "." || loc.Target == "" {
			cl.Target = e.Name()
		}
		n, err := d.node(ctx, cl, child, info)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, nil
}

func (d *Dir) Open(ctx context.Context, loc ref.Locator) (io.ReadCloser, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p, err := d.fsPath(loc)
	if err != nil {
		return nil, err
	}
	f, err := d.FS.Open(p)
	if err != nil {
		return nil, err
	}
	return f, nil
}

func (d *Dir) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	children, err := d.List(ctx, loc)
	if err != nil {
		return nil, err
	}
	edges := make([]Edge, 0, len(children))
	for _, child := range children {
		edges = append(edges, Edge{
			SchemaVersion: EdgeSchema, Src: loc.String(), Rel: "contains",
			Dst: child.Ref, Attrs: map[string]any{},
		})
	}
	return edges, nil
}

// Hidden reports paths skipped by the agentic noise rules.
func (d *Dir) Hidden() int {
	if d.matcher == nil {
		return 0
	}
	return d.matcher.Hidden()
}
