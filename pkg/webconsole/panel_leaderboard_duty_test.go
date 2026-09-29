// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/ladder"
)

func seedLadderEvents(t *testing.T, events []ladder.Event, err error) {
	t.Helper()
	orig := readLadderEventsFn
	t.Cleanup(func() { readLadderEventsFn = orig })
	readLadderEventsFn = func() ([]ladder.Event, error) { return events, err }
}

func testDutyStamp(season, i int) time.Time {
	return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).
		Add(time.Duration(season)*time.Hour + time.Duration(i)*time.Second)
}

func testDutyDeliveries(agent string, season, n int, outcome, cost float64) []ladder.Event {
	events := make([]ladder.Event, 0, n)
	for i := 0; i < n; i++ {
		events = append(events, ladder.Event{
			At:      testDutyStamp(season, i),
			Season:  season,
			Kind:    ladder.EventKindDelivery,
			Agent:   agent,
			Story:   fmt.Sprintf("%s-s%d-%d", agent, season, i),
			Points:  3,
			Outcome: outcome,
			Cost:    cost,
		})
	}
	return events
}

func testDutyCertEvents(agent string, season int, kinds ...ladder.CertKind) []ladder.Event {
	events := make([]ladder.Event, 0, len(kinds))
	for i, k := range kinds {
		events = append(events, ladder.Event{
			At:     testDutyStamp(season, 1000+i),
			Season: season,
			Kind:   ladder.EventKindCert,
			Agent:  agent,
			Cert:   ladder.Certificate{Kind: k, Season: season},
		})
	}
	return events
}

func testSampleDutyEvents() []ladder.Event {
	certs := []ladder.CertKind{ladder.CertL1, ladder.CertL2, ladder.CertL3, ladder.CertSteer}
	var events []ladder.Event
	events = append(events, testDutyDeliveries("agent-a:m1", 1, 8, 1.0, 5.0)...)
	events = append(events, testDutyDeliveries("agent-b:m2", 1, 8, 0.5, 2.0)...)
	events = append(events, testDutyCertEvents("agent-a:m1", 1, certs...)...)
	events = append(events, testDutyCertEvents("agent-b:m2", 1, certs...)...)
	return events
}

// TestDutyLeaderboardAPIShape tests the schema, lines, season, and duty rows.
func TestDutyLeaderboardAPIShape(t *testing.T) {
	h, _ := newBoardTestServer(t)
	events := testSampleDutyEvents()
	seedLadderEvents(t, events, nil)

	d := getJSON(t, h, "/api/sprint/leaderboard/duty")
	if d["schema_version"] != capability.LeaderboardDutySchema {
		t.Fatalf("schema_version = %v, want %s", d["schema_version"], capability.LeaderboardDutySchema)
	}
	if int(d["season"].(float64)) != 1 {
		t.Errorf("season = %v, want 1", d["season"])
	}

	lines, ok := d["lines"].(map[string]any)
	if !ok {
		t.Fatalf("lines missing or wrong type: %v", d["lines"])
	}
	// In the sample events, L3Code was fitted, but L4Manage and others were unfitted (null).
	for _, k := range []string{"L3Code", "L4Code", "L4Manage", "L5Code", "L5Manage", "L5Judge"} {
		if _, exists := lines[k]; !exists {
			t.Errorf("lines missing field %q", k)
		}
	}
	if lines["L4Manage"] != nil {
		t.Errorf("unfitted line L4Manage = %v, want null (nil)", lines["L4Manage"])
	}

	duties, ok := d["duties"].(map[string]any)
	if !ok {
		t.Fatalf("duties missing or wrong type: %v", d["duties"])
	}
	for _, dutyName := range []string{"code", "manage", "judge"} {
		rows, ok := duties[dutyName].([]any)
		if !ok {
			t.Fatalf("duties[%q] missing or wrong type: %v", dutyName, duties[dutyName])
		}
		if dutyName == "code" {
			if len(rows) != 2 {
				t.Fatalf("code duty has %d rows, want 2", len(rows))
			}
			top := rows[0].(map[string]any)
			for _, key := range []string{"rank", "separable", "agent", "r", "rd", "lower", "events", "established", "band", "move"} {
				if _, exists := top[key]; !exists {
					t.Errorf("row missing field %q; got %v", key, top)
				}
			}
			if top["agent"] != "agent-a:m1" {
				t.Errorf("top agent = %v, want agent-a:m1", top["agent"])
			}
			if int(top["rank"].(float64)) != 1 {
				t.Errorf("top rank = %v, want 1", top["rank"])
			}
			if !top["established"].(bool) {
				t.Errorf("top established = %v, want true", top["established"])
			}
		}
	}
}

// TestDutyLeaderboardDutyFilter tests filtering to a single duty (?duty=code).
func TestDutyLeaderboardDutyFilter(t *testing.T) {
	h, _ := newBoardTestServer(t)
	events := testSampleDutyEvents()
	seedLadderEvents(t, events, nil)

	d := getJSON(t, h, "/api/sprint/leaderboard/duty?duty=code")
	duties := d["duties"].(map[string]any)
	if _, ok := duties["code"]; !ok {
		t.Errorf("duties missing code: %v", duties)
	}
	if _, ok := duties["manage"]; ok {
		t.Errorf("duties should not contain manage when duty=code: %v", duties)
	}
}

