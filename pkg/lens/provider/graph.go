// Graph exposes a cached pkg/codegraph snapshot as a read-only tree:
//
//	graph:.                   project node (the whole snapshot)
//	graph:.!/<file>           file node (symbols sharing one source file)
//	graph:.!/<file>!/<label>  symbol node
//	graph:.!/<label>           symbol node with no source file
//
// The provider never builds or writes: it reads the on-disk cache written
// by `graph build` (pkg/codegraph.Load) and yields bashy-node-v1 and
// bashy-edge-v1 rows. A missing cache is an explicit error naming the cache
// path and the build command, never an empty success. A set Snapshot
// overrides the on-disk cache; it exists for tests.
package provider

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gfygraph "github.com/qiangli/gfy/pkg/graph"
	"github.com/qiangli/yoke/pkg/codegraph"
	"github.com/qiangli/yoke/pkg/ref"
)

var _ Provider = (*Graph)(nil)

// Graph is a read-only Provider over a codegraph snapshot of one project.
type Graph struct {
	Root string // project root; relative locator targets resolve here
	// Snapshot overrides the on-disk cache when non-nil (tests).
	Snapshot *gfygraph.Graph
}

// NewGraph returns a graph: provider reading the project under root.
func NewGraph(root string) *Graph { return &Graph{Root: root} }

// graphProject is the canonical locator target: the provider's own root.
const graphProject = "."

func graphRef(inner ...string) string {
	return ref.Locator{Scheme: "graph", Target: graphProject, Inner: inner}.String()
}

// graphCheck rejects what this provider cannot serve before any snapshot is
// read. Only the project target is addressable; location scopes need
// transports v1 does not have.
func graphCheck(loc ref.Locator) error {
	if loc.Scheme != "graph" {
		return ErrUnsupported
	}
	if loc.ScopeKind != "" {
		return fmt.Errorf("provider: graph: location scopes are not supported: %w", ErrUnsupported)
	}
	rel := filepath.ToSlash(filepath.Clean("/" + loc.Target))
	if rel != "/"+graphProject && rel != "/" {
		return fmt.Errorf("provider: graph: only the project %q is addressable, not %q", graphProject, loc.Target)
	}
	if len(loc.Inner) > 2 {
		return fmt.Errorf("provider: graph: address at most file and symbol (!/<file>!/<label>): %w", ErrUnsupported)
	}
	return nil
}

// graphAttrStr reads an optional string attribute without mutating the node.
func graphAttrStr(attrs map[string]any, key string) string {
	if v, ok := attrs[key].(string); ok {
		return v
	}
	return ""
}

// graphRoot returns the absolute project root the snapshot paths relativize
// against.
func (g *Graph) graphRoot() string {
	root := g.Root
	if root == "" {
		root = "."
	}
	if abs, err := filepath.Abs(root); err == nil {
		return abs
	}
	return root
}

// graphRelFile relativizes a codegraph source_file against the project root,
// mirroring cmds/graph: absolute paths under the root become slash-relative,
// anything else passes through unchanged.
func graphRelFile(root, file string) string {
	if file == "" {
		return ""
	}
	if filepath.IsAbs(file) {
		if rel, err := filepath.Rel(root, file); err == nil && rel != ".." && !strings.HasPrefix(rel, "../") {
			file = rel
		}
	}
	return filepath.ToSlash(file)
}

// graphSymbolRef is the code identity: file and label, never gfy's per-build
// node id.
func graphSymbolRef(file, label string) string {
	if file == "" {
		return graphRef(label)
	}
	return graphRef(file, label)
}

// graphIndex is a read-only, deterministically ordered view of one snapshot.
type graphIndex struct {
	snap     *gfygraph.Graph
	root     string
	files    []string            // sorted source files (slash-relative, non-empty)
	byFile   map[string][]string // file -> node ids, in stable ref order
	external []string            // node ids with no source file, in stable ref order
	refOf    map[string]string   // node id -> symbol ref
	edges    []gfygraph.EdgeData // as stored (already source/target sorted)
}

