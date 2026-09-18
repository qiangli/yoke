// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package spacegraph

import (
	"testing"
	"time"
)

func ctx(at time.Time) Context {
	return Context{
		Self: "dragon", Coordinate: "c8f3", Place: "p1a9",
		At: at, Source: "exec:ssh",
	}
}

func find(edges []Edge, src string, rel Relation, dst string) (Edge, bool) {
	for _, e := range edges {
		if e.Src == src && e.Rel == rel && e.Dst == dst {
			return e, true
		}
	}
	return Edge{}, false
}

// TestSSHYieldsTheExpectedGraph is the worked example the design is built
// around: one successful ssh must name the host, the endpoint, the account, and
// the relation between them.
func TestSSHYieldsTheExpectedGraph(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	now := time.Now().UTC()

	obs := Observe([]string{"ssh", "-p", "2222", "user@remote.host"}, true, ctx(now))
	if err := s.RecordAll(obs); err != nil {
		t.Fatal(err)
	}

	edges, err := s.Edges(now)
	if err != nil {
		t.Fatal(err)
	}

	e, ok := find(edges, "host:dragon", RelReached, "endpoint:remote.host:2222")
	if !ok {
		t.Fatalf("missing the reached edge; got %+v", edges)
	}
	if e.Via != "account:user@remote.host" {
		t.Errorf("reached edge must carry the account it authenticated as, got %q", e.Via)
	}
	if e.N != 1 || e.OK != 1 {
		t.Errorf("want n=1 ok=1, got n=%d ok=%d", e.N, e.OK)
	}
	if e.Coordinate != "c8f3" || e.Place != "p1a9" {
		t.Errorf("edge must record where it was observed: %+v", e)
	}

	if _, ok := find(edges, "endpoint:remote.host:2222", RelRunsOn, "host:remote.host"); !ok {
		t.Errorf("endpoint must be tied back to its host; got %+v", edges)
	}

	nodes, err := s.Nodes(now)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"host:dragon": true, "host:remote.host": true,
		"endpoint:remote.host:2222": true, "account:user@remote.host": true,
		"net:p1a9": true,
	}
	for _, n := range nodes {
		delete(want, n.ID)
	}
	if len(want) != 0 {
		t.Errorf("missing nodes: %v (got %+v)", want, nodes)
	}
}

// TestEdgesAccumulateNotFragment is THE anti-fragmentation assertion.
//
// If time, counts or an episode ever leak into the edge key, this is the test
// that catches it — and it is the failure that would otherwise be silent: the
// store fills with singletons, nothing errors, and the graph learns nothing.
func TestEdgesAccumulateNotFragment(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)

	base := time.Now().UTC().Add(-72 * time.Hour)
	for i := 0; i < 7; i++ {
		at := base.Add(time.Duration(i) * 9 * time.Hour)
		if err := s.RecordAll(Observe(
			[]string{"ssh", "-p", "2222", "user@remote.host"}, true, ctx(at))); err != nil {
			t.Fatal(err)
		}
	}

	now := time.Now().UTC()
	edges, _ := s.Edges(now)
	e, ok := find(edges, "host:dragon", RelReached, "endpoint:remote.host:2222")
	if !ok {
		t.Fatal("edge disappeared")
	}
	if e.N != 7 || e.OK != 7 {
		t.Errorf("seven observations must be ONE edge with n=7, got n=%d ok=%d", e.N, e.OK)
	}
	if !e.First.Before(e.Last) {
		t.Errorf("first/last must span the observations: %v .. %v", e.First, e.Last)
	}

	// And the identity must be stable across all of them.
	seen := map[string]bool{}
	for _, x := range edges {
		if seen[x.ID()] {
			t.Errorf("duplicate edge identity %s", x.ID())
		}
		seen[x.ID()] = true
	}
}

