package provider

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	gfygraph "github.com/qiangli/gfy/pkg/graph"
	"github.com/qiangli/yoke/pkg/ref"
)

func newTestGraph(t *testing.T) *Graph {
	t.Helper()
	g := gfygraph.New(true)
	addNode := func(id, label, file, typ, loc string) {
		g.AddNode(id, map[string]any{
			"label": label, "file_type": typ,
			"source_file": file, "source_location": loc,
		})
	}
	addNode("n1", "Beta", "b.go", "code", "1")
	addNode("n2", "Alpha", "a.go", "code", "3")
	addNode("n3", "Run", "a.go", "code", "7")
	addNode("n4", "Base", "", "code", "")
	g.AddEdge("n2", "n3", map[string]any{"relation": "calls", "confidence": "EXTRACTED"})
	g.AddEdge("n3", "n4", map[string]any{"relation": "imports", "confidence": "INFERRED"})
	g.AddEdge("n1", "n2", map[string]any{"relation": "calls", "confidence": "EXTRACTED"})
	return &Graph{Root: t.TempDir(), Snapshot: g}
}

func graphLoc(t *testing.T, s string) ref.Locator {
	t.Helper()
	l, err := ref.ParseLocator(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestGraphStatProjectAndList(t *testing.T) {
	g := newTestGraph(t)
	ctx := context.Background()
	n, err := g.Stat(ctx, graphLoc(t, "graph:."))
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "directory" || n.Type != "graph/project" || n.Ref != "graph:." ||
		n.SchemaVersion != NodeSchema || n.Path != "." {
		t.Fatalf("project node: %+v", n)
	}
	nodes, err := g.List(ctx, graphLoc(t, "graph:."))
	if err != nil {
		t.Fatal(err)
	}
	if n.Children == nil || *n.Children != uint64(len(nodes)) {
		t.Fatalf("children mismatch: %+v nodes=%d", n.Children, len(nodes))
	}
	var refs []string
	for _, c := range nodes {
		if c.SchemaVersion != NodeSchema {
			t.Fatalf("bad schema: %+v", c)
		}
		refs = append(refs, c.Ref)
	}
	want := []string{"graph:.!/Base", "graph:.!/a.go", "graph:.!/b.go"}
	if !reflect.DeepEqual(refs, want) {
		t.Fatalf("project children: got %v want %v", refs, want)
	}
	// Stable across calls.
	again, err := g.List(ctx, graphLoc(t, "graph:."))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(nodes, again) {
		t.Fatalf("unstable list:\n%v\n%v", nodes, again)
	}
}

func TestGraphFileAndSymbol(t *testing.T) {
	g := newTestGraph(t)
	ctx := context.Background()
	f, err := g.Stat(ctx, graphLoc(t, "graph:.!/a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Kind != "file" || f.Path != "a.go" || f.Ref != "graph:.!/a.go" {
		t.Fatalf("file node: %+v", f)
	}
	syms, err := g.List(ctx, graphLoc(t, "graph:.!/a.go"))
	if err != nil {
		t.Fatal(err)
	}
	if f.Children == nil || *f.Children != uint64(len(syms)) || len(syms) != 2 {
		t.Fatalf("file children: %+v syms=%d", f.Children, len(syms))
	}
	if syms[0].Ref != "graph:.!/a.go!/Alpha" || syms[1].Ref != "graph:.!/a.go!/Run" {
		t.Fatalf("symbol order: %+v", syms)
	}
	s, err := g.Stat(ctx, graphLoc(t, "graph:.!/a.go!/Alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Kind != "symbol" || s.Name != "Alpha" || s.Path != "a.go!/Alpha" ||
		s.Type != "code" || s.Attrs["source_location"] != "3" {
		t.Fatalf("symbol node: %+v", s)
	}
	ext, err := g.Stat(ctx, graphLoc(t, "graph:.!/Base"))
	if err != nil {
		t.Fatal(err)
	}
	if ext.Kind != "symbol" || ext.Name != "Base" || ext.Path != "Base" {
		t.Fatalf("external symbol: %+v", ext)
	}
}

func TestGraphEdges(t *testing.T) {
	g := newTestGraph(t)
	ctx := context.Background()
	edges, err := g.Edges(ctx, graphLoc(t, "graph:."))
	if err != nil {
		t.Fatal(err)
	}
	type key struct{ src, rel, dst string }
	seen := map[key]bool{}
	prev := key{}
	for _, e := range edges {
		if e.SchemaVersion != EdgeSchema {
			t.Fatalf("bad edge schema: %+v", e)
		}
		k := key{e.Src, e.Rel, e.Dst}
		if seen[k] {
			t.Fatalf("duplicate edge: %+v", e)
		}
		seen[k] = true
		if (key{}) != prev && !(prev.src < k.src ||
			(prev.src == k.src && prev.rel < k.rel) ||
			(prev.src == k.src && prev.rel == k.rel && prev.dst <= k.dst)) {
			t.Fatalf("edges not stably ordered at %+v after %+v", k, prev)
		}
		prev = k
	}
	for _, want := range []key{
		{"graph:.", "contains", "graph:.!/a.go"},
		{"graph:.", "contains", "graph:.!/Base"},
		{"graph:.!/a.go", "contains", "graph:.!/a.go!/Alpha"},
		{"graph:.!/a.go!/Alpha", "calls", "graph:.!/a.go!/Run"},
	} {
		if !seen[want] {
			t.Fatalf("missing edge %+v in %+v", want, edges)
		}
	}
	// Deterministic across calls.
	again, err := g.Edges(ctx, graphLoc(t, "graph:."))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(edges, again) {
		t.Fatal("unstable edges")
	}
	// Symbol edges: incident relations plus the defined_in edge home.
	sym, err := g.Edges(ctx, graphLoc(t, "graph:.!/a.go!/Alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if len(sym) != 3 {
		t.Fatalf("symbol edges: %+v", sym)
	}
	if sym[1].Rel != "defined_in" || sym[1].Src != "graph:.!/a.go!/Alpha" || sym[1].Dst != "graph:.!/a.go" {
		t.Fatalf("defined_in edge: %+v", sym)
	}
	file, err := g.Edges(ctx, graphLoc(t, "graph:.!/a.go"))
	if err != nil || len(file) != 2 || file[0].Rel != "contains" {
		t.Fatalf("file edges: %+v %v", file, err)
	}
}

func TestGraphNoMutation(t *testing.T) {
	g := newTestGraph(t)
	ctx := context.Background()
	before, _ := json.Marshal(g.Snapshot)
	for _, loc := range []string{
		"graph:.", "graph:.!/a.go", "graph:.!/a.go!/Alpha", "graph:.!/Base",
	} {
		if _, err := g.Stat(ctx, graphLoc(t, loc)); err != nil {
			t.Fatal(err)
		}
		if _, err := g.List(ctx, graphLoc(t, "graph:.")); err != nil {
			t.Fatal(err)
		}
		if _, err := g.Edges(ctx, graphLoc(t, loc)); err != nil {
			t.Fatal(err)
		}
	}
	after, _ := json.Marshal(g.Snapshot)
	if string(before) != string(after) {
		t.Fatal("provider mutated its snapshot")
	}
}

func TestGraphAbsence(t *testing.T) {
	g := newTestGraph(t)
	ctx := context.Background()
	for _, loc := range []string{"graph:.!/missing.go", "graph:.!/a.go!/Nope", "graph:.!/Nope"} {
		if _, err := g.Stat(ctx, graphLoc(t, loc)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s stat: %v", loc, err)
		}
		if _, err := g.Edges(ctx, graphLoc(t, loc)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("%s edges: %v", loc, err)
		}
	}
	// Listing an absence is os.ErrNotExist for files, ErrUnsupported for symbols.
	if _, err := g.List(ctx, graphLoc(t, "graph:.!/missing.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file list: %v", err)
	}
	if _, err := g.List(ctx, graphLoc(t, "graph:.!/a.go!/Nope")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("missing symbol list: %v", err)
	}
	// No cache on disk is an explicit, actionable absence, not an empty tree.
	empty := NewGraph(t.TempDir())
	if _, err := empty.Stat(ctx, graphLoc(t, "graph:.")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing cache stat: %v", err)
	}
	if _, err := empty.List(ctx, graphLoc(t, "graph:.")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing cache list: %v", err)
	}
}

func TestGraphErrors(t *testing.T) {
	g := newTestGraph(t)
	ctx := context.Background()
	if _, err := g.Stat(ctx, graphLoc(t, "graph:sub")); err == nil {
		t.Fatal("foreign target allowed")
	}
	if _, err := g.Stat(ctx, ref.Locator{Scheme: "dir", Target: "."}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("wrong scheme: %v", err)
	}
	if _, err := g.Stat(ctx, ref.Locator{Scheme: "graph", Target: ".", ScopeKind: "host", ScopeValue: "h"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("scope: %v", err)
	}
	if _, err := g.Stat(ctx, ref.Locator{Scheme: "graph", Target: ".", Inner: []string{"a", "b", "c"}}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("deep inner: %v", err)
	}
	if _, err := g.Open(ctx, graphLoc(t, "graph:.")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open: %v", err)
	}
	if _, err := g.List(ctx, graphLoc(t, "graph:.!/a.go!/Alpha")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("list symbol: %v", err)
	}
}