func indexGraph(snap *gfygraph.Graph, root string) *graphIndex {
	idx := &graphIndex{
		snap:   snap,
		root:   root,
		byFile: map[string][]string{},
		refOf:  map[string]string{},
	}
	seenRef := map[string]bool{}
	for _, id := range snap.Nodes() {
		file := graphRelFile(root, graphAttrStr(snap.NodeAttrs(id), "source_file"))
		label := graphAttrStr(snap.NodeAttrs(id), "label")
		if label == "" {
			label = id
		}
		r := graphSymbolRef(file, label)
		idx.refOf[id] = r
		// Two gfy ids can share one label and file; the ref keeps the
		// first in node-id order and the duplicate stays unreachable.
		if seenRef[r] {
			continue
		}
		seenRef[r] = true
		if file == "" {
			idx.external = append(idx.external, id)
		} else {
			idx.byFile[file] = append(idx.byFile[file], id)
		}
	}
	byRef := func(a, b string) bool { return idx.refOf[a] < idx.refOf[b] }
	for file, ids := range idx.byFile {
		sort.SliceStable(ids, func(i, j int) bool { return byRef(ids[i], ids[j]) })
		idx.byFile[file] = ids
	}
	sort.SliceStable(idx.external, func(i, j int) bool { return byRef(idx.external[i], idx.external[j]) })
	for file := range idx.byFile {
		idx.files = append(idx.files, file)
	}
	sort.Strings(idx.files)
	idx.edges = snap.Edges()
	return idx
}

// load checks the context and locator, then returns the snapshot index
// without mutating anything on disk: the test override when set, else the
// cache `graph build` wrote. A missing cache wraps os.ErrNotExist with the
// path and the command that creates it.
func (g *Graph) load(ctx context.Context, loc ref.Locator) (*graphIndex, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := graphCheck(loc); err != nil {
		return nil, err
	}
	if g.Snapshot != nil {
		return indexGraph(g.Snapshot, g.graphRoot()), nil
	}
	root := g.Root
	if root == "" {
		root = "."
	}
	gc, err := codegraph.Load(codegraph.CachePath(root))
	if err != nil {
		return nil, err
	}
	if gc == nil || gc.Graph == nil {
		return nil, fmt.Errorf("provider: graph: no cached code graph at %s (run `graph build` to create it): %w", codegraph.CachePath(root), os.ErrNotExist)
	}
	return indexGraph(gc.Graph, g.graphRoot()), nil
}

// graphProjectNode shapes the snapshot root. Children counts files plus
// file-less symbols, exactly the rows List returns.
func graphProjectNode(idx *graphIndex) Node {
	kids := uint64(len(idx.files) + len(idx.external))
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           graphRef(),
		Path:          graphProject,
		Name:          filepath.Base(idx.root),
		Kind:          "directory",
		Type:          "graph/project",
		Children:      &kids,
		Attrs: map[string]any{
			"nodes": idx.snap.NodeCount(),
			"edges": idx.snap.EdgeCount(),
		},
	}
}

func graphFileNode(file string, n int) Node {
	count := uint64(n)
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           graphRef(file),
		Path:          file,
		Name:          filepath.Base(file),
		Kind:          "file",
		Type:          "graph/file",
		Children:      &count,
		Attrs:         map[string]any{},
	}
}

// graphSymbolNode shapes one code node. Type mirrors cmds/graph: the node's
// file_type, defaulting to unknown.
func graphSymbolNode(idx *graphIndex, id string) Node {
	a := idx.snap.NodeAttrs(id)
	label := graphAttrStr(a, "label")
	if label == "" {
		label = id
	}
	file := graphRelFile(idx.root, graphAttrStr(a, "source_file"))
	typ := graphAttrStr(a, "file_type")
	if typ == "" {
		typ = "unknown"
	}
	attrs := map[string]any{}
	for _, k := range []string{"file_type", "source_location"} {
		if v := graphAttrStr(a, k); v != "" {
			attrs[k] = v
		}
	}
	path := label
	if file != "" {
		path = file + "!/" + label
	}
	return Node{
		SchemaVersion: NodeSchema,
		Ref:           idx.refOf[id],
		Path:          path,
		Name:          label,
		Kind:          "symbol",
		Type:          typ,
		Attrs:         attrs,
	}
}