// TestPartialSuccessIsVisible — an edge seen 3 times and working once is a
// different claim from one seen once and working once. Averaging hides it.
func TestPartialSuccessIsVisible(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	now := time.Now().UTC()

	argv := []string{"ssh", "user@remote.host"}
	_ = s.RecordAll(Observe(argv, true, ctx(now)))
	_ = s.RecordAll(Observe(argv, false, ctx(now.Add(time.Minute))))
	_ = s.RecordAll(Observe(argv, false, ctx(now.Add(2*time.Minute))))

	edges, _ := s.Edges(now.Add(3 * time.Minute))
	e, ok := find(edges, "host:dragon", RelReached, "host:remote.host")
	if !ok {
		t.Fatal("missing edge")
	}
	if e.N != 3 || e.OK != 1 {
		t.Errorf("want n=3 ok=1 so the contest is visible, got n=%d ok=%d", e.N, e.OK)
	}
}

// TestSupersedeIsBiTemporal — closing an edge must not erase what was believed
// before. "What did this machine think last Tuesday" is the only way to explain
// why an agent did what it did.
func TestSupersedeIsBiTemporal(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)

	then := time.Now().UTC().Add(-48 * time.Hour)
	_ = s.RecordAll(Observe([]string{"ssh", "user@remote.host"}, true, ctx(then)))

	edges, _ := s.Edges(then)
	if len(edges) == 0 {
		t.Fatal("no edge recorded")
	}
	id := ""
	for _, e := range edges {
		if e.Rel == RelReached {
			id = e.ID()
		}
	}

	cut := time.Now().UTC().Add(-24 * time.Hour)
	if err := s.Supersede(id, cut); err != nil {
		t.Fatal(err)
	}

	// Gone now...
	if live, _ := s.Live(time.Now().UTC()); live[id].Rel != "" {
		t.Error("superseded edge must not be live now")
	}
	// ...but still true then.
	if live, _ := s.Live(then.Add(time.Hour)); live[id].Rel == "" {
		t.Error("superseded edge must still be believed at the earlier time")
	}
}

// TestNoSelfEdge — reaching yourself is not a fact about the environment.
func TestNoSelfEdge(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	now := time.Now().UTC()

	_ = s.RecordAll(Observe([]string{"ssh", "user@dragon"}, true, ctx(now)))

	edges, _ := s.Edges(now)
	for _, e := range edges {
		if e.Src == "host:dragon" && e.Rel == RelReached && e.Dst == "host:dragon" {
			t.Errorf("self-edge recorded: %+v", e)
		}
	}
}

// TestUnknownBinaryTeachesNothing — inventing an edge from a flag shape is how
// a wrong fact enters a store an agent trusts.
func TestUnknownBinaryTeachesNothing(t *testing.T) {
	obs := Observe([]string{"some-unknown-tool", "-p", "2222", "user@remote.host"}, true,
		Context{Self: "dragon", At: time.Now().UTC()})
	for _, o := range obs {
		if o.Rel == RelReached {
			t.Errorf("must not infer a connection from an unknown binary: %+v", o)
		}
	}
}

// TestIdempotentReplay — reading the log twice must not double the counts.
func TestIdempotentReplay(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	now := time.Now().UTC()
	_ = s.RecordAll(Observe([]string{"ssh", "user@remote.host"}, true, ctx(now)))

	a, _ := s.Edges(now)
	b, _ := s.Edges(now)
	if len(a) != len(b) {
		t.Fatalf("replay is not stable: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].N != b[i].N {
			t.Errorf("counts drift on replay: %+v vs %+v", a[i], b[i])
		}
	}
}

func TestEdgeIDExcludesTime(t *testing.T) {
	a := Edge{Src: "host:x", Rel: RelReached, Dst: "host:y",
		N: 1, First: time.Now(), Last: time.Now()}
	b := Edge{Src: "host:x", Rel: RelReached, Dst: "host:y",
		N: 99, First: time.Now().Add(time.Hour), Last: time.Now().Add(2 * time.Hour)}
	if a.ID() != b.ID() {
		t.Error("edge identity must not depend on time or counts — this is the " +
			"silent-fragmentation failure")
	}
}
