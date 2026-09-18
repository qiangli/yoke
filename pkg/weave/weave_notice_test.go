package weave

import (
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

func TestOwnerNoticeIsDurableDeduplicatedAndRedacted(t *testing.T) {
	dir, root := newQueueInTempRepo(t)
	q := &weaveQueue{Root: root, Items: []*weaveItem{{ID: 7, Owner: "worker", State: "submitted", Head: "abc123"}}}
	if err := saveConductorLock(dir, &ConductorLock{Holder: "accountable-conductor", HeartbeatAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	weaveQueueOwnerNotice(dir, q, q.Items[0], "run-terminal")
	if len(q.OwnerNotices) != 1 || q.OwnerNotices[0].Owner != "accountable-conductor" {
		t.Fatalf("notice owner = %#v", q.OwnerNotices)
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	weaveDeliverOwnerNotices(dir)
	weaveDeliverOwnerNotices(dir) // replay must not duplicate the durable event.
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	var notices []room.Event
	for _, e := range events {
		if e.Activity != nil {
			notices = append(notices, e)
		}
	}
	if len(notices) != 1 {
		t.Fatalf("owner notices = %d, want one: %#v", len(notices), notices)
	}
	got := notices[0]
	if got.Body != "weave run #7 submitted" || got.MatchReason != "owner" {
		t.Fatalf("visible notice = %#v", got)
	}
	a := got.Activity
	if a.ID == "" || a.Version != 1 || a.Actor != "worker" || a.Verb != "completed" || a.Noun != "run" || a.ObjectRef != "run:7" || a.Repo != root || a.FetchRef != "weave:run:7" {
		t.Fatalf("activity = %#v", a)
	}
}

func TestOwnerNoticeNoOwnerIsNoop(t *testing.T) {
	dir, root := newQueueInTempRepo(t)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	q := &weaveQueue{Root: root, Items: []*weaveItem{{ID: 8, State: "submitted"}}}
	weaveQueueOwnerNotice(dir, q, q.Items[0], "run-terminal")
	if len(q.OwnerNotices) != 0 {
		t.Fatalf("unowned no-op queued notices: %#v", q.OwnerNotices)
	}
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	weaveDeliverOwnerNotices(dir)
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("unowned no-op emitted %d timeline events", len(events))
	}
}
