package watch

import (
	"context"
	"strings"
	"testing"
	"time"
)

// scriptRunner replays canned snapshots, one per poll.
func scriptRunner(t *testing.T, script ...[]any) Runner {
	t.Helper()
	i := 0
	return func(context.Context) ([]byte, int, error) {
		if i >= len(script) {
			t.Fatalf("runner called %d times, script has %d", i+1, len(script))
		}
		step := script[i]
		i++
		return []byte(step[0].(string)), step[1].(int), nil
	}
}

func snap(out string) []any { return []any{out, 0} }

func TestPollSourceFirstCycleIsBaseline(t *testing.T) {
	// A watcher started mid-flight must not alarm on the state it found: that is
	// the "replayed the whole history" failure, and it is the first cycle's job
	// to establish a baseline, not to report one.
	p := NewPollSource("poll", "sprint:480", scriptRunner(t,
		snap(`[{"id":"a","stage":"in"}]`),
		snap(`[{"id":"a","stage":"in"},{"id":"b","stage":"queued"}]`),
	))
	evs, err := p.Poll(context.Background(), at0)
	if err != nil || len(evs) != 0 {
		t.Fatalf("first cycle = (%+v, %v), want no events", evs, err)
	}
	evs, err = p.Poll(context.Background(), at0.Add(time.Minute))
	if err != nil || len(evs) != 1 || evs[0].Kind != KindAdded || evs[0].Key != "b" {
		t.Fatalf("second cycle = (%+v, %v), want one added b", evs, err)
	}
}

func TestPollSourceFailedSnapshotKeepsBaseline(t *testing.T) {
	p := NewPollSource("poll", "sprint:480", func() Runner {
		outs := []struct {
			out  string
			exit int
		}{
			{`[{"id":"a","stage":"in"}]`, 0},
			{"", 1},
			{`[{"id":"a","stage":"in"}]`, 0},
		}
		i := 0
		return func(context.Context) ([]byte, int, error) {
			o := outs[i]
			i++
			return []byte(o.out), o.exit, nil
		}
	}())
	if _, err := p.Poll(context.Background(), at0); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	evs, err := p.Poll(context.Background(), at0.Add(time.Minute))
	if err == nil {
		t.Fatal("a command that exited non-zero must be reported as a source error")
	}
	if len(evs) != 0 {
		t.Fatalf("failed snapshot produced events: %+v", evs)
	}
	// The baseline survived, so the recovered snapshot is a no-change.
	evs, err = p.Poll(context.Background(), at0.Add(2*time.Minute))
	if err != nil || len(evs) != 0 {
		t.Fatalf("recovered cycle = (%+v, %v), want no events", evs, err)
	}
}

func TestPollSourceEmptySnapshotIsNotEverythingRemoved(t *testing.T) {
	p := NewPollSource("poll", "mb", scriptRunner(t,
		snap(`[{"id":"a"},{"id":"b"}]`),
		snap(""),
	))
	if _, err := p.Poll(context.Background(), at0); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	evs, err := p.Poll(context.Background(), at0.Add(time.Minute))
	if err == nil || len(evs) != 0 {
		t.Fatalf("empty snapshot = (%+v, %v), want a source error and no events", evs, err)
	}

	// Opt in and it is read literally, for a command whose empty output really
	// does mean empty.
	q := NewPollSource("poll", "mb", scriptRunner(t,
		snap(`[{"id":"a"},{"id":"b"}]`),
		snap(""),
	))
	q.AllowEmpty = true
	if _, err := q.Poll(context.Background(), at0); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	evs, err = q.Poll(context.Background(), at0.Add(time.Minute))
	if err != nil || len(evs) != 2 {
		t.Fatalf("--allow-empty = (%+v, %v), want two removals", evs, err)
	}
	for _, e := range evs {
		if e.Kind != KindRemoved {
			t.Fatalf("kind = %q, want removed", e.Kind)
		}
	}
}

func TestPollSourceLineFallbackAndBinary(t *testing.T) {
	p := NewPollSource("poll", "@dev-box", scriptRunner(t,
		snap("peers: 2\n"),
		snap("peers: 2\nwarning: \x00\xfe truncated\n"),
	))
	if _, err := p.Poll(context.Background(), at0); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	evs, err := p.Poll(context.Background(), at0.Add(time.Minute))
	if err != nil {
		t.Fatalf("line fallback: %v", err)
	}
	if len(evs) != 1 || evs[0].Kind != KindAdded || !strings.Contains(evs[0].Text, "truncated") {
		t.Fatalf("events = %+v, want one added line", evs)
	}
	if strings.ContainsRune(evs[0].Text, 0) {
		t.Fatalf("raw NUL reached the event: %q", evs[0].Text)
	}
}
