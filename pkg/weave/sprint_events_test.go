package weave

import (
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

// Every thread kind reaches the host event log, not only stage.
func TestWeaveStoryAppendEmitsEveryKindToHostLog(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	prev := announceEnabled
	announceEnabled = false
	t.Cleanup(func() { announceEnabled = prev })
	s := &weaveStory{ID: 418}
	for _, kind := range []string{"note", "decision", "progress", kindStage} {
		weaveStoryAppend(s, "mgr", kind, "body for "+kind)
	}
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	for i, e := range events {
		if e.Type != room.EventSprint || e.Target != "sprint:#418" || e.Actor != "mgr" || e.Seq != int64(i+1) {
			t.Fatalf("event %d = %+v", i, e)
		}
	}
	if events[1].Topic != "decision" || events[3].Topic != kindStage {
		t.Fatalf("kinds not carried in Topic: %+v", events)
	}
}
