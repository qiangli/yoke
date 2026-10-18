package graphcmd

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/qiangli/coreutils/tool"
	gfygraph "github.com/qiangli/gfy/pkg/graph"
	"github.com/qiangli/gfy/pkg/search"
	"github.com/qiangli/yoke/pkg/lens/provider"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
)

// Graph-shaped read verbs (neighbors, impact) emit the bashy-graph-v1 stream
// under --json (Sprint 446 S19), so `graph … --json | lens -t mermaid` works.
// A code node's identity is graph:ROOT!/FILE!/LABEL — label and source file,
// never gfy's node id, which is assigned nondeterministically per build.

const streamProvider = "graph"

func graphLocator(root string, inner ...string) string {
	return ref.Locator{Scheme: "graph", Target: filepath.ToSlash(root), Inner: inner}.String()
}

func relSource(root, file string) string {
	if file == "" {
		return ""
	}
	if filepath.IsAbs(file) {
		if rel, err := filepath.Rel(root, file); err == nil && !strings.HasPrefix(rel, "..") {
			file = rel
		}
	}
	return filepath.ToSlash(file)
}

func codeNodeRef(g *gfygraph.Graph, root, id string) string {
	label := nodeLabel(g, id)
	if file := relSource(root, edgeStr(g.NodeAttrs(id), "source_file")); file != "" && file != label {
		return graphLocator(root, file, label)
	}
	return graphLocator(root, label)
}

func codeNode(g *gfygraph.Graph, root, id string) provider.Node {
	a := g.NodeAttrs(id)
	file := relSource(root, edgeStr(a, "source_file"))
	attrs := map[string]any{}
	for _, k := range []string{"file_type", "source_location"} {
		if v := edgeStr(a, k); v != "" {
			attrs[k] = v
		}
	}
	typ := edgeStr(a, "file_type")
	if typ == "" {
		typ = "unknown"
	}
	return provider.Node{
		SchemaVersion: provider.NodeSchema, Ref: codeNodeRef(g, root, id), Path: file,
		Name: nodeLabel(g, id), Kind: "symbol", Type: typ, Attrs: attrs,
	}
}

// streamBuilder collects one result, keeping the first node per ref (two gfy
// ids can share a label and file) and dropping repeated edges.
type streamBuilder struct {
	g     *gfygraph.Graph
	root  string
	out   stream.Graph
	nodes map[string]bool
	edges map[[3]string]bool
}

func newStreamBuilder(g *gfygraph.Graph, root string) *streamBuilder {
	return &streamBuilder{g: g, root: root, out: stream.Graph{Source: graphLocator(root)},
		nodes: map[string]bool{}, edges: map[[3]string]bool{}}
}

func (b *streamBuilder) node(id string) {
	n := codeNode(b.g, b.root, id)
	if !b.nodes[n.Ref] {
		b.nodes[n.Ref] = true
		b.out.Nodes = append(b.out.Nodes, n)
	}
}

func (b *streamBuilder) edge(src, dst string, attrs map[string]any) {
	rel := edgeStr(attrs, "relation")
	if rel == "" {
		rel = "related"
	}
	e := provider.Edge{SchemaVersion: provider.EdgeSchema, Src: codeNodeRef(b.g, b.root, src), Rel: rel,
		Dst: codeNodeRef(b.g, b.root, dst), Attrs: map[string]any{}}
	if c := edgeStr(attrs, "confidence"); c != "" {
		e.Attrs["confidence"] = c
	}
	k := [3]string{e.Src, e.Rel, e.Dst}
	if !b.edges[k] {
		b.edges[k] = true
		b.out.Edges = append(b.out.Edges, e)
	}
}

func (b *streamBuilder) write(rc *tool.RunContext, name string, query url.Values) int {
	if err := stream.Encode(rc.Out, &b.out, streamProvider, query.Encode()); err != nil {
		fmt.Fprintf(rc.Err, "%s: %v\n", name, err)
		return 1
	}
	return 0
}

// resolveAddresses maps consumer input (`graph impact -`) to node ids: a
// graph: locator selects by label (and source file when it names one), any
// other line is a symbol looked up as on the command line.
func resolveAddresses(g *gfygraph.Graph, root string, addrs []string) ([]string, []string) {
	var ids, missing []string
	seen := map[string]bool{}
	for _, a := range addrs {
		id := ""
		if loc, err := ref.ParseLocator(a); err == nil && loc.Scheme == "graph" && len(loc.Inner) > 0 {
			label := loc.Inner[len(loc.Inner)-1]
			file := ""
			if len(loc.Inner) > 1 {
				file = loc.Inner[len(loc.Inner)-2]
			}
			for _, n := range g.Nodes() {
				if nodeLabel(g, n) == label && (file == "" || relSource(root, edgeStr(g.NodeAttrs(n), "source_file")) == file) {
					id = n
					break
				}
			}
		} else {
			id = search.FindNode(g, a)
		}
		switch {
		case id == "":
			missing = append(missing, a)
		case !seen[id]:
			seen[id] = true
			ids = append(ids, id)
		}
	}
	return ids, missing
}
