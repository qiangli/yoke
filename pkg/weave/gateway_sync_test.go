package weave

import (
	"context"
	"encoding/json"
	"testing"
)

// syncClaimedConductor claims key on the sync route through the exported
// host entry point, sharing the conductor's host-scoped state directory.
func syncClaimedConductor(t *testing.T, key, taskID string) (*gatewayConductor, *gatewayTestClient) {
	t.Helper()
	t.Setenv("BASHY_HOME", t.TempDir())
	dir, err := gatewayStateDir()
	if err != nil {
		t.Fatal(err)
	}
	c := &gatewayTestClient{directiveMockSessionClient: &directiveMockSessionClient{}}
	g := newGatewayConductor(c, dir)
	g.execute = func(context.Context, gatewayDirective) (string, int, bool) {
		t.Fatal("pull route executed a sync-claimed task")
		return "", 0, false
	}
	id, fresh, err := ClaimGatewayTaskOnHost(key, taskID)
	if err != nil || !fresh || id != taskID {
		t.Fatalf("sync claim = %q %v %v", id, fresh, err)
	}
	return g, c
}

func gatewayEventsOf(c *gatewayTestClient, kind string) []map[string]any {
	var out []map[string]any
	for _, ev := range c.snapshot() {
		if ev.Kind != kind {
			continue
		}
		var m map[string]any
		_ = json.Unmarshal(ev.Detail, &m)
		out = append(out, m)
	}
	return out
}

func assertSyncResultPublished(t *testing.T, c *gatewayTestClient) {
	t.Helper()
	results := gatewayEventsOf(c, "result")
	if len(results) != 1 || results[0]["text"] != "sync out" || results[0]["exit"] != float64(0) || results[0]["verified"] != true {
		t.Fatalf("result events = %v", results)
	}
	var terminal bool
	for _, s := range gatewayEventsOf(c, "state") {
		if s["state"] == "done" {
			terminal = true
		}
	}
	if !terminal {
		t.Fatalf("no done state: %v", gatewayEventsOf(c, "state"))
	}
}

func TestGatewayPullAfterCompletedSyncEmitsStoredResult(t *testing.T) {
	isolateGateway(t)
	g, c := syncClaimedConductor(t, "k-sync", "task")
	if err := CompleteGatewayTaskOnHost("k-sync", "task", "sync out", 0, true, []byte(`{"content":[]}`)); err != nil {
		t.Fatal(err)
	}
	// Before any pull directive the outbox owes nothing: a sync call that
	// answered has no pull timeline to publish to.
	if err := g.flushOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if evs := c.snapshot(); len(evs) != 0 {
		t.Fatalf("unadopted sync record emitted %v", evs)
	}
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "1", "task", "k-sync", "run")); err != nil {
		t.Fatal(err)
	}
	g.wg.Wait()
	if len(gatewayEventsOf(c, "ack")) != 1 {
		t.Fatalf("ack events = %v", c.snapshot())
	}
	assertSyncResultPublished(t, c)
	// A redelivered directive and later outbox passes publish nothing again.
	before := len(c.snapshot())
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "1", "task", "k-sync", "run")); err != nil {
		t.Fatal(err)
	}
	if err := g.flushOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(gatewayEventsOf(c, "result")); got != 1 || len(c.snapshot()) != before+1 {
		t.Fatalf("duplicate publication: %v", c.snapshot()[before:])
	}
}

func TestGatewayPullAfterInFlightSyncWaitsForResult(t *testing.T) {
	isolateGateway(t)
	g, c := syncClaimedConductor(t, "k-sync", "task")
	if err := g.apply(context.Background(), "task", gatewayEvent(t, "1", "task", "k-sync", "run")); err != nil {
		t.Fatal(err)
	}
	g.wg.Wait()
	states := gatewayEventsOf(c, "state")
	if len(states) != 1 || states[0]["state"] != "running" || len(gatewayEventsOf(c, "result")) != 0 {
		t.Fatalf("in-flight events = %v", c.snapshot())
	}
	// The sync handler finishes; the next poll's outbox pass publishes it.
	if err := CompleteGatewayTaskOnHost("k-sync", "task", "sync out", 0, true, nil); err != nil {
		t.Fatal(err)
	}
	if err := g.flushOutbox(context.Background()); err != nil {
		t.Fatal(err)
	}
	assertSyncResultPublished(t, c)
	r, err := readGatewayRecord(gatewayRecordPath(g.dir, "k-sync"))
	if err != nil || !r.ResultSent || !r.TerminalStateSent || r.State != "done" {
		t.Fatalf("record = %+v %v", r, err)
	}
}
