package weave

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

func holderOf(s string) *string { return &s }

func TestAddressedToIsSeatOrLateBoundRole(t *testing.T) {
	me := "plinth@dragon"
	cases := []struct {
		to     string
		holder *string
		want   bool
	}{
		{"plinth@dragon", nil, true},
		{"PLINTH@dragon", nil, true},
		{"plinth", nil, true},
		{"rafter@dragon", nil, false},
		{"plinth@noviwin1", nil, false}, // same name, other host: a different seat
		{"", holderOf("plinth@dragon"), true},
		{"", holderOf("rafter@dragon"), false},
		{"", nil, false},
		{"conductor:217", holderOf("plinth@dragon"), true},
		{"conductor:217", holderOf("rafter@dragon"), false},
		{"conductor", holderOf("plinth@dragon"), true},
	}
	for _, c := range cases {
		if got := addressedTo(c.to, "plinth", me, c.holder); got != c.want {
			t.Errorf("addressedTo(%q, holder=%v) = %v, want %v", c.to, c.holder, got, c.want)
		}
	}
}

func messageEvent(id, from, to, body string) Event {
	d, _ := json.Marshal(messageDetail{Schema: bus.BoardSchema, ID: id, From: from, To: to, Topic: "mb"})
	return Event{ID: "ev-" + id, Kind: MessageEventKind, Summary: body, Detail: d}
}

func TestDeliverRemoteMailFilesOnceUnderTheUUID(t *testing.T) {
	const id = "0192f3a4-7c1e-7000-8000-000000000001"
	fake := &fakeSessionClient{
		tasks: []TaskSummary{{ID: "task-1", TargetRepo: "github.com/qiangli/bashy", Status: "active"}},
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	me, _ := SessionParticipant()
	feed := []Event{
		messageEvent(id, "rafter@noviwin1", me, "rebase first"),
		messageEvent("0192f3a4-7c1e-7000-8000-000000000002", "rafter@noviwin1", "someone-else@noviwin1", "not mine"),
		messageEvent("0192f3a4-7c1e-7000-8000-000000000003", me, "rafter@noviwin1", "my own outbox copy"),
		{ID: "ev-note", Kind: "note", Summary: "a plain note"},
	}
	// Two polls return the same batch: a redelivery must be a no-op.
	fake.eventsPolls = []EventsResponse{{Events: feed, Cursor: "c1"}, {Events: feed, Cursor: "c1"}}

	rep, err := DeliverRemoteMail(context.Background(), repo, "plinth")
	if err != nil {
		t.Fatal(err)
	}
	if rep.Delivered != 1 || rep.Cursor != "c1" || rep.Session != "task-1" || rep.Skipped != "" {
		t.Fatalf("report = %+v", rep)
	}
	posts, _ := bus.Posts()
	if len(posts) != 1 || posts[0].ID != id || posts[0].To != "plinth" || posts[0].From != "rafter@noviwin1" || posts[0].Body != "rebase first" || posts[0].IdempotencyKey != id {
		t.Fatalf("board = %+v", posts)
	}
	directed, _, _, _ := bus.Unseen("plinth", 0)
	if len(directed) != 1 {
		t.Fatalf("the delivered message must read as DIRECTED mail for plinth, got %d", len(directed))
	}

	rep2, err := DeliverRemoteMail(context.Background(), repo, "plinth")
	if err != nil {
		t.Fatal(err)
	}
	posts, _ = bus.Posts()
	if len(posts) != 1 {
		t.Fatalf("redelivery filed a duplicate: %d posts", len(posts))
	}
	_ = rep2
	if cp, _ := feedCursorPath(repo, "task-1", "plinth"); readFeedCursor(cp) != "c1" {
		t.Fatalf("feed cursor not persisted")
	}
}

func TestDeliverRemoteMailSkipsSilentlyWhenUnpaired(t *testing.T) {
	fake := &fakeSessionClient{}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv("CLOUDBOX_TOKEN", "")
	t.Setenv("BASHY_FLEET_TOKEN", "")
	t.Setenv("BASHY_API_KEY", "")
	t.Setenv("PATH", t.TempDir())
	credCache.mu.Lock()
	credCache.m = map[string]credEntry{}
	credCache.mu.Unlock()
	rep, err := DeliverRemoteMail(context.Background(), repo, "plinth")
	if err != nil || rep.Skipped == "" || rep.Delivered != 0 {
		t.Fatalf("unpaired must skip silently: rep=%+v err=%v", rep, err)
	}
}
