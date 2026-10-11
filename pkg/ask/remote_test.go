package ask

import (
	"errors"
	"strings"
	"testing"
)

var testRemotePolicy = RemotePolicy{Approvers: []string{"qiangli"}}

func remoteDirective(r Request, mutate ...func(*RemoteDirective)) RemoteDirective {
	d := RemoteDirective{
		ID: "dir-1", Approval: r.ID, Host: "hostA", Token: r.Approval.Token,
		Verdict: VerdictAllow, By: "person:qiangli@hostB", TraceID: "trace-1",
	}
	for _, m := range mutate {
		m(&d)
	}
	return d
}

// A directive from an allowed person carrying the exact token settles the
// approval once, through Decide, and a resume on the owner host redeems it
// exactly as a local allow.
func TestApplyRemoteSettlesOnceWithTheSameToken(t *testing.T) {
	isolateApprovals(t)
	argv := []string{"make", "deploy"}
	r, err := Raise(ApprovalSpec{Source: SourceConfirm, Session: "card-a", Argv: argv})
	if err != nil {
		t.Fatal(err)
	}
	rc, err := ApplyRemote(remoteDirective(r), testRemotePolicy)
	if err != nil || !rc.Applied() || rc.Decision == nil || rc.Decision.Via != ViaRemote || rc.Token != r.Approval.Token || rc.TraceID != "trace-1" {
		t.Fatalf("apply: rc=%+v err=%v", rc, err)
	}
	if !strings.Contains(rc.Decision.By, "person:qiangli@hostB") {
		t.Fatalf("decision must name the remote approver, got %q", rc.Decision.By)
	}
	d, ok := Resolve(SourceConfirm, r.Approval.Token, "card-a", argv)
	if !ok || !d.Allowed() {
		t.Fatalf("the owner's resume must redeem the remote allow: %+v %v", d, ok)
	}
	var remote bool
	for _, e := range approvalEvents(t) {
		if e.Topic == "remote" && strings.Contains(e.Body, "dir-1") && strings.Contains(e.Body, "trace-1") {
			remote = true
		}
	}
	if !remote {
		t.Fatal("an applied remote directive must leave a room event carrying its id and trace")
	}

	// Replay: the same directive (or any other) cannot settle it again.
	rc, err = ApplyRemote(remoteDirective(r), testRemotePolicy)
	if !errors.Is(err, ErrRemoteRejected) || rc.Applied() || !strings.Contains(rc.Reason, "already been decided") {
		t.Fatalf("replay must be rejected: rc=%+v err=%v", rc, err)
	}
	rc, err = ApplyRemote(remoteDirective(r, func(d *RemoteDirective) { d.ID = "dir-2"; d.Verdict = VerdictDeny }), testRemotePolicy)
	if err == nil || rc.Applied() {
		t.Fatalf("a second directive must not overturn the first: rc=%+v", rc)
	}
	if got, _ := LoadDecision(r.ID); got.Verdict != VerdictAllow {
		t.Fatalf("decision changed under replay: %+v", got)
	}
}

func TestApplyRemoteFailsClosed(t *testing.T) {
	isolateApprovals(t)
	r, err := Raise(ApprovalSpec{Source: SourceConfirm, Argv: []string{"rm", "x"}})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*RemoteDirective){
		"token mismatch":  func(d *RemoteDirective) { d.Token = "00000000" },
		"no token":        func(d *RemoteDirective) { d.Token = "" },
		"prefix id":       func(d *RemoteDirective) { d.Approval = r.ID[:8] },
		"traversal id":    func(d *RemoteDirective) { d.Approval = "../../etc" },
		"unknown id":      func(d *RemoteDirective) { d.Approval = strings.Repeat("0", 32) },
		"agent approver":  func(d *RemoteDirective) { d.By = "plinth@hostB" },
		"other person":    func(d *RemoteDirective) { d.By = "person:mallory@hostB" },
		"hostless seat":   func(d *RemoteDirective) { d.By = "person:qiangli" },
		"bad verdict":     func(d *RemoteDirective) { d.Verdict = "maybe" },
		"no directive id": func(d *RemoteDirective) { d.ID = "" },
	}
	for name, m := range cases {
		rc, err := ApplyRemote(remoteDirective(r, m), testRemotePolicy)
		if !errors.Is(err, ErrRemoteRejected) || rc.Applied() || rc.Status != RemoteRejected || rc.Reason == "" {
			t.Errorf("%s: want a rejected receipt, got rc=%+v err=%v", name, rc, err)
		}
	}
	if _, err := ApplyRemote(remoteDirective(r), RemotePolicy{}); err == nil {
		t.Error("an empty policy must admit nobody")
	}
	if _, ok := LoadDecision(r.ID); ok {
		t.Fatal("a rejected directive must not decide the approval")
	}
	// A full seat in the policy admits that seat only.
	seat := RemotePolicy{Approvers: []string{"person:qiangli@hostB"}}
	if _, err := ApplyRemote(remoteDirective(r, func(d *RemoteDirective) { d.By = "person:qiangli@hostC" }), seat); err == nil {
		t.Error("a seat policy must not admit the same name on another host")
	}
	if rc, err := ApplyRemote(remoteDirective(r, func(d *RemoteDirective) { d.Verdict = VerdictDeny }), seat); err != nil || rc.Decision.Verdict != VerdictDeny {
		t.Fatalf("seat policy deny: rc=%+v err=%v", rc, err)
	}
}
