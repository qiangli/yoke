// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"reflect"
	"strings"
	"testing"
)

func doc(t *testing.T, md string) *Document {
	t.Helper()
	d, err := Parse(strings.NewReader(md), "DAG.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return d
}

func names(nodes []*Node) []string {
	out := make([]string, len(nodes))
	for i, n := range nodes {
		out[i] = n.Task.Name
	}
	return out
}

func TestBuildGraphUnknownDep(t *testing.T) {
	g := doc(t, "## Tasks\n\n### a\nRequires: ghost\n"+block("", "echo a"))
	if _, err := BuildGraph(g); err == nil {
		t.Fatal("want unknown-dep error")
	} else if ExitCodeOf(err) != 2 {
		t.Errorf("want exit 2, got %d", ExitCodeOf(err))
	}
}

func TestSubgraphUnknownTargetSuggestsNearest(t *testing.T) {
	md := "## Tasks\n\n" +
		"### build\n" + block("", "echo build") +
		"### install\n" + block("", "echo install")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}
	_, err = g.Subgraph("insall")
	if err == nil {
		t.Fatal("want unknown-target error")
	}
	if got := err.Error(); !strings.Contains(got, `unknown target "insall"`) ||
		!strings.Contains(got, `did you mean "install"?`) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestTopoSortOrderAndDeterminism(t *testing.T) {
	// clean -> {compile, docs} -> package (fan-out + fan-in)
	md := "## Tasks\n\n" +
		"### clean\n" + block("", "echo clean") +
		"### compile\nRequires: clean\n" + block("", "echo compile") +
		"### docs\nRequires: clean\n" + block("", "echo docs") +
		"### package\nRequires: compile, docs\n" + block("", "echo package")
	g, err := BuildGraph(doc(t, md))
	if err != nil {
		t.Fatalf("BuildGraph: %v", err)
	}

	first, err := g.TopoSort()
	if err != nil {
		t.Fatalf("TopoSort: %v", err)
	}
	if !reflect.DeepEqual(names(first), []string{"clean", "compile", "docs", "package"}) {
		t.Fatalf("order = %v", names(first))
	}
	// Determinism: a second sort yields the identical order.
	second, _ := g.TopoSort()
	if !reflect.DeepEqual(names(first), names(second)) {
		t.Errorf("non-deterministic: %v vs %v", names(first), names(second))
	}
}

func TestSubgraphClosure(t *testing.T) {
	md := "## Tasks\n\n" +
		"### a\n" + block("", "echo a") +
		"### b\nRequires: a\n" + block("", "echo b") +
		"### unrelated\n" + block("", "echo u")
	g, _ := BuildGraph(doc(t, md))
	sub, err := g.Subgraph("b")
	if err != nil {
		t.Fatalf("Subgraph: %v", err)
	}
	if !reflect.DeepEqual(sub.Order, []string{"a", "b"}) {
		t.Errorf("closure = %v (should exclude 'unrelated')", sub.Order)
	}
}

func TestDetectCycle(t *testing.T) {
	md := "## Tasks\n\n" +
		"### a\nRequires: c\n" + block("", "echo a") +
		"### b\nRequires: a\n" + block("", "echo b") +
		"### c\nRequires: b\n" + block("", "echo c")
	g, _ := BuildGraph(doc(t, md))
	if cyc, ok := g.DetectCycle(); !ok {
		t.Fatal("want cycle")
	} else if len(cyc) < 2 {
		t.Errorf("cycle path too short: %v", cyc)
	}
	if _, err := g.TopoSort(); err == nil {
		t.Fatal("TopoSort should reject a cycle")
	} else if ExitCodeOf(err) != 4 {
		t.Errorf("want exit 4 (state conflict), got %d", ExitCodeOf(err))
	}
}
