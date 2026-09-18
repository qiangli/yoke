package bus

import (
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func TestInvalidActivityDoesNotAppendTimeline(t *testing.T) {
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	err := Publish(Notification{
		Principal: "weave", To: "owner", Body: "short",
		Activity: &room.Activity{ID: "bad", Version: 1, Actor: "weave"},
	})
	if err == nil {
		t.Fatal("Publish accepted an incomplete activity")
	}
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("invalid activity appended %d timeline events", len(events))
	}
}
