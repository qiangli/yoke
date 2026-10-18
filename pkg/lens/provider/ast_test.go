package provider

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/qiangli/yoke/pkg/ref"
)

const astSrc = `package x

func Alpha() {}

type Beta struct{}

func (Beta) Run() {}
`

func newTestAST(t *testing.T) *AST {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(astSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewAST(dir)
}

func astLoc(t *testing.T, s string) ref.Locator {
	t.Helper()
	l, err := ref.ParseLocator(s)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func TestASTStatFileAndList(t *testing.T) {
	a := newTestAST(t)
	ctx := context.Background()
	n, err := a.Stat(ctx, astLoc(t, "ast:x.go"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != "file" || n.Type != "ast/file" || n.Ref != "ast:x.go" || n.Attrs["language"] != "go" {
		t.Fatalf("file node: %+v", n)
	}
	nodes, err := a.List(ctx, astLoc(t, "ast:x.go"))
	if err != nil {
		t.Fatal(err)
	}
	if n.Children == nil || *n.Children != uint64(len(nodes)) || len(nodes) < 2 {
		t.Fatalf("children mismatch: %+v nodes=%d", n.Children, len(nodes))
	}
	names := map[string]bool{}
	for _, s := range nodes {
		if s.Kind != "symbol" || s.SchemaVersion != NodeSchema {
			t.Fatalf("bad symbol node: %+v", s)
		}
		names[s.Name] = true
	}
	if !names["Alpha"] || !names["Beta"] {
		t.Fatalf("missing symbols: %v", names)
	}
}

func TestASTStatSymbolAndEdges(t *testing.T) {
	a := newTestAST(t)
	ctx := context.Background()
	s, err := a.Stat(ctx, astLoc(t, "ast:x.go!/Alpha"))
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "Alpha" || s.Attrs["line"] != 3 || s.Attrs["exported"] != true {
		t.Fatalf("symbol: %+v", s)
	}
	edges, err := a.Edges(ctx, astLoc(t, "ast:x.go"))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range edges {
		if e.Rel != "contains" || e.Src != "ast:x.go" || e.SchemaVersion != EdgeSchema {
			t.Fatalf("bad edge: %+v", e)
		}
		if e.Dst == "ast:x.go!/Alpha" {
			found = true
		}
	}
	if !found {
		t.Fatalf("no contains edge to Alpha: %+v", edges)
	}
	back, err := a.Edges(ctx, astLoc(t, "ast:x.go!/Alpha"))
	if err != nil || len(back) != 1 || back[0].Rel != "defined_in" || back[0].Dst != "ast:x.go" {
		t.Fatalf("back edge: %+v %v", back, err)
	}
}

func TestASTErrors(t *testing.T) {
	a := newTestAST(t)
	ctx := context.Background()
	if _, err := a.Stat(ctx, astLoc(t, "ast:x.go!/Nope")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing symbol: %v", err)
	}
	if _, err := a.Stat(ctx, astLoc(t, "ast:missing.go")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file: %v", err)
	}
	if _, err := a.Stat(ctx, astLoc(t, "ast:notes.txt")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("unsupported lang: %v", err)
	}
	if _, err := a.Stat(ctx, ref.Locator{Scheme: "ast", Target: "../x.go"}); err == nil {
		t.Fatal("escape allowed")
	}
	if _, err := a.Stat(ctx, ref.Locator{Scheme: "dir", Target: "x.go"}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("wrong scheme: %v", err)
	}
	if _, err := a.Open(ctx, astLoc(t, "ast:x.go")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("open: %v", err)
	}
	if _, err := a.List(ctx, astLoc(t, "ast:x.go!/Alpha")); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("list symbol: %v", err)
	}
}
