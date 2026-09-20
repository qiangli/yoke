package weave

import (
	"context"
	"encoding/json"
	"strings"
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
		{"codex@dragon", nil, false},    // another seat on this host
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
	// The pass may run under ANOTHER identity on the host (a live session's
	// wrapper relaying for its agent as the person): mail to `<reader>@<host>`
	// is still the reader's.
	person := "person:qiangli@dragon"
	if !addressedTo("codex-gpt5.6-sol@dragon", "codex-gpt5.6-sol", person, nil) {
		t.Fatal("mail to the reader's seat address was not the reader's when the pass ran as the person")
	}
	if addressedTo("codex-gpt5.6-sol@noviwin1", "codex-gpt5.6-sol", person, nil) {
		t.Fatal("the same name on another host is a different seat")
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
	if len(posts) != 1 || posts[0].ID != id || posts[0].To != "plinth" || posts[0].From != "rafter@noviwin1" || posts[0].Body != "rebase first" || posts[0].IdempotencyKey != id+"|plinth" {
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

// One host, two readers, one message (sprint 220): a feed event a second
// seat also reads must file for it too, not abort the pass with "notice key
// reused with different content" — the once-key is per message AND reader.
func TestDeliverRemoteMailFilesForASecondReaderOnTheSameHost(t *testing.T) {
	const id = "0192f3a4-7c1e-7000-8000-000000000009"
	fake := &fakeSessionClient{
		tasks: []TaskSummary{{ID: "task-1", TargetRepo: "github.com/qiangli/bashy", Status: "active"}},
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	me, _ := SessionParticipant()
	// Addressed to nobody in particular: it is the lease holder's — and this
	// host holds the lease — so every reader here files it.
	fake.tasks[0].LeaseHolder = &me
	feed := []Event{messageEvent(id, "rafter@noviwin1", "", "who has the lease?")}
	fake.eventsPolls = []EventsResponse{{Events: feed, Cursor: "c1"}, {Events: feed, Cursor: "c1"}}
	if rep, err := DeliverRemoteMail(context.Background(), repo, "plinth"); err != nil || rep.Delivered != 1 {
		t.Fatalf("first reader: %+v %v", rep, err)
	}
	if rep, err := DeliverRemoteMail(context.Background(), repo, "keel"); err != nil || rep.Delivered != 1 {
		t.Fatalf("second reader on the same host: %+v %v", rep, err)
	}
	posts, _ := bus.Posts()
	if len(posts) != 2 || posts[0].ID != id || posts[1].ID != id || posts[0].To == posts[1].To {
		t.Fatalf("board = %+v", posts)
	}
}

// `<name>@<host>` reaches a rostered PERSON (rostered as `person:<name>@<host>`)
// — the spelling the identity rule teaches; the prefix is the wire's, not the
// sender's (sprint 220).
func TestResolveRemoteParticipantReachesAPersonWithoutThePrefix(t *testing.T) {
	fake := &fakeSessionClient{
		tasks: []TaskSummary{{ID: "task-1", TargetRepo: "github.com/qiangli/bashy", Status: "active"}},
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	join := func(p string) Event {
		d, _ := json.Marshal(map[string]string{"participant": p})
		return Event{ID: "ev-" + p, Kind: "join", Summary: p + " joined", Detail: d}
	}
	fake.eventsPolls = []EventsResponse{{Events: []Event{join("person:noviadmin@winbox"), join("rafter@winbox")}}}
	for _, target := range []string{"noviadmin@winbox", "person:noviadmin@winbox", "noviadmin"} {
		fake.eventsPolls = []EventsResponse{{Events: []Event{join("person:noviadmin@winbox"), join("rafter@winbox")}}}
		route, err := ResolveRemoteParticipant(context.Background(), repo, target)
		if err != nil || route.Participant != "person:noviadmin@winbox" || route.Host != "winbox" {
			t.Fatalf("%q → %+v, %v", target, route, err)
		}
	}
}

// Every SEAT joins the session, not just the first verb run in a checkout:
// a live agent session beside the person on the same host must reach the
// roster or nobody on another host can address it (sprint 220).
func TestEnsureRepoSessionJoinsEachSeat(t *testing.T) {
	fake := &fakeSessionClient{
		tasks: []TaskSummary{{ID: "task-1", TargetRepo: "github.com/qiangli/bashy", Status: "active"}},
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	if _, err := EnsureRepoSession(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	first := len(fake.joins)
	if first == 0 {
		t.Fatal("the first seat did not join")
	}
	// Same seat again: no second join.
	if _, err := EnsureRepoSession(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if len(fake.joins) != first {
		t.Fatalf("a known seat joined again: %d → %d", first, len(fake.joins))
	}
	// A second seat in the same checkout — the live agent — joins too.
	// (Clear the higher-precedence identity envs the developer's shell may
	// carry, so the seat under test is the one BASHY_AGENT names.)
	for _, k := range []string{"BASHY_PRINCIPAL", "WEAVE_CONDUCTOR", "BASHY_AGENT_ID"} {
		t.Setenv(k, "")
	}
	t.Setenv("BASHY_AGENT", "codex-gpt5.6-sol")
	if _, err := EnsureRepoSession(context.Background(), repo); err != nil {
		t.Fatal(err)
	}
	if len(fake.joins) != first+1 || !strings.HasPrefix(fake.joins[first].Participant, "codex-gpt5.6-sol@") {
		t.Fatalf("second seat: joins = %+v", fake.joins)
	}
	if _, err := EnsureRepoSession(context.Background(), repo); err != nil || len(fake.joins) != first+1 {
		t.Fatalf("second seat joined twice: %v %d", err, len(fake.joins))
	}
}
