package bus

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

func runEvents(t *testing.T, args ...string) []map[string]any {
	t.Helper()
	cmd := NewEventsCmd()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(append(args, "--json"))
	if err := cmd.Execute(); err != nil {
		t.Fatalf("events %v: %v", args, err)
	}
	var lines []map[string]any
	sc := bufio.NewScanner(strings.NewReader(out.String()))
	for sc.Scan() {
		var m map[string]any
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			t.Fatalf("bad line %q: %v", sc.Text(), err)
		}
		lines = append(lines, m)
	}
	return lines
}

func TestEventsShowsEveryDomainTypeWithContiguousSeq(t *testing.T) {
	isolate(t)
	room.Record(room.EventSprint, "mgr", "sprint:#418", "decision", "comment")
	room.Record(room.EventHandoff, "a", "handoff:h", "claimed", "brief")
	room.Record(room.EventTodo, "a", "todo:abc", "done", "done: x")
	lines := runEvents(t)
	if len(lines) != 3 {
		t.Fatalf("got %d lines: %v", len(lines), lines)
	}
	for i, want := range []string{"sprint", "handoff", "todo"} {
		if lines[i]["type"] != want || lines[i]["seq"] != float64(i+1) || lines[i]["schema_version"] != EventsSchemaVersion {
			t.Fatalf("line %d = %v", i, lines[i])
		}
	}
	if got := runEvents(t, "--kind", "todo,handoff"); len(got) != 2 {
		t.Fatalf("--kind: %v", got)
	}
	if got := runEvents(t, "--since", "2"); len(got) != 1 || got[0]["type"] != "todo" {
		t.Fatalf("--since 2: %v", got)
	}
}

func TestEventsSincePastHeadResyncs(t *testing.T) {
	isolate(t)
	room.Record(room.EventNote, "a", "", "", "x")
	got := runEvents(t, "--since", "50")
	if len(got) != 1 || got[0]["type"] != "resync" || got[0]["seq"] != float64(1) || got[0]["reason"] != "rebase" {
		t.Fatalf("got %v", got)
	}
}

func TestEventsFollowPicksUpLaterEvents(t *testing.T) {
	isolate(t)
	room.Record(room.EventNote, "a", "", "", "before")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := NewEventsCmd()
	pr := &chanWriter{ch: make(chan string, 8)}
	cmd.SetOut(pr)
	cmd.SetArgs([]string{"--follow", "--json", "--interval", "20ms"})
	cmd.SetContext(ctx)
	go cmd.Execute()
	time.Sleep(150 * time.Millisecond)
	room.Record(room.EventTodo, "a", "todo:z", "done", "later")
	select {
	case s := <-pr.ch:
		if !strings.Contains(s, `"type":"todo"`) || !strings.Contains(s, `"seq":2`) {
			t.Fatalf("got %s", s)
		}
	case <-ctx.Done():
		t.Fatal("follow never delivered the new event")
	}
}

type chanWriter struct{ ch chan string }

func (c *chanWriter) Write(p []byte) (int, error) { c.ch <- string(p); return len(p), nil }
