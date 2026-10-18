package lens

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

// GraphViews are the TO formats a bashy-graph-v1 stream renders to. They are
// the S11 views, computed from stream records instead of a live walk.
var GraphViews = []string{"listing", "edges", "paths", "tree", "outline", "json", "dot", "mermaid"}

// ViewOptions are the walk/render options a graph row honours.
type ViewOptions struct {
	Depth *int // nil = unbounded; 0 = roots only
	Null  bool // paths: NUL-terminated
}

func graphRow(view string) func([]byte, ViewOptions, bool) ([]byte, error) {
	return func(data []byte, o ViewOptions, declared bool) ([]byte, error) {
		g, err := stream.Decode(bytes.NewReader(data), stream.DecodeOptions{Declared: declared})
		if err != nil {
			return nil, err
		}
		var b bytes.Buffer
		err = RenderGraph(&b, g, view, o)
		return b.Bytes(), err
	}
}

// PullGraph walks loc through p and returns the stream a producer of the same
// snapshot and options emits. Containers are Walk's directories; edges come
// from every container the bounded walk expanded.
func PullGraph(ctx context.Context, p provider.Provider, loc ref.Locator, depth int) (*stream.Graph, error) {
	root, err := Walk(ctx, p, loc, depth)
	if err != nil {
		return nil, err
	}
	g := &stream.Graph{Source: loc.String()}
	var visit func(TreeEntry, int) error
	visit = func(e TreeEntry, level int) error {
		g.Nodes = append(g.Nodes, e.Node)
		if e.Node.Kind == "directory" && (depth < 0 || level < depth) {
			l, err := ref.ParseLocator(e.Node.Ref)
			if err != nil {
				return err
			}
			rows, err := p.Edges(ctx, l)
			if err != nil && !errors.Is(err, provider.ErrUnsupported) {
				return err
			}
			g.Edges = append(g.Edges, rows...)
		}
		for _, c := range e.Children {
			if err := visit(c, level+1); err != nil {
				return err
			}
		}
		return nil
	}
	return g, visit(root, 0)
}

// PullQuery is the normalized query a pull binds its cursor to.
func PullQuery(depth int) string { return "depth=" + strconv.Itoa(depth) }

// RenderGraph renders decoded stream records. Containment is the "contains"
// relation between two nodes present in the stream; roots are the nodes no
// such edge points at, in stream order, and siblings are in name order. Other relations appear only in the
// edge views (edges, dot, mermaid).
func RenderGraph(out io.Writer, g *stream.Graph, view string, o ViewOptions) error {
	limit := -1
	if o.Depth != nil {
		limit = *o.Depth
	}
	enc := json.NewEncoder(out)
	enc.SetEscapeHTML(false)
	switch view {
	case "edges":
		for _, e := range g.Edges {
			if err := enc.Encode(e); err != nil {
				return err
			}
		}
		return nil
	case "dot":
		fmt.Fprintln(out, "digraph G {")
		for _, e := range g.Edges {
			a, _ := json.Marshal(e.Src)
			b, _ := json.Marshal(e.Dst)
			rel, _ := json.Marshal(e.Rel)
			fmt.Fprintf(out, "  %s -> %s [label=%s];\n", a, b, rel)
		}
		_, err := fmt.Fprintln(out, "}")
		return err
	case "mermaid":
		fmt.Fprintln(out, "graph TD")
		for _, e := range g.Edges {
			if _, err := fmt.Fprintf(out, "  %s -->|%s| %s\n", mermaidID(e.Src), strings.ReplaceAll(e.Rel, "|", " "), mermaidID(e.Dst)); err != nil {
				return err
			}
		}
		return nil
	}
	roots := containment(g, limit)
	switch view {
	case "listing":
		return eachRoot(roots, func(n provider.Node, _ int) error { return enc.Encode(n) })
	case "paths":
		sep := "\n"
		if o.Null {
			sep = "\x00"
		}
		return eachRoot(roots, func(n provider.Node, _ int) error { _, err := io.WriteString(out, n.Ref+sep); return err })
	case "outline":
		return eachRoot(roots, func(n provider.Node, level int) error {
			_, err := fmt.Fprintf(out, "%s %s\n", strings.Repeat("#", min(level+1, 6)), n.Name)
			return err
		})
	case "json":
		if roots == nil {
			roots = []TreeEntry{}
		}
		return enc.Encode(roots)
	case "tree":
		for _, r := range roots {
			if _, err := fmt.Fprintln(out, r.Node.Name); err != nil {
				return err
			}
			if err := tree(out, r.Children, ""); err != nil {
				return err
			}
		}
		return nil
	}
	return fmt.Errorf("lens: unsupported view %q", view)
}

func containment(g *stream.Graph, limit int) []TreeEntry {
	byRef := make(map[string]provider.Node, len(g.Nodes))
	for _, n := range g.Nodes {
		byRef[n.Ref] = n
	}
	kids := map[string][]string{}
	contained := map[string]bool{}
	for _, e := range g.Edges {
		if e.Rel != "contains" || e.Src == e.Dst {
			continue
		}
		if _, ok := byRef[e.Src]; !ok {
			continue
		}
		if _, ok := byRef[e.Dst]; !ok {
			continue
		}
		kids[e.Src] = append(kids[e.Src], e.Dst)
		contained[e.Dst] = true
	}
	// Siblings read in name order, as S11's walk lists them; ref breaks ties.
	for _, ks := range kids {
		sort.SliceStable(ks, func(i, j int) bool {
			a, b := byRef[ks[i]], byRef[ks[j]]
			if a.Name != b.Name {
				return a.Name < b.Name
			}
			return a.Ref < b.Ref
		})
	}
	onPath := map[string]bool{}
	var build func(ref string, level int) TreeEntry
	build = func(ref string, level int) TreeEntry {
		e := TreeEntry{Node: byRef[ref]}
		if limit >= 0 && level >= limit {
			return e
		}
		onPath[ref] = true
		for _, k := range kids[ref] {
			if !onPath[k] {
				e.Children = append(e.Children, build(k, level+1))
			}
		}
		delete(onPath, ref)
		return e
	}
	var roots []TreeEntry
	for _, n := range g.Nodes {
		if !contained[n.Ref] {
			roots = append(roots, build(n.Ref, 0))
		}
	}
	return roots
}

func eachRoot(roots []TreeEntry, fn func(provider.Node, int) error) error {
	for _, r := range roots {
		if err := each(r, fn); err != nil {
			return err
		}
	}
	return nil
}
