package lenscmd

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/lens/stream"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/qiangli/yoke/pkg/room"
)

func TestParseSinceCursor_Start(t *testing.T) {
	loc := ref.Locator{Scheme: "bus", Target: "events"}
	now := time.Now()
	res, err := ParseSinceCursor("start", loc, "delta:jsonl", now, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Mode != SinceModeStart || res.Token != "" {
		t.Fatalf("res = %+v, want start with empty token", res)
	}
}

func TestParseSinceCursor_CursorEnvelope(t *testing.T) {
	loc := ref.Locator{Scheme: "bus", Target: "events"}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	// Valid cursor
	c := stream.Cursor{
		SchemaVersion: stream.CursorSchema,
		Kind:          stream.KindChangeStream,
		Token:         "123",
		Binding: stream.Binding{
			Provider: "bus",
			Target:   "bus:events",
			Query:    "delta:jsonl",
		},
	}
	data, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}

	res, err := ParseSinceCursor(string(data), loc, "delta:jsonl", now, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Mode != SinceModeCursor || res.Token != "123" {
		t.Fatalf("res = %+v, want cursor with token 123", res)
	}

	// File reference @file
	res, err = ParseSinceCursor("@/path/to/cursor.json", loc, "delta:jsonl", now, func(path string) ([]byte, error) {
		if path != "/path/to/cursor.json" {
			return nil, fmt.Errorf("unexpected path: %s", path)
		}
		return data, nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Mode != SinceModeCursor || res.Token != "123" {
		t.Fatalf("res = %+v, want cursor with token 123", res)
	}

	// Mismatched provider: must reject and not interchange provider tokens
	cProvider := c
	cProvider.Binding.Provider = "run"
	dataProv, _ := json.Marshal(cProvider)
	_, err = ParseSinceCursor(string(dataProv), loc, "delta:jsonl", now, nil)
	if err == nil || !strings.Contains(err.Error(), "cursor is bound to a different provider") {
		t.Fatalf("expected provider binding mismatch, got %v", err)
	}

	// Mismatched target
	cTarget := c
	cTarget.Binding.Target = "bus:other"
	dataTarget, _ := json.Marshal(cTarget)
	_, err = ParseSinceCursor(string(dataTarget), loc, "delta:jsonl", now, nil)
	if err == nil || !strings.Contains(err.Error(), "cursor is bound to a different provider") {
		t.Fatalf("expected target binding mismatch, got %v", err)
	}

	// Mismatched query
	cQuery := c
	cQuery.Binding.Query = "delta:txt"
	dataQuery, _ := json.Marshal(cQuery)
	_, err = ParseSinceCursor(string(dataQuery), loc, "delta:jsonl", now, nil)
	if err == nil || !strings.Contains(err.Error(), "cursor is bound to a different provider") {
		t.Fatalf("expected query binding mismatch, got %v", err)
	}

	// Expired cursor
	cExpired := c
	cExpired.Expiry = "2026-10-18T11:00:00Z"
	dataExpired, _ := json.Marshal(cExpired)
	_, err = ParseSinceCursor(string(dataExpired), loc, "delta:jsonl", now, nil)
	if err == nil || !strings.Contains(err.Error(), "cursor expired") {
		t.Fatalf("expected expired error, got %v", err)
	}

	// Non-expired cursor
	cValid := c
	cValid.Expiry = "2026-10-18T13:00:00Z"
	dataValid, _ := json.Marshal(cValid)
	res, err = ParseSinceCursor(string(dataValid), loc, "delta:jsonl", now, nil)
	if err != nil {
		t.Fatalf("unexpected error for unexpired cursor: %v", err)
	}
	if res.Token != "123" {
		t.Fatalf("token = %q, want 123", res.Token)
	}
}

func TestParseSinceCursor_Sequence(t *testing.T) {
	loc := ref.Locator{Scheme: "bus", Target: "events"}
	now := time.Now()

	for _, spec := range []string{"42", "#42", "seq:42"} {
		res, err := ParseSinceCursor(spec, loc, "delta:jsonl", now, nil)
		if err != nil {
			t.Fatalf("spec %q: unexpected error: %v", spec, err)
		}
		if res.Mode != SinceModeSeq || res.Seq != 42 || res.Token != "42" {
			t.Fatalf("spec %q: got %+v, want seq=42 token=42", spec, res)
		}
	}

	// Invalid sequences
	for _, bad := range []string{"0", "#0", "seq:0", "-1", "#-1", "seq:-1", "1.5", "seq:abc", "#xyz", "seq:9999999999999999999999999999999999999999"} {
		_, err := ParseSinceCursor(bad, loc, "delta:jsonl", now, nil)
		if err == nil {
			t.Errorf("bad sequence %q: expected error, got nil", bad)
		}
	}
}

func TestParseSinceCursor_Duration(t *testing.T) {
	loc := ref.Locator{Scheme: "bus", Target: "events"}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)

	res, err := ParseSinceCursor("10m", loc, "delta:jsonl", now, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Mode != SinceModeDuration || res.Duration != 10*time.Minute {
		t.Fatalf("res = %+v, want duration 10m", res)
	}
	expectedCutoff := now.Add(-10 * time.Minute)
	if !res.Time.Equal(expectedCutoff) {
		t.Fatalf("time = %v, want %v", res.Time, expectedCutoff)
	}

	// Negative or zero duration
	for _, bad := range []string{"0s", "-10m"} {
		_, err := ParseSinceCursor(bad, loc, "delta:jsonl", now, nil)
		if err == nil {
			t.Errorf("bad duration %q: expected error, got nil", bad)
		}
	}
}

func TestParseSinceCursor_Timestamp(t *testing.T) {
	loc := ref.Locator{Scheme: "bus", Target: "events"}
	now := time.Now()

	ts := "2026-10-18T17:42:52Z"
	res, err := ParseSinceCursor(ts, loc, "delta:jsonl", now, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res.Mode != SinceModeTime {
		t.Fatalf("res = %+v, want mode time", res)
	}
	if res.Time.Year() != 2026 || res.Time.Month() != 10 || res.Time.Day() != 18 {
		t.Fatalf("unexpected parsed time: %v", res.Time)
	}
}

func TestParseSinceCursor_InvalidSpecifications(t *testing.T) {
	loc := ref.Locator{Scheme: "bus", Target: "events"}
	now := time.Now()

	for _, invalid := range []string{"", "   ", "some_random_word", "{not-valid-json", "@"} {
		_, err := ParseSinceCursor(invalid, loc, "delta:jsonl", now, func(path string) ([]byte, error) {
			return nil, os.ErrNotExist
		})
		if err == nil {
			t.Errorf("expected error for %q, got nil", invalid)
		}
	}
}

func TestBusDeltaSinceSequenceShorthand(t *testing.T) {
	isolateRoom(t)

	room.Record(room.EventSprint, "mgr", "sprint:446", "decision", "event 1")
	room.Record(room.EventTodo, "agent1", "todo:13778c0d7a83", "doing", "event 2")
	room.Record(room.EventNote, "agent2", "note", "doing", "event 3")

	workDir := t.TempDir()

	// --since=2 should only deliver event 3 (seq > 2)
	for _, spec := range []string{"2", "#2", "seq:2"} {
		out, stderr, code := runTool(t, workDir, "", "--since", spec, "-t", "jsonl", "bus:events")
		if code != 0 || stderr != "" {
			t.Fatalf("spec=%q code=%d stderr=%q out=%q", spec, code, stderr, out)
		}

		data, cursor := deltaCursor(t, out)
		lines := strings.Split(data, "\n")
		if len(lines) != 1 {
			t.Fatalf("spec=%q expected 1 line, got %d:\n%s", spec, len(lines), data)
		}
		var row map[string]any
		if err := json.Unmarshal([]byte(lines[0]), &row); err != nil {
			t.Fatalf("invalid json: %v", err)
		}
		if int64(row["seq"].(float64)) != 3 {
			t.Errorf("spec=%q seq = %v, want 3", spec, row["seq"])
		}

		// Verify emitted trailing cursor can be used for next resume
		outNext, stderrNext, codeNext := runTool(t, workDir, "", "--since", cursor, "-t", "jsonl", "bus:events")
		if codeNext != 0 || stderrNext != "" {
			t.Fatalf("resume code=%d stderr=%q", codeNext, stderrNext)
		}
		dataNext, _ := deltaCursor(t, outNext)
		if dataNext != "" {
			t.Errorf("expected empty data after head, got %q", dataNext)
		}
	}
}