// TestDutyLeaderboardCostView tests ?cost=1 returns cost metrics.
func TestDutyLeaderboardCostView(t *testing.T) {
	h, _ := newBoardTestServer(t)
	events := testSampleDutyEvents()
	seedLadderEvents(t, events, nil)

	d := getJSON(t, h, "/api/sprint/leaderboard/duty?duty=code&cost=1")
	duties := d["duties"].(map[string]any)
	codeRows := duties["code"].([]any)
	if len(codeRows) == 0 {
		t.Fatal("no code rows in cost view")
	}
	r0 := codeRows[0].(map[string]any)
	if r0["cost"] == nil {
		t.Errorf("cost missing from row: %v", r0)
	}
	if r0["r_per_dollar"] == nil {
		t.Errorf("r_per_dollar missing from row: %v", r0)
	}
}

// TestDutyLeaderboardEmptyStore tests that an empty store returns 200, rows [], and note.
func TestDutyLeaderboardEmptyStore(t *testing.T) {
	h, _ := newBoardTestServer(t)
	seedLadderEvents(t, []ladder.Event{}, nil)

	d := getJSON(t, h, "/api/sprint/leaderboard/duty")
	if d["schema_version"] != capability.LeaderboardDutySchema {
		t.Errorf("schema_version = %v", d["schema_version"])
	}
	rows, ok := d["rows"].([]any)
	if !ok || len(rows) != 0 {
		t.Errorf("rows = %v, want empty []", d["rows"])
	}
	note, ok := d["note"].(string)
	if !ok || !strings.Contains(note, "no rated events yet") {
		t.Errorf("note = %q, want it to contain 'no rated events yet'", note)
	}
}

// TestDutyLeaderboardStoreReadFailure tests that a store error returns 200 with unavailable.
func TestDutyLeaderboardStoreReadFailure(t *testing.T) {
	h, _ := newBoardTestServer(t)
	seedLadderEvents(t, nil, errors.New("disk failure"))

	d := getJSON(t, h, "/api/sprint/leaderboard/duty")
	unavail, ok := d["unavailable"].(string)
	if !ok || !strings.Contains(unavail, "disk failure") {
		t.Errorf("unavailable = %v, want it to mention 'disk failure'", d["unavailable"])
	}
}

// TestDutyAgentEventsEndpoint tests filtering, ordering (newest first), and limit 100.
func TestDutyAgentEventsEndpoint(t *testing.T) {
	h, _ := newBoardTestServer(t)

	// Create 150 events for agent-target, and 20 for agent-other
	var events []ladder.Event
	for i := 0; i < 150; i++ {
		events = append(events, ladder.Event{
			At:      testDutyStamp(1, i),
			Season:  1,
			Kind:    ladder.EventKindDelivery,
			Agent:   "agent-target:m1",
			Story:   fmt.Sprintf("story-%d", i),
			Points:  3,
			Outcome: 1.0,
			Cost:    1.5,
		})
	}
	for i := 0; i < 20; i++ {
		events = append(events, ladder.Event{
			At:      testDutyStamp(1, 200+i),
			Season:  1,
			Kind:    ladder.EventKindDelivery,
			Agent:   "agent-other:m2",
			Story:   fmt.Sprintf("other-%d", i),
			Points:  2,
			Outcome: 1.0,
		})
	}
	seedLadderEvents(t, events, nil)

	// Fetch for agent-target:m1
	d := getJSON(t, h, "/api/sprint/leaderboard/duty/agent?name=agent-target:m1")
	if d["agent"] != "agent-target:m1" {
		t.Errorf("agent = %v, want agent-target:m1", d["agent"])
	}
	rawEvents, ok := d["events"].([]any)
	if !ok {
		t.Fatalf("events missing or not a list: %v", d)
	}

	// Max limit is 100
	if len(rawEvents) != 100 {
		t.Fatalf("got %d events, want limit of 100", len(rawEvents))
	}

	// Ordering: newest first (index 149 should be first, index 50 should be 100th)
	first := rawEvents[0].(map[string]any)
	if first["story"] != "story-149" {
		t.Errorf("first event story = %v, want story-149 (newest first)", first["story"])
	}
	last := rawEvents[99].(map[string]any)
	if last["story"] != "story-50" {
		t.Errorf("last event story = %v, want story-50", last["story"])
	}

	// Agent filter: none should belong to agent-other
	for idx, r := range rawEvents {
		ev := r.(map[string]any)
		if ev["agent"] != "agent-target:m1" {
			t.Errorf("event %d has agent = %v, want agent-target:m1", idx, ev["agent"])
		}
	}

	// No paths in payload: check serialized JSON string
	body := do(h, "GET", "/api/sprint/leaderboard/duty/agent?name=agent-target:m1", "127.0.0.1:5555", nil).Body.String()
	if strings.Contains(body, "/Users/") || strings.Contains(body, "/home/") || strings.Contains(body, "\\Users\\") {
		t.Errorf("payload contains filesystem path: %s", body)
	}
}

// TestDutyLeaderboardAPIIsReadOnly verifies mutating methods are not handled.
func TestDutyLeaderboardAPIIsReadOnly(t *testing.T) {
	h, _ := newBoardTestServer(t)
	seedLadderEvents(t, testSampleDutyEvents(), nil)

	for _, m := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		body := do(h, m, "/api/sprint/leaderboard/duty", "127.0.0.1:5555", nil).Body.String()
		if strings.Contains(body, capability.LeaderboardDutySchema) {
			t.Errorf("%s /api/sprint/leaderboard/duty was answered by duty handler; GET only", m)
		}
		agentBody := do(h, m, "/api/sprint/leaderboard/duty/agent?name=agent-a:m1", "127.0.0.1:5555", nil).Body.String()
		if strings.Contains(agentBody, "agent-a:m1") {
			t.Errorf("%s /api/sprint/leaderboard/duty/agent was answered; GET only", m)
		}
	}
}
