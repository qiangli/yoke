package weave

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/ask"
)

// feedOf turns what a host appended into what the other host's feed returns.
func feedOf(reqs []AppendEventReq, ids ...string) []Event {
	out := make([]Event, len(reqs))
	for i, r := range reqs {
		out[i] = Event{ID: ids[i], Author: "owner@example.test", Kind: r.Kind, Summary: r.Summary, Detail: r.Detail}
	}
	return out
}

// The two-host shape over one fake repo-session feed: B sends a directive for
// an approval queued on A, A's drain applies it with the same token and posts
// a receipt, B reads the receipt; a replay of the directive and a mismatched
// token are rejected with explicit receipts and change nothing.
func TestRemoteApprovalDirectiveAppliesOnOwnerAndReplaysFailClosed(t *testing.T) {
	fake := &fakeSessionClient{
		tasks:        []TaskSummary{{ID: "task-1", Owner: "owner@example.test", TargetRepo: "github.com/qiangli/bashy", Status: "active"}},
		rosterEvents: []Event{{Kind: "join", Author: "owner@example.test", Detail: json.RawMessage(`{"participant":"person:qiangli@hostB"}`)}},
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("USER", "qiangli")
	t.Setenv(RemoteApproversEnv, "")
	ownerHost := sessionHostName()

	// Host A: an approval waits in the local queue.
	argv := []string{"make", "deploy"}
	r, err := ask.Raise(ask.ApprovalSpec{Source: ask.SourceConfirm, Session: "card-a", Argv: argv})
	if err != nil {
		t.Fatal(err)
	}

	// Host B: the operator answers it remotely.
	sent, err := SendApprovalDirective(context.Background(), repo, ask.RemoteDirective{
		Approval: r.ID, Host: ownerHost, Token: r.Approval.Token, Verdict: ask.VerdictAllow,
		By: "person:qiangli@hostB", ByURN: "forged-client-urn", TraceID: "trace-9",
	})
	if err != nil || sent.ID == "" || sent.At == "" {
		t.Fatalf("send: %+v %v", sent, err)
	}
	if len(fake.appends) != 1 || fake.appends[0].Kind != ApprovalDirectiveKind || len(ApprovalReceiptKind) > 16 {
		t.Fatalf("directive append = %+v", fake.appends)
	}
	replay := fake.appends[0]
	other := ask.RemoteDirective{ID: "dir-x", Approval: r.ID, Host: "elsewhere", Token: r.Approval.Token, Verdict: ask.VerdictDeny, By: "person:qiangli@hostB"}
	otherDetail, _ := json.Marshal(other)

	// Host A drains: the directive (delivered twice by the feed) and another
	// host's directive it must ignore.
	feed := append(feedOf(fake.appends, "ev-1"), Event{ID: "ev-other", Kind: ApprovalDirectiveKind, Detail: otherDetail})
	fake.appends = nil
	fake.eventsPolls = []EventsResponse{{Events: feed, Cursor: "c1"}, {Events: feed, Cursor: "c1"}}
	rep, err := DeliverRemoteMail(context.Background(), repo, "plinth")
	if err != nil || rep.Approvals != 1 {
		t.Fatalf("owner drain: %+v %v", rep, err)
	}
	if rep, err := DeliverRemoteMail(context.Background(), repo, "keel"); err != nil || rep.Approvals != 0 {
		t.Fatalf("a feed redelivery to a second reader must not re-answer: %+v %v", rep, err)
	}
	if len(fake.appends) != 1 || fake.appends[0].Kind != ApprovalReceiptKind {
		t.Fatalf("want exactly one receipt, got %+v", fake.appends)
	}
	d, ok := ask.LoadDecision(r.ID)
	if !ok || !d.Allowed() || d.Via != ask.ViaRemote || !strings.Contains(d.By, "person:qiangli@hostB") ||
		!strings.Contains(d.By, "owner@example.test") || strings.Contains(d.By, "forged-client-urn") {
		t.Fatalf("A's decision = %+v %v", d, ok)
	}
	if got, ok := ask.Resolve(ask.SourceConfirm, r.Approval.Token, "card-a", argv); !ok || !got.Allowed() {
		t.Fatal("A's resume with the same token must redeem the remote allow")
	}

	// Host B reads the receipt for its directive.
	fake.eventsPolls = []EventsResponse{{Events: feedOf(fake.appends, "ev-2"), Cursor: "c2"}}
	rc, found, err := FindApprovalReceipt(context.Background(), repo, sent.ID)
	if err != nil || !found || !rc.Applied() || rc.Token != r.Approval.Token || rc.Host != ownerHost || rc.TraceID != "trace-9" {
		t.Fatalf("B's receipt: %+v found=%v err=%v", rc, found, err)
	}

	// A replay of the same directive in a new event, and a forged token, are
	// both answered with a rejection and leave the decision alone.
	forged := ask.RemoteDirective{ID: "dir-forged", Approval: r.ID, Host: ownerHost, Token: "00000000", Verdict: ask.VerdictDeny, By: "person:qiangli@hostB"}
	forgedDetail, _ := json.Marshal(forged)
	fake.appends = nil
	fake.eventsPolls = []EventsResponse{{Events: []Event{
		{ID: "ev-3", Author: "owner@example.test", Kind: replay.Kind, Detail: replay.Detail},
		{ID: "ev-4", Author: "owner@example.test", Kind: ApprovalDirectiveKind, Detail: forgedDetail},
	}, Cursor: "c3"}}
	if rep, err := DeliverRemoteMail(context.Background(), repo, "plinth"); err != nil || rep.Approvals != 2 {
		t.Fatalf("replay drain: %+v %v", rep, err)
	}
	for _, a := range fake.appends {
		var rc ask.RemoteReceipt
		_ = json.Unmarshal(a.Detail, &rc)
		if a.Kind != ApprovalReceiptKind || rc.Applied() || rc.Status != ask.RemoteRejected {
			t.Fatalf("replay/forgery must be rejected explicitly: %+v", rc)
		}
	}
	if d2, _ := ask.LoadDecision(r.ID); d2.Verdict != ask.VerdictAllow || !d2.At.Equal(d.At) {
		t.Fatalf("decision changed: %+v", d2)
	}
}

func TestRemoteApprovalRejectsForgedSeatWithCorrectToken(t *testing.T) {
	const owner = "owner@example.test"
	fake := &fakeSessionClient{
		tasks:        []TaskSummary{{ID: "task-1", Owner: owner, TargetRepo: "github.com/qiangli/bashy", Status: "active"}},
		rosterEvents: []Event{{Kind: "join", Author: "attacker@example.test", Detail: json.RawMessage(`{"participant":"person:qiangli@hostB"}`)}},
	}
	repo := sessionTestEnv(t, "git@github.com:qiangli/bashy.git", fake)
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv(RemoteApproversEnv, "")
	r, err := ask.Raise(ask.ApprovalSpec{Source: ask.SourceConfirm, Session: "forged-seat", Argv: []string{"make", "deploy"}})
	if err != nil {
		t.Fatal(err)
	}
	d := ask.RemoteDirective{ID: "forged-seat", Approval: r.ID, Host: sessionHostName(), Token: r.Approval.Token,
		Verdict: ask.VerdictAllow, By: "person:qiangli@hostB", ByURN: owner}
	detail, _ := json.Marshal(d)
	sc, err := EnsureRepoSession(context.Background(), repo)
	if err != nil {
		t.Fatal(err)
	}
	for i, author := range []string{"attacker@example.test", ""} {
		applied, err := applyApprovalEvent(context.Background(), sc, repo, "task-1",
			Event{ID: "forgery-" + string(rune('a'+i)), Kind: ApprovalDirectiveKind, Author: author, Detail: detail})
		if err != nil || !applied {
			t.Fatalf("forged event author=%q: receipt=%v err=%v", author, applied, err)
		}
		var receipt ask.RemoteReceipt
		if err := json.Unmarshal(fake.appends[i].Detail, &receipt); err != nil || receipt.Status != ask.RemoteRejected {
			t.Fatalf("forged event author=%q receipt=%+v err=%v", author, receipt, err)
		}
	}
	if _, decided := ask.LoadDecision(r.ID); decided {
		t.Fatal("forged account settled an approval despite the correct token and claimed person seat")
	}
}