// graphCodeEdge maps one snapshot edge onto addressed symbol refs. Relations
// default to "related", matching cmds/graph.
func graphCodeEdge(idx *graphIndex, e gfygraph.EdgeData) (Edge, bool) {
	src, ok := idx.refOf[e.Source]
	if !ok {
		return Edge{}, false
	}
	dst, ok := idx.refOf[e.Target]
	if !ok {
		return Edge{}, false
	}
	rel := graphAttrStr(e.Attrs, "relation")
	if rel == "" {
		rel = "related"
	}
	edge := Edge{
		SchemaVersion: EdgeSchema,
		Src:           src,
		Rel:           rel,
		Dst:           dst,
		Attrs:         map[string]any{},
	}
	if c := graphAttrStr(e.Attrs, "confidence"); c != "" {
		edge.Attrs["confidence"] = c
	}
	return edge, true
}

func sortGraphEdges(edges []Edge) {
	sort.SliceStable(edges, func(i, j int) bool {
		if edges[i].Src != edges[j].Src {
			return edges[i].Src < edges[j].Src
		}
		if edges[i].Rel != edges[j].Rel {
			return edges[i].Rel < edges[j].Rel
		}
		return edges[i].Dst < edges[j].Dst
	})
}

// isFile reports whether inner names a file of the snapshot.
func (idx *graphIndex) isFile(inner []string) bool {
	return len(inner) == 1 && len(idx.byFile[inner[0]]) > 0
}

// resolveSymbol maps an addressed file/label back to its snapshot node id. A
// single inner segment names either a file or a file-less symbol label.
func (idx *graphIndex) resolveSymbol(inner []string) (string, error) {
	switch len(inner) {
	case 1:
		if idx.isFile(inner) {
			return "", fmt.Errorf("provider: graph: %q is a file, not a symbol", inner[0])
		}
		for _, id := range idx.external {
			if idx.refOf[id] == graphRef(inner[0]) {
				return id, nil
			}
		}
		return "", fmt.Errorf("provider: graph: no file or symbol %q: %w", inner[0], os.ErrNotExist)
	case 2:
		ids, ok := idx.byFile[inner[0]]
		if !ok {
			return "", fmt.Errorf("provider: graph: no file %q: %w", inner[0], os.ErrNotExist)
		}
		want := graphRef(inner[0], inner[1])
		for _, id := range ids {
			if idx.refOf[id] == want {
				return id, nil
			}
		}
		return "", fmt.Errorf("provider: graph: no symbol %q in %s: %w", inner[1], inner[0], os.ErrNotExist)
	default:
		return "", fmt.Errorf("provider: graph: address at most file and symbol (!/<file>!/<label>): %w", ErrUnsupported)
	}
}

// Stat returns the project, file or symbol node.
func (g *Graph) Stat(ctx context.Context, loc ref.Locator) (Node, error) {
	idx, err := g.load(ctx, loc)
	if err != nil {
		return Node{}, err
	}
	switch len(loc.Inner) {
	case 0:
		return graphProjectNode(idx), nil
	case 1:
		if idx.isFile(loc.Inner) {
			return graphFileNode(loc.Inner[0], len(idx.byFile[loc.Inner[0]])), nil
		}
		id, err := idx.resolveSymbol(loc.Inner)
		if err != nil {
			return Node{}, err
		}
		return graphSymbolNode(idx, id), nil
	default:
		id, err := idx.resolveSymbol(loc.Inner)
		if err != nil {
			return Node{}, err
		}
		return graphSymbolNode(idx, id), nil
	}
}

