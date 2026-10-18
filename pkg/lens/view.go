// Package lens walks providers and renders bounded tree and graph views.
package lens

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/ref"
)

// TreeEntry is a nested node view. Entry remains S3's archive listing row.
type TreeEntry struct {
	Node     provider.Node `json:"node"`
	Children []TreeEntry   `json:"children,omitempty"`
}

// Walk includes root at depth zero; depth < 0 means no limit.
func Walk(ctx context.Context, p provider.Provider, loc ref.Locator, depth int) (TreeEntry, error) {
	n, err := p.Stat(ctx, loc)
	if err != nil {
		return TreeEntry{}, err
	}
	return walk(ctx, p, loc, n, depth, 0)
}

func walk(ctx context.Context, p provider.Provider, loc ref.Locator, n provider.Node, limit, level int) (TreeEntry, error) {
	e := TreeEntry{Node: n}
	if n.Kind != "directory" || limit >= 0 && level >= limit {
		return e, nil
	}
	children, err := p.List(ctx, loc)
	if err != nil {
		return TreeEntry{}, err
	}
	for _, child := range children {
		cl, err := ref.ParseLocator(child.Ref)
		if err != nil {
			return TreeEntry{}, err
		}
		c, err := walk(ctx, p, cl, child, limit, level+1)
		if err != nil {
			return TreeEntry{}, err
		}
		e.Children = append(e.Children, c)
	}
	return e, nil
}

// Render emits a declared view. Edges is requested only for graph views.
func Render(ctx context.Context, out io.Writer, p provider.Provider, loc ref.Locator, view string, depth int, nul bool) error {
	if view == "edges" || view == "dot" || view == "mermaid" {
		edges, err := collectEdges(ctx, p, loc, depth)
		if err != nil {
			return err
		}
		switch view {
		case "edges":
			for _, edge := range edges {
				if err := json.NewEncoder(out).Encode(edge); err != nil {
					return err
				}
			}
		case "dot":
			fmt.Fprintln(out, "digraph G {")
			for _, edge := range edges {
				a, _ := json.Marshal(edge.Src)
				b, _ := json.Marshal(edge.Dst)
				rel, _ := json.Marshal(edge.Rel)
				fmt.Fprintf(out, "  %s -> %s [label=%s];\n", a, b, rel)
			}
			fmt.Fprintln(out, "}")
		case "mermaid":
			fmt.Fprintln(out, "graph TD")
			for _, edge := range edges {
				fmt.Fprintf(out, "  %s -->|%s| %s\n", mermaidID(edge.Src), strings.ReplaceAll(edge.Rel, "|", " "), mermaidID(edge.Dst))
			}
		}
		return nil
	}
	e, err := Walk(ctx, p, loc, depth)
	if err != nil {
		return err
	}
	switch view {
	case "listing":
		return each(e, func(n provider.Node, _ int) error { return json.NewEncoder(out).Encode(n) })
	case "json":
		return json.NewEncoder(out).Encode(e)
	case "paths":
		sep := "\n"
		if nul {
			sep = "\x00"
		}
		return each(e, func(n provider.Node, _ int) error { _, err := io.WriteString(out, n.Ref+sep); return err })
	case "outline":
		return each(e, func(n provider.Node, level int) error {
			_, err := fmt.Fprintf(out, "%s %s\n", strings.Repeat("#", min(level+1, 6)), n.Name)
			return err
		})
	case "tree":
		_, err := fmt.Fprintln(out, e.Node.Name)
		if err != nil {
			return err
		}
		return tree(out, e.Children, "")
	default:
		return fmt.Errorf("lens: unsupported view %q", view)
	}
}

func collectEdges(ctx context.Context, p provider.Provider, loc ref.Locator, depth int) ([]provider.Edge, error) {
	root, err := Walk(ctx, p, loc, depth)
	if err != nil {
		return nil, err
	}
	var edges []provider.Edge
	var visit func(TreeEntry, int) error
	visit = func(e TreeEntry, level int) error {
		if e.Node.Kind == "directory" && depth >= 0 && level >= depth {
			return nil
		}
		l, err := ref.ParseLocator(e.Node.Ref)
		if err != nil {
			return err
		}
		rows, err := p.Edges(ctx, l)
		if err != nil {
			return err
		}
		edges = append(edges, rows...)
		for _, child := range e.Children {
			if child.Node.Kind == "directory" {
				if err := visit(child, level+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(root, 0); err != nil {
		return nil, err
	}
	return edges, nil
}

func each(e TreeEntry, fn func(provider.Node, int) error) error {
	var visit func(TreeEntry, int) error
	visit = func(e TreeEntry, depth int) error {
		if err := fn(e.Node, depth); err != nil {
			return err
		}
		for _, c := range e.Children {
			if err := visit(c, depth+1); err != nil {
				return err
			}
		}
		return nil
	}
	return visit(e, 0)
}

func tree(w io.Writer, children []TreeEntry, prefix string) error {
	for i, c := range children {
		last := i == len(children)-1
		branch, next := "├── ", "│   "
		if last {
			branch, next = "└── ", "    "
		}
		count := ""
		if c.Node.Children != nil {
			count = fmt.Sprintf(" (%d)", *c.Node.Children)
		}
		if _, err := fmt.Fprintf(w, "%s%s%s%s\n", prefix, branch, c.Node.Name, count); err != nil {
			return err
		}
		if err := tree(w, c.Children, prefix+next); err != nil {
			return err
		}
	}
	return nil
}

func mermaidID(s string) string { b, _ := json.Marshal(s); return string(b) }
