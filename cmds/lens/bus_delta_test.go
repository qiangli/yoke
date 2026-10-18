package lenscmd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/room"
)

func isolateRoom(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", dir)
	return dir
}

func TestBusDeltaRegistration(t *testing.T) {
	src, ok := deltaSources["bus"]
	if !ok || src == nil {
		t.Fatal("delta source for bus scheme is not registered")
	}
}

func TestBusDeltaSinceStartAndResume(t *testing.T) {
	isolateRoom(t)

	room.Record(room.EventSprint, "mgr", "sprint:446", "decision", "picked architecture")
	room.Record(room.EventTodo, "agent1", "todo:13778c0d7a83", "doing", "working on bus delta")

	workDir := t.TempDir()
	out, stderr, code := runTool(t, workDir, "", "--since=start", "-t", "jsonl", "bus:events")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q out=%q", code, stderr, out)
	}

	data, cursor := deltaCursor(t, out)
	lines := strings.Split(data, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d:\n%s", len(lines), data)
	}

	for i, expectedType := range []string{room.EventSprint, room.EventTodo} {
		var row map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &row); err != nil {
			t.Fatalf("line %d: invalid JSON: %v", i, err)
		}
		if row["schema_version"] != bus.EventsSchemaVersion {
			t.Errorf("line %d: schema_version = %v, want %s", i, row["schema_version"], bus.EventsSchemaVersion)
		}
		if row["type"] != expectedType {
			t.Errorf("line %d: type = %v, want %s", i, row["type"], expectedType)
		}
		if int64(row["seq"].(float64)) != int64(i+1) {
			t.Errorf("line %d: seq = %v, want %d", i, row["seq"], i+1)
		}
	}

	// Resuming when no events were added yields empty records and unchanged position.
	out2, stderr2, code2 := runTool(t, workDir, "", "--since", cursor, "-t", "jsonl", "bus:events")
	if code2 != 0 || stderr2 != "" {
		t.Fatalf("resume empty: code=%d stderr=%q", code2, stderr2)
	}
	data2, cursor2 := deltaCursor(t, out2)
	if data2 != "" {
		t.Fatalf("expected empty data on resume with no new events, got: %q", data2)
	}

	// Record a 3rd real event.
	room.Record(room.EventHandoff, "agent1", "handoff:h1", "claimed", "handoff to reviewer")

	// Resuming delivers ONLY the new event.
	out3, stderr3, code3 := runTool(t, workDir, "", "--since", cursor2, "-t", "jsonl", "bus:events")
	if code3 != 0 || stderr3 != "" {
		t.Fatalf("resume new: code=%d stderr=%q", code3, stderr3)
	}
	data3, _ := deltaCursor(t, out3)
	lines3 := strings.Split(data3, "\n")
	if len(lines3) != 1 {
		t.Fatalf("expected 1 new event, got %d:\n%s", len(lines3), data3)
	}
	var row3 map[string]any
	if err := json.Unmarshal([]byte(lines3[0]), &row3); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if row3["type"] != room.EventHandoff || int64(row3["seq"].(float64)) != 3 {
		t.Fatalf("unexpected event: %v", row3)
	}
}