// List returns the project's files and file-less symbols, or one file's
// symbols in stable ref order. Symbols are leaves.
func (g *Graph) List(ctx context.Context, loc ref.Locator) ([]Node, error) {
	idx, err := g.load(ctx, loc)
	if err != nil {
		return nil, err
	}
	switch len(loc.Inner) {
	case 0:
		var nodes []Node
		for _, file := range idx.files {
			nodes = append(nodes, graphFileNode(file, len(idx.byFile[file])))
		}
		for _, id := range idx.external {
			nodes = append(nodes, graphSymbolNode(idx, id))
		}
		sort.SliceStable(nodes, func(i, j int) bool { return nodes[i].Ref < nodes[j].Ref })
		if nodes == nil {
			nodes = []Node{}
		}
		return nodes, nil
	case 1:
		ids, ok := idx.byFile[loc.Inner[0]]
		if !ok {
			if _, serr := idx.resolveSymbol(loc.Inner); serr != nil {
				return nil, serr
			}
			return nil, ErrUnsupported
		}
		nodes := make([]Node, 0, len(ids))
		for _, id := range ids {
			nodes = append(nodes, graphSymbolNode(idx, id))
		}
		return nodes, nil
	default:
		return nil, ErrUnsupported
	}
}

// Edges returns the full snapshot from the project (contains closure plus
// every code relation, so `-t graph` carries the whole stream), one file's
// contains edges, or one symbol's incident relations with its defined_in edge.
func (g *Graph) Edges(ctx context.Context, loc ref.Locator) ([]Edge, error) {
	idx, err := g.load(ctx, loc)
	if err != nil {
		return nil, err
	}
	switch len(loc.Inner) {
	case 0:
		var edges []Edge
		for _, file := range idx.files {
			edges = append(edges, Edge{
				SchemaVersion: EdgeSchema,
				Src:           graphRef(),
				Rel:           "contains",
				Dst:           graphRef(file),
				Attrs:         map[string]any{},
			})
			for _, id := range idx.byFile[file] {
				edges = append(edges, Edge{
					SchemaVersion: EdgeSchema,
					Src:           graphRef(file),
					Rel:           "contains",
					Dst:           idx.refOf[id],
					Attrs:         map[string]any{},
				})
			}
		}
		for _, id := range idx.external {
			edges = append(edges, Edge{
				SchemaVersion: EdgeSchema,
				Src:           graphRef(),
				Rel:           "contains",
				Dst:           idx.refOf[id],
				Attrs:         map[string]any{},
			})
		}
		for _, e := range idx.edges {
			if mapped, ok := graphCodeEdge(idx, e); ok {
				edges = append(edges, mapped)
			}
		}
		sortGraphEdges(edges)
		if edges == nil {
			edges = []Edge{}
		}
		return edges, nil
	case 1:
		ids, ok := idx.byFile[loc.Inner[0]]
		if !ok {
			if _, serr := idx.resolveSymbol(loc.Inner); serr != nil {
				return nil, serr
			}
			return idx.symbolEdges(loc.Inner)
		}
		edges := make([]Edge, 0, len(ids))
		for _, id := range ids {
			edges = append(edges, Edge{
				SchemaVersion: EdgeSchema,
				Src:           graphRef(loc.Inner[0]),
				Rel:           "contains",
				Dst:           idx.refOf[id],
				Attrs:         map[string]any{},
			})
		}
		return edges, nil
	default:
		return idx.symbolEdges(loc.Inner)
	}
}

// symbolEdges maps every snapshot edge touching the addressed symbol, plus
// its defined_in edge to the owning file.
func (idx *graphIndex) symbolEdges(inner []string) ([]Edge, error) {
	id, err := idx.resolveSymbol(inner)
	if err != nil {
		return nil, err
	}
	var edges []Edge
	for _, e := range idx.edges {
		if e.Source != id && e.Target != id {
			continue
		}
		if mapped, ok := graphCodeEdge(idx, e); ok {
			edges = append(edges, mapped)
		}
	}
	if len(inner) == 2 {
		edges = append(edges, Edge{
			SchemaVersion: EdgeSchema,
			Src:           idx.refOf[id],
			Rel:           "defined_in",
			Dst:           graphRef(inner[0]),
			Attrs:         map[string]any{},
		})
	}
	sortGraphEdges(edges)
	if edges == nil {
		edges = []Edge{}
	}
	return edges, nil
}

func (g *Graph) Open(context.Context, ref.Locator) (io.ReadCloser, error) {
	return nil, ErrUnsupported
}
