// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package board

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet/fleettest"
	"github.com/qiangli/yoke/pkg/room"
)

// Sprint 220, story 688d01a9: the console showed a whole fleet as
// "unavailable" and a seat holding a sprint as idle. Two registries the board
// never read, and one it read in the wrong field names.

// `weave fleet --agents --json` writes `installed`, never `available`; the
// board decoded `found`/`available` and so read every row as absent.
func TestFleetAvailabilityDecodesTheWireItIsGiven(t *testing.T) {
	raw := `{"tools":[
	  {"agent":"claude-fable5","binding":"claude:fable5","installed":true,"path":"/x/claude","tool":"claude"},
	  {"agent":"agy-opus4.6","binding":"agy:opus4.6","installed":true,"tool":"agy","cooling_until":"2026-09-20T10:00:00Z"},
	  {"agent":"agy-gpt-oss-120b","installed":false,"tool":"agy"},
	  {"agent":"claude-haiku4.5","installed":true,"tool":"claude","probed":true,"usable":false}]}`
	var result struct {
		Tools []fleetAvailability `json:"tools"`
	}
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		t.Fatal(err)
	}
	got := map[string][2]bool{}
	for _, r := range result.Tools {
		got[r.Agent] = [2]bool{r.found(), r.available()}
	}
	want := map[string][2]bool{
		"claude-fable5":    {true, true},
		"agy-opus4.6":      {true, false}, // installed but cooling
		"agy-gpt-oss-120b": {false, false},
		"claude-haiku4.5":  {true, false}, // probe ran and failed
	}
	for name, w := range want {
		if got[name] != w {
			t.Fatalf("%s: found/available = %v, want %v", name, got[name], w)
		}
	}
}

// Seed all three registries and assert the board agrees with each: the
// catalog row is available; the room member is `live`; the fresh lease holder
// is `conducting` its sprint; a lease name the catalog never registered is
// still a row, because a seat that exists must be listable to be chosen.
func TestBoardShowsLiveSeatsAndConductorsNotJustTheCatalog(t *testing.T) {
	fleettest.Ring(t)
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Chdir(t.TempDir())

	// A live seat: a running process (this one) holding the fable identity.
	if err := room.Join(room.Card{ID: room.AgentClaimID("claude-fable5"), Nick: "claude-fable5", Tool: "claude", Binding: "claude:fable5", Mode: "interactive", Task: "inbox --watch", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	// A dead seat must not count: room.Members prunes it.
	if err := room.Join(room.Card{ID: room.AgentClaimID("agy-opus4.6"), Nick: "agy-opus4.6", Tool: "agy", Binding: "agy:opus4.6", PID: 999999999}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	b := &Board{Sprints: []Sprint{
		{ID: 220, Column: "doing", LeaseHolder: "plinth", Conductor: "plinth"},               // fresh lease, name not in the catalog
		{ID: 216, Column: "doing", LeaseHolder: "claude-haiku4.5", LeaseStale: true},         // stale: no claim on the seat
		{ID: 214, Column: "doing", LeaseHolder: "agy-gemini3.1", Conductor: "agy-gemini3.1"}, // fresh lease on a catalog agent
	}}
	wire := func() (map[string]fleetAvailability, error) {
		return map[string]fleetAvailability{
			"claude-fable5":   {Agent: "claude-fable5", Installed: true},
			"agy-gemini3.1":   {Agent: "agy-gemini3.1", Installed: true},
			"claude-haiku4.5": {Agent: "claude-haiku4.5", Installed: true},
		}, nil
	}
	if err := (fleetSource{loadAvailability: wire}).Load(context.Background(), b, Options{Now: now}); err != nil {
		t.Fatal(err)
	}
	rows := map[string]Agent{}
	for _, a := range b.Agents {
		rows[a.Name] = a
	}
	if a := rows["claude-fable5"]; !a.Found || !a.Available || a.State != "live" || !a.Live || a.Mode != "interactive" {
		t.Fatalf("live seat rendered as %+v", a)
	}
	if a := rows["agy-opus4.6"]; a.Live || a.State == "live" {
		t.Fatalf("a dead seat rendered live: %+v", a)
	}
	if a := rows["agy-gemini3.1"]; a.State != "conducting" || a.Sprint != 214 || !a.Available {
		t.Fatalf("fresh lease holder rendered as %+v", a)
	}
	if a := rows["claude-haiku4.5"]; a.State == "conducting" {
		t.Fatalf("a STALE lease made a conductor: %+v", a)
	}
	a, ok := rows["plinth"]
	if !ok {
		t.Fatalf("the seat holding sprint 220 is in no list: %v", keys(rows))
	}
	if a.State != "conducting" || a.Sprint != 220 {
		t.Fatalf("uncatalogued conductor rendered as %+v", a)
	}
}

func keys(m map[string]Agent) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