func TestBusDeltaSequenceFiltering(t *testing.T) {
	isolateRoom(t)

	room.Record(room.EventSprint, "mgr", "sprint:446", "decision", "first")
	room.Record(room.EventTodo, "agent1", "todo:abc", "doing", "second")
	room.Record(room.EventHandoff, "agent2", "handoff:h1", "done", "third")

	workDir := t.TempDir()
	out, stderr, code := runTool(t, workDir, "", "--since=start", "-t", "jsonl", "bus:2")
	if code != 0 || stderr != "" {
		t.Fatalf("bus:2 start: code=%d stderr=%q out=%q", code, stderr, out)
	}

	data, cursor := deltaCursor(t, out)
	lines := strings.Split(data, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 event for bus:2, got %d:\n%s", len(lines), data)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatal(err)
	}
	if row["seq"] != float64(2) || row["type"] != room.EventTodo {
		t.Fatalf("expected seq 2 todo, got: %v", row)
	}

	// Resuming on bus:2 delivers no duplicate events.
	out2, stderr2, code2 := runTool(t, workDir, "", "--since", cursor, "-t", "jsonl", "bus:2")
	if code2 != 0 || stderr2 != "" {
		t.Fatalf("bus:2 resume: code=%d stderr=%q", code2, stderr2)
	}
	data2, _ := deltaCursor(t, out2)
	if data2 != "" {
		t.Fatalf("expected no events on bus:2 refresh, got: %q", data2)
	}

	// Changed target (bus:2 cursor on bus:3) is refused with restart guidance.
	out3, stderr3, code3 := runTool(t, workDir, "", "--since", cursor, "-t", "jsonl", "bus:3")
	if code3 == 0 || out3 != "" || !strings.Contains(stderr3, "restart with --since=start") {
		t.Fatalf("expected target mismatch failure: code=%d stderr=%q out=%q", code3, stderr3, out3)
	}
}

func TestBusDeltaKindFiltering(t *testing.T) {
	isolateRoom(t)

	room.Record(room.EventSprint, "mgr", "sprint:446", "decision", "sprint 1")
	room.Record(room.EventTodo, "agent1", "todo:abc", "doing", "todo 1")
	room.Record(room.EventSprint, "mgr", "sprint:446", "progress", "sprint 2")

	workDir := t.TempDir()
	out, stderr, code := runTool(t, workDir, "", "--since=start", "-t", "jsonl", "bus:sprint")
	if code != 0 || stderr != "" {
		t.Fatalf("bus:sprint code=%d stderr=%q", code, stderr)
	}
	data, _ := deltaCursor(t, out)
	lines := strings.Split(data, "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 sprint events, got %d:\n%s", len(lines), data)
	}
	for _, l := range lines {
		var row map[string]any
		if err := json.Unmarshal([]byte(l), &row); err != nil {
			t.Fatal(err)
		}
		if row["type"] != room.EventSprint {
			t.Fatalf("unexpected type in filtered stream: %v", row)
		}
	}
}

func TestBusDeltaStrictLocatorValidation(t *testing.T) {
	isolateRoom(t)
	workDir := t.TempDir()

	for _, tc := range []struct {
		loc string
		err string
	}{
		{"bus:events!/inner", "container descent"},
		{"bus:events@host1", "location scopes"},
		{"bus:not_a_valid_target_xyz", "not a valid sequence"},
		{"bus:0", "not a positive sequence"},
		{"bus:-5", "not a positive sequence"},
	} {
		out, stderr, code := runTool(t, workDir, "", "--since=start", "-t", "jsonl", tc.loc)
		if code == 0 || out != "" || !strings.Contains(stderr, tc.err) {
			t.Errorf("locator %q: code=%d stderr=%q want error containing %q", tc.loc, code, stderr, tc.err)
		}
	}
}

func TestBusDeltaRetentionExpiration(t *testing.T) {
	roomDir := isolateRoom(t)

	room.Record(room.EventSprint, "mgr", "sprint:446", "decision", "event 1")
	room.Record(room.EventTodo, "agent1", "todo:1", "doing", "event 2")

	workDir := t.TempDir()
	out, stderr, code := runTool(t, workDir, "", "--since=start", "-t", "jsonl", "bus:events")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q", code, stderr)
	}
	_, cursor := deltaCursor(t, out)

	// Simulate retention archiving events through seq 5.
	archDir := filepath.Join(roomDir, "archive")
	if err := os.MkdirAll(archDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(archDir, ".watermark"), []byte("5\n"), 0600); err != nil {
		t.Fatal(err)
	}

	// Resuming cursor that was at seq 2 (< floor 5) must fail with ErrDeltaExpired and restart guidance.
	out2, stderr2, code2 := runTool(t, workDir, "", "--since", cursor, "-t", "jsonl", "bus:events")
	if code2 == 0 || out2 != "" || !strings.Contains(stderr2, "delta position expired") || !strings.Contains(stderr2, "restart with --since=start") {
		t.Fatalf("expected expiration guidance: code=%d out=%q stderr=%q", code2, out2, stderr2)
	}

	// Direct query for archived sequence must fail with ErrDeltaExpired.
	out3, stderr3, code3 := runTool(t, workDir, "", "--since=start", "-t", "jsonl", "bus:2")
	if code3 == 0 || out3 != "" || !strings.Contains(stderr3, "delta position expired") || !strings.Contains(stderr3, "restart with --since=start") {
		t.Fatalf("expected archived seq expiration: code=%d out=%q stderr=%q", code3, out3, stderr3)
	}
}

