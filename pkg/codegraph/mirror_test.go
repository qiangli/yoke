package codegraph

import (
	"context"
	"strings"
	"testing"

	"github.com/qiangli/gfy/pkg/graph"
)

// fakeMirror records every Upsert / Mutate call so the test can assert on
// batching behavior. It is intentionally not a partial of graphMirror — it
// is the full interface, so a missing method would fail to compile.
type fakeMirror struct {
	mutateCalls []string
	upsertCalls []struct {
		query string
		nq    string
	}
}

func (f *fakeMirror) Mutate(_ context.Context, nq []byte) (map[string]string, error) {
	f.mutateCalls = append(f.mutateCalls, string(nq))
	return nil, nil
}

func (f *fakeMirror) Upsert(_ context.Context, query string, nq []byte) error {
	f.upsertCalls = append(f.upsertCalls, struct {
		query string
		nq    string
	}{query: query, nq: string(nq)})
	return nil
}

func (f *fakeMirror) MirrorTarget() {}

func TestMirrorTo_BatchesEdgeUpserts(t *testing.T) {
	// Build a small synthetic graph with 1000 nodes + 1000 edges. Without
	// batching this would emit 1000 Upsert calls; with batchSize=200 we
	// expect 5. Catches the regression that previously did one round-trip
	// per edge and pegged the GC under sustained load.
	g := graph.New(true)
	const n = 1000
	for i := 0; i < n; i++ {
		label := "node-" + itoa(i)
		g.AddNode(label, map[string]any{"kind": "func", "file": "main.go"})
	}
	for i := 0; i < n; i++ {
		src := "node-" + itoa(i)
		dst := "node-" + itoa((i+1)%n)
		g.AddEdge(src, dst, map[string]any{"type": "calls"})
	}
	gc := &GraphContext{Graph: g}

	fake := &fakeMirror{}
	if err := gc.MirrorTo(context.Background(), fake); err != nil {
		t.Fatalf("MirrorTo: %v", err)
	}

	// Nodes: 1000 / 500 batch size = 2 Mutate calls.
	if got, want := len(fake.mutateCalls), 2; got != want {
		t.Errorf("Mutate calls: got %d, want %d", got, want)
	}

	// Edges: 1000 / 200 batch size = 5 Upsert calls. Strictly: must be
	// fewer than the edge count — this is the regression guard.
	if len(fake.upsertCalls) >= n {
		t.Fatalf("Upsert calls (%d) should be far fewer than edge count (%d) — batching regressed",
			len(fake.upsertCalls), n)
	}
	if got, want := len(fake.upsertCalls), 5; got != want {
		t.Errorf("Upsert calls: got %d, want %d", got, want)
	}

	// Each batch query should contain multiple eq(code.label, ...) lookups
	// and the mutation should reference uid(v0) ... uid(vN).
	first := fake.upsertCalls[0]
	if c := strings.Count(first.query, "eq(code.label,"); c < 100 {
		t.Errorf("first batch query has only %d code.label lookups; expected ~400 (2 per edge × 200)", c)
	}
	if !strings.Contains(first.nq, "uid(v0)") || !strings.Contains(first.nq, "uid(v199)") {
		t.Errorf("first batch mutation missing expected uid bindings; got: %s", truncate(first.nq, 200))
	}
}

func TestMirrorTo_EmptyGraph_NoUpserts(t *testing.T) {
	gc := &GraphContext{Graph: graph.New(true)}
	fake := &fakeMirror{}
	if err := gc.MirrorTo(context.Background(), fake); err != nil {
		t.Fatalf("MirrorTo: %v", err)
	}
	if len(fake.upsertCalls) != 0 {
		t.Errorf("empty graph should produce zero upserts; got %d", len(fake.upsertCalls))
	}
	if len(fake.mutateCalls) != 0 {
		t.Errorf("empty graph should produce zero mutates; got %d", len(fake.mutateCalls))
	}
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
