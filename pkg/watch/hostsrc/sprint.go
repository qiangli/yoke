package hostsrc

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/watch"
)

// SprintSource watches sprint threads.
//
// It reads the ROOM TIMELINE rather than the sprint store, and that is the
// load-bearing choice. Every thread entry already passes through one function
// in the sprint store, which records it on the host event log (what
// `bashy events` reads); the board announcement is a projection of the same
// hook. So a watcher on the timeline cannot disagree with the board about what
// happened, a stage change cannot reach one without reaching the other, and a
// watcher started late still drains the entries it missed — none of which is
// true of a watcher that re-reads the card and diffs it.
type SprintSource struct {
	Read Timeline
	// Sprint narrows to one card ("480" or "#480"). Empty watches every card.
	Sprint string
	// Backfill reports entries already on the log that are younger than this.
	Backfill time.Duration
	// Tail is how much of the timeline each poll reads.
	Tail int

	started bool
	lastSeq int64
}

// NewSprintSource builds the sprint-thread source.
func NewSprintSource(read Timeline) *SprintSource {
	if read == nil {
		read = room.Timeline
	}
	return &SprintSource{Read: read, Tail: 400}
}

// Name implements watch.Source.
func (s *SprintSource) Name() string { return "sprint-thread" }

// BackfillWindow implements watch.Backfiller, so an invocation can announce
// how much of the past its first lines describe.
func (s *SprintSource) BackfillWindow() time.Duration { return s.Backfill }

// Poll implements watch.Source.
func (s *SprintSource) Poll(_ context.Context, now time.Time) ([]watch.Event, error) {
	tail := s.Tail
	if tail <= 0 {
		tail = 400
	}
	events, err := s.Read(tail)
	if err != nil {
		return nil, fmt.Errorf("event log unreadable: %w", err)
	}

	want := normalizeSprint(s.Sprint)
	var out []watch.Event
	for _, e := range events {
		if e.Seq <= s.lastSeq {
			continue
		}
		s.lastSeq = e.Seq
		if e.Type != room.EventSprint {
			continue
		}
		locator := sprintLocator(e.Target)
		if want != "" && locator != "sprint:"+want {
			continue
		}
		at := eventTime(e, now)
		if !s.started && now.Sub(at) > s.Backfill {
			continue
		}
		raw, _ := json.Marshal(e)
		out = append(out, watch.Event{
			At: at, Source: "sprint-thread", Kind: watch.KindAdded, Locator: locator,
			Key: strconv.FormatInt(e.Seq, 10), Field: "thread",
			After: e.Topic,
			Text:  watch.SanitizeText([]byte(threadLine(e))),
			Attrs: map[string]string{
				"kind": e.Topic, "author": e.Actor, "target": e.Target,
				"seq": strconv.FormatInt(e.Seq, 10),
			},
			Raw: raw,
		})
	}
	s.started = true
	return out, nil
}

// sprintLocator normalizes the room target ("sprint:#480") to the lens locator
// spelling the rest of the vocabulary uses ("sprint:480"), so one rule file can
// name a sprint the same way everywhere.
func sprintLocator(target string) string {
	t := strings.TrimSpace(target)
	if t == "" {
		return "sprint"
	}
	if rest, ok := strings.CutPrefix(t, "sprint:"); ok {
		return "sprint:" + strings.TrimPrefix(strings.TrimSpace(rest), "#")
	}
	return t
}

func normalizeSprint(s string) string {
	return strings.TrimPrefix(strings.TrimSpace(s), "#")
}

func threadLine(e room.Event) string {
	kind := e.Topic
	if kind == "" {
		kind = "note"
	}
	who := e.Actor
	if who == "" {
		who = "unattributed"
	}
	return fmt.Sprintf("%s by %s: %s", kind, who, firstLine(e.Body, 200))
}

func eventTime(e room.Event, fallback time.Time) time.Time {
	if t, err := time.Parse(time.RFC3339, strings.TrimSpace(e.TS)); err == nil {
		return t
	}
	if t, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(e.TS)); err == nil {
		return t
	}
	return fallback
}