func TestBusDeltaParseResumeToken(t *testing.T) {
	// Valid unsigned decimal tokens
	for _, tc := range []struct {
		token string
		want  int64
	}{
		{"0", 0},
		{"1", 1},
		{"42", 42},
		{"1000000", 1000000},
	} {
		got, err := parseResumeToken(tc.token)
		if err != nil {
			t.Errorf("parseResumeToken(%q) unexpected error: %v", tc.token, err)
		}
		if got != tc.want {
			t.Errorf("parseResumeToken(%q) = %d, want %d", tc.token, got, tc.want)
		}
	}

	// Malformed tokens: {}, unrelated object, negative, fractional, overflow, non-digit
	for _, bad := range []string{
		"{}",
		`{"seq":0}`,
		`{"unrelated":true}`,
		"-1",
		"-42",
		"1.5",
		"3.14",
		"9999999999999999999999999999999999999999999999999999", // overflow
		"",
		"   ",
		" 1 ",
		"0x10",
		"abc",
		"eyl", // base64 {}
	} {
		if got, err := parseResumeToken(bad); err == nil {
			t.Errorf("parseResumeToken(%q) unexpectedly succeeded with %d, want error", bad, got)
		}
	}
}

func TestBusDeltaMalformedTokensInCursor(t *testing.T) {
	isolateRoom(t)
	workDir := t.TempDir()

	for _, badToken := range []string{
		"{}",
		`{"unrelated": 1}`,
		"-1",
		"1.5",
		"9999999999999999999999999999999999999999999999999999",
	} {
		cursorJSON, err := json.Marshal(stream.Cursor{
			SchemaVersion: stream.CursorSchema,
			Kind:          stream.KindChangeStream,
			Token:         badToken,
			Binding: stream.Binding{
				Provider: "bus",
				Target:   "bus:events",
				Query:    "delta:jsonl",
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		out, stderr, code := runTool(t, workDir, "", "--since", string(cursorJSON), "-t", "jsonl", "bus:events")
		if code == 0 || out != "" || !strings.Contains(stderr, "invalid resume token") {
			t.Errorf("token %q: code=%d out=%q stderr=%q, want invalid resume token error", badToken, code, out, stderr)
		}
	}
}

func TestBusDeltaCursorBindingValidation(t *testing.T) {
	isolateRoom(t)
	workDir := t.TempDir()

	// Mismatched provider
	cProvider, _ := json.Marshal(stream.Cursor{
		SchemaVersion: stream.CursorSchema,
		Kind:          stream.KindChangeStream,
		Token:         "1",
		Binding: stream.Binding{
			Provider: "other-provider",
			Target:   "bus:events",
			Query:    "delta:jsonl",
		},
	})
	out, stderr, code := runTool(t, workDir, "", "--since", string(cProvider), "-t", "jsonl", "bus:events")
	if code == 0 || out != "" || !strings.Contains(stderr, "cursor is bound to a different provider, target or query") || !strings.Contains(stderr, "restart with --since=start") {
		t.Errorf("mismatched provider: code=%d out=%q stderr=%q", code, out, stderr)
	}

	// Mismatched query
	cQuery, _ := json.Marshal(stream.Cursor{
		SchemaVersion: stream.CursorSchema,
		Kind:          stream.KindChangeStream,
		Token:         "1",
		Binding: stream.Binding{
			Provider: "bus",
			Target:   "bus:events",
			Query:    "delta:text",
		},
	})
	out, stderr, code = runTool(t, workDir, "", "--since", string(cQuery), "-t", "jsonl", "bus:events")
	if code == 0 || out != "" || !strings.Contains(stderr, "cursor is bound to a different provider, target or query") || !strings.Contains(stderr, "restart with --since=start") {
		t.Errorf("mismatched query: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestBusDeltaCursorPastHead(t *testing.T) {
	isolateRoom(t)
	room.Record(room.EventSprint, "mgr", "sprint:446", "decision", "event 1")

	workDir := t.TempDir()
	cursorJSON, _ := json.Marshal(stream.Cursor{
		SchemaVersion: stream.CursorSchema,
		Kind:          stream.KindChangeStream,
		Token:         "999", // seq 999 is past head (1)
		Binding: stream.Binding{
			Provider: "bus",
			Target:   "bus:events",
			Query:    "delta:jsonl",
		},
	})
	out, stderr, code := runTool(t, workDir, "", "--since", string(cursorJSON), "-t", "jsonl", "bus:events")
	if code == 0 || out != "" || !strings.Contains(stderr, "delta position expired") || !strings.Contains(stderr, "restart with --since=start") {
		t.Fatalf("expected past-head expiration guidance: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

func TestBusDeltaTimeAndDuration(t *testing.T) {
	isolateRoom(t)

	now := time.Now().UTC().Truncate(time.Second)
	tOld := now.Add(-30 * time.Minute)
	tRecent := now.Add(-5 * time.Minute)

	if err := room.Emit(room.Event{Type: room.EventSprint, Actor: "mgr", Target: "sprint:446", TS: tOld.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{Type: room.EventTodo, Actor: "agent1", Target: "todo:13778c0d7a83", TS: tRecent.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()

	// 1. Duration: --since=10m should deliver only the recent event (5 min ago)
	out, stderr, code := runTool(t, workDir, "", "--since=10m", "-t", "jsonl", "bus:events")
	if code != 0 || stderr != "" {
		t.Fatalf("code=%d stderr=%q out=%q", code, stderr, out)
	}
	data, cursor := deltaCursor(t, out)
	lines := strings.Split(data, "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line for --since=10m, got %d:\n%s", len(lines), data)
	}
	var row map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if int64(row["seq"].(float64)) != 2 {
		t.Errorf("seq = %v, want 2", row["seq"])
	}

	// 2. Trailing cursor resume: should deliver 0 records since we are at head
	outRes, stderrRes, codeRes := runTool(t, workDir, "", "--since", cursor, "-t", "jsonl", "bus:events")
	if codeRes != 0 || stderrRes != "" {
		t.Fatalf("resume code=%d stderr=%q", codeRes, stderrRes)
	}
	dataRes, _ := deltaCursor(t, outRes)
	if dataRes != "" {
		t.Fatalf("expected empty data after resume, got %q", dataRes)
	}

	// 3. Absolute timestamp between event 1 and event 2: delivers only event 2
	tBetween := now.Add(-15 * time.Minute).Format(time.RFC3339)
	outBetween, stderrBetween, codeBetween := runTool(t, workDir, "", "--since", tBetween, "-t", "jsonl", "bus:events")
	if codeBetween != 0 || stderrBetween != "" {
		t.Fatalf("code=%d stderr=%q", codeBetween, stderrBetween)
	}
	dataBetween, _ := deltaCursor(t, outBetween)
	linesBetween := strings.Split(dataBetween, "\n")
	if len(linesBetween) != 1 {
		t.Fatalf("expected 1 line for timestamp between, got %d:\n%s", len(linesBetween), dataBetween)
	}

	// 4. Absolute timestamp before event 1: delivers both events
	tBefore := now.Add(-45 * time.Minute).Format(time.RFC3339)
	outBefore, stderrBefore, codeBefore := runTool(t, workDir, "", "--since", tBefore, "-t", "jsonl", "bus:events")
	if codeBefore != 0 || stderrBefore != "" {
		t.Fatalf("code=%d stderr=%q", codeBefore, stderrBefore)
	}
	dataBefore, _ := deltaCursor(t, outBefore)
	linesBefore := strings.Split(dataBefore, "\n")
	if len(linesBefore) != 2 {
		t.Fatalf("expected 2 lines for timestamp before, got %d:\n%s", len(linesBefore), dataBefore)
	}

	// 5. Absolute timestamp after event 2: delivers 0 events
	tAfter := now.Add(5 * time.Minute).Format(time.RFC3339)
	outAfter, stderrAfter, codeAfter := runTool(t, workDir, "", "--since", tAfter, "-t", "jsonl", "bus:events")
	if codeAfter != 0 || stderrAfter != "" {
		t.Fatalf("code=%d stderr=%q", codeAfter, stderrAfter)
	}
	dataAfter, _ := deltaCursor(t, outAfter)
	if dataAfter != "" {
		t.Fatalf("expected empty data for future timestamp, got %q", dataAfter)
	}
}

func TestBusDeltaTimeRetentionExpiry(t *testing.T) {
	isolateRoom(t)

	now := time.Now().UTC().Truncate(time.Second)
	tOld := now.Add(-2 * time.Hour)
	tNew := now.Add(-5 * time.Minute)

	if err := room.Emit(room.Event{Type: room.EventSprint, Actor: "mgr", Target: "sprint:446", TS: tOld.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}
	if err := room.Emit(room.Event{Type: room.EventTodo, Actor: "agent1", Target: "todo:13778c0d7a83", TS: tNew.Format(time.RFC3339)}); err != nil {
		t.Fatal(err)
	}

	// Force archive of event 1 by rotating timeline
	t.Setenv("BASHY_ROOM_RETENTION", "1h")
	if _, err := room.RotateTimeline(now); err != nil {
		t.Fatal(err)
	}

	workDir := t.TempDir()

	// If floor > 0 and requested time is before oldest live event, ErrDeltaExpired is returned
	tBeforeOld := now.Add(-3 * time.Hour).Format(time.RFC3339)
	out, stderr, code := runTool(t, workDir, "", "--since", tBeforeOld, "-t", "jsonl", "bus:events")
	if code == 0 || out != "" || !strings.Contains(stderr, "delta position expired") || !strings.Contains(stderr, "restart with --since=start") {
		t.Fatalf("expected retention expiration guidance: code=%d out=%q stderr=%q", code, out, stderr)
	}
}

// Event time and append sequence are independent (e.g. a delayed producer).
func TestBusDeltaOutOfOrderTime(t *testing.T) {
	isolateRoom(t)
	now := time.Now().UTC()
	for _, age := range []time.Duration{5 * time.Minute, 30 * time.Minute, time.Minute} {
		if err := room.Emit(room.Event{Type: room.EventSprint, TS: now.Add(-age).Format(time.RFC3339Nano)}); err != nil {
			t.Fatal(err)
		}
	}
	out, stderr, code := runTool(t, t.TempDir(), "", "--since=10m", "-t", "jsonl", "bus:events")
	if code != 0 {
		t.Fatal(stderr)
	}
	data, cursor := deltaCursor(t, out)
	lines := strings.Split(data, "\n")
	if len(lines) != 2 {
		t.Fatalf("want two recent events: %s", data)
	}
	for i, seq := range []float64{1, 3} {
		var row map[string]any
		if err := json.Unmarshal([]byte(lines[i]), &row); err != nil || row["seq"] != seq {
			t.Fatalf("row=%v err=%v", row, err)
		}
	}
	out, stderr, code = runTool(t, t.TempDir(), "", "--since", cursor, "-t", "jsonl", "bus:events")
	if code != 0 {
		t.Fatal(stderr)
	}
	if data, _ := deltaCursor(t, out); data != "" {
		t.Fatalf("replayed: %s", data)
	}
}
