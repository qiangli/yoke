// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package shape

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/shape/state"
)

const (
	ledgerOwner  = "owner@example.test"
	ledgerMember = "cloud:c@example.test"
	ledgerCand   = "0123456789abcdef"
)

// ledgerStory is the decision log of: a member's wish linked to a todo is
// activated (rev1); an incident on rev1 is repaired (rev2); the repair is
// undone and restored; a second wish is rejected; a third fails verification.
func ledgerStory() []state.AutoDecision {
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	n := 0
	at := func(d state.AutoDecision) state.AutoDecision {
		d.App, d.Time = "site", t0.Add(time.Duration(n)*time.Minute)
		n++
		return d
	}
	wish := state.AutoDecision{Source: "human", Problem: "todo:T-42", Agent: "stub", Principal: ledgerMember, Line: ledgerOwner, Base: "base0"}
	repair := state.AutoDecision{Source: "incident", Ref: "inc1", Problem: "incident:inc1", Principal: ledgerOwner, Line: ledgerOwner, Base: "rev1"}
	with := func(d state.AutoDecision, event, result, detail, rev string) state.AutoDecision {
		d.Event, d.Result, d.Detail = event, result, detail
		if rev != "" {
			d.Rev = rev
		}
		return at(d)
	}
	wishDone := wish
	wishDone.Why, wishDone.Expect = "the footer was missing", "footer shows on every page"
	repairDone := repair
	repairDone.Agent, repairDone.Why = "fixer", "script error in /home/op/shape-work/site/app.js"
	return []state.AutoDecision{
		at(state.AutoDecision{Event: "switch", Result: "on"}),
		with(wish, "trigger", "wish", ledgerCand+" add a footer", ""),
		with(wishDone, "verdict", "pass", ledgerCand+" checks passed", "rev1"),
		with(wishDone, "activation", "activated", ledgerCand+" rev1", "rev1"),
		with(repair, "trigger", "repair", "script /", ""),
		with(repairDone, "attempt", "failed", "agent exited 1", ""),
		with(repairDone, "attempt", "activated", "", "rev2"),
		with(repairDone, "verdict", "pass", "checks passed", "rev2"),
		with(repairDone, "activation", "activated", "rev2 v2", "rev2"),
		at(state.AutoDecision{Event: "undo", Result: "activated", Source: "human", Principal: ledgerOwner, Line: ledgerOwner, By: "person", Rev: "rev2", Base: "rev2", Detail: "broke search"}),
		at(state.AutoDecision{Event: "restore", Result: "activated", Source: "human", Principal: ledgerOwner, Line: ledgerOwner, By: "person", Rev: "rev2", Base: "rev1"}),
		at(state.AutoDecision{Event: "trigger", Result: "wish", Source: "human", Problem: "cand:aaaa1111bbbb2222", Principal: ledgerOwner, Line: ledgerOwner, Detail: "aaaa1111bbbb2222 make it pink"}),
		at(state.AutoDecision{Event: "reject", Result: "rejected", Source: "human", Problem: "cand:aaaa1111bbbb2222", Principal: ledgerOwner, Line: ledgerOwner, By: "person", Detail: "make it pink too loud"}),
		at(state.AutoDecision{Event: "trigger", Result: "wish", Source: "human", Problem: "cand:cccc3333dddd4444", Principal: "cloud:other@example.test", Line: ledgerOwner, Detail: "cccc3333dddd4444 add search"}),
		at(state.AutoDecision{Event: "verdict", Result: "fail", Source: "human", Problem: "cand:cccc3333dddd4444", Principal: "cloud:other@example.test", Line: ledgerOwner, Rev: "rev9", Detail: "cccc3333dddd4444 build failed in /var/folders/x/shape-work/key/work/pages/index.md"}),
		at(state.AutoDecision{Event: "trigger", Result: "wish", Source: "human", Problem: "cand:other", Detail: "another app"}),
	}[:15] // the last line is appended per test where needed
}

func ledgerProblemByKey(t *testing.T, l Ledger, key string) Problem {
	t.Helper()
	for _, p := range l.Problems {
		if p.Key == key {
			return p
		}
	}
	t.Fatalf("no problem %q in %+v", key, l.Problems)
	return Problem{}
}

func ledgerStandings(p Problem) string {
	var s []string
	for _, a := range p.Attempts {
		s = append(s, a.Standing)
	}
	return strings.Join(s, ",")
}

func TestLedgerFoldStandingAcrossRepairUndoRestore(t *testing.T) {
	lines := ledgerStory()
	// Another app's lines never reach this ledger.
	lines = append(lines, state.AutoDecision{App: "blog", Event: "trigger", Result: "wish", Problem: "cand:x", Detail: "other app"})

	// Step by step: the standing follows the events, and nothing is deleted.
	for _, tc := range []struct {
		upto int
		want string
		open bool
	}{
		{2, "pending", true},                      // wish only
		{4, "active", false},                      // activated
		{5, "active,pending", true},               // the incident reopens the problem
		{6, "active,failed", true},                // first repair attempt failed
		{9, "superseded,failed,active", false},    // repair landed
		{10, "active,failed,undone", true},        // undo: the wish is in effect again, the last try did not hold
		{11, "superseded,failed,restored", false}, // restore
	} {
		p := ledgerProblemByKey(t, Fold(lines[:tc.upto], "site", ""), "todo:T-42")
		if got := ledgerStandings(p); got != tc.want || p.Open != tc.open {
			t.Errorf("after %d lines: standings %q open %v, want %q open %v", tc.upto, got, p.Open, tc.want, tc.open)
		}
	}

	l := Fold(lines, "site", "")
	if len(l.Problems) != 3 {
		t.Fatalf("problems = %d, want 3 (the incident joins the wish): %+v", len(l.Problems), l.Problems)
	}
	if l.Problems[0].Key != "cand:cccc3333dddd4444" || l.Problems[2].Key != "todo:T-42" {
		t.Errorf("problems are not newest first: %q, %q, %q", l.Problems[0].Key, l.Problems[1].Key, l.Problems[2].Key)
	}
	p := ledgerProblemByKey(t, l, "todo:T-42")
	if len(p.Incidents) != 1 || p.Incidents[0] != "inc1" || p.Request != "add a footer" {
		t.Errorf("problem = %+v", p)
	}
	wish, failed, repair := p.Attempts[0], p.Attempts[1], p.Attempts[2]
	if wish.Candidate != ledgerCand || wish.Agent != "stub" || wish.Principal != ledgerMember || wish.Line != ledgerOwner ||
		wish.Request != "add a footer" || wish.Why != "the footer was missing" || wish.Expect != "footer shows on every page" ||
		wish.Observed != "pass: checks passed" || wish.Rev != "rev1" || wish.Evidence != "shape-evidence:rev1" ||
		len(wish.Incidents) != 1 || wish.Incidents[0] != "inc1" {
		t.Errorf("wish attempt = %+v", wish)
	}
	if failed.Observed != "failed: agent exited 1" || failed.Source != "incident" {
		t.Errorf("failed repair attempt = %+v", failed)
	}
	if repair.Rev != "rev2" || repair.Base != "rev1" || repair.Agent != "fixer" || repair.By != "person" {
		t.Errorf("repair attempt = %+v", repair)
	}

	// Rejected and failed attempts are kept.
	if r := ledgerProblemByKey(t, l, "cand:aaaa1111bbbb2222"); ledgerStandings(r) != "rejected" || r.Attempts[0].Reason != "too loud" || r.Attempts[0].Request != "make it pink" || !r.Open {
		t.Errorf("rejected problem = %+v", r)
	}
	if f := ledgerProblemByKey(t, l, "cand:cccc3333dddd4444"); ledgerStandings(f) != "failed" || !f.Open {
		t.Errorf("failed problem = %+v", f)
	}
}

func TestLedgerFoldSystemUndoAndUnjoinedIncident(t *testing.T) {
	lines := ledgerStory()[:9]
	lines = append(lines,
		state.AutoDecision{App: "site", Event: "undo", Result: "activated", Source: "incident", Ref: "inc1", Problem: "incident:inc1", By: "system", Rev: "rev2", Line: ledgerOwner, Detail: "patch release failed"},
		// An incident on a revision no attempt activated keeps its own problem.
		state.AutoDecision{App: "site", Event: "trigger", Result: "repair", Source: "incident", Ref: "inc7", Problem: "incident:inc7", Base: "release0", Detail: "asset /logo.png"},
		// A line from before the ledger fields existed groups by its source item.
		state.AutoDecision{App: "site", Event: "refusal", Result: "off", Source: "feed", Ref: "feed-9", Detail: "unattended shaping is off on this host"},
	)
	l := Fold(lines, "site", "")
	p := ledgerProblemByKey(t, l, "todo:T-42")
	if got := ledgerStandings(p); got != "active,failed,undone" || p.Attempts[2].By != "system" || p.Attempts[2].Reason != "patch release failed" {
		t.Errorf("standings %q, repair %+v", got, p.Attempts[2])
	}
	if own := ledgerProblemByKey(t, l, "incident:inc7"); !own.Open || ledgerStandings(own) != "pending" {
		t.Errorf("unjoined incident = %+v", own)
	}
	if old := ledgerProblemByKey(t, l, "feed:feed-9"); ledgerStandings(old) != "failed" {
		t.Errorf("old line = %+v", old)
	}
}

func TestLedgerFoldAtBaseRevision(t *testing.T) {
	lines := ledgerStory()
	l := Fold(lines, "site", "rev1")
	p := ledgerProblemByKey(t, l, "todo:T-42")
	if got := ledgerStandings(p); got != "active,later,later" || p.Open {
		t.Errorf("at rev1: standings %q open %v, want the wish active and the repair marked later", got, p.Open)
	}
	if len(p.Attempts[0].Incidents) != 0 {
		t.Errorf("an incident after the base is on the base's attempt: %+v", p.Attempts[0])
	}
	if len(l.Problems) != 3 {
		t.Errorf("later problems are hidden: %d", len(l.Problems))
	}
	if r := ledgerProblemByKey(t, l, "cand:aaaa1111bbbb2222"); ledgerStandings(r) != "later" {
		t.Errorf("later reject = %+v", r)
	}
	// rev2 was last made active by the restore.
	if got := ledgerStandings(ledgerProblemByKey(t, Fold(lines, "site", "rev2"), "todo:T-42")); got != "superseded,failed,restored" {
		t.Errorf("at rev2: %q", got)
	}
	if unknown := Fold(lines, "site", "nope"); unknown.Note == "" || ledgerStandings(ledgerProblemByKey(t, unknown, "todo:T-42")) != "superseded,failed,restored" {
		t.Errorf("unknown base = %+v", unknown)
	}
}

func TestLedgerRestoreOfEarlierVersionUndoesLaterOnes(t *testing.T) {
	var lines []state.AutoDecision
	for i, rev := range []string{"r1", "r2", "r3"} {
		key := fmt.Sprintf("cand:%d", i)
		lines = append(lines,
			state.AutoDecision{App: "site", Event: "trigger", Result: "wish", Problem: key, Line: "p", Detail: "wish " + rev},
			state.AutoDecision{App: "site", Event: "activation", Result: "activated", Problem: key, Line: "p", Rev: rev})
	}
	lines = append(lines, state.AutoDecision{App: "site", Event: "restore", Result: "activated", Line: "p", Rev: "r1", By: "person"})
	l := Fold(lines, "site", "")
	for key, want := range map[string]string{"cand:0": "restored", "cand:1": "undone", "cand:2": "undone"} {
		if got := ledgerStandings(ledgerProblemByKey(t, l, key)); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
}

func TestLedgerBound(t *testing.T) {
	var lines []state.AutoDecision
	t0 := time.Date(2026, 11, 26, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 60; i++ {
		key, rev := fmt.Sprintf("cand:%04d", i), fmt.Sprintf("rev%04d", i)
		at := t0.Add(time.Duration(i) * time.Minute)
		lines = append(lines, state.AutoDecision{App: "site", Time: at, Event: "trigger", Result: "wish", Problem: key, Detail: strings.Repeat("long request ", 20)})
		if i%10 != 9 { // every tenth stays open
			lines = append(lines, state.AutoDecision{App: "site", Time: at, Event: "activation", Result: "activated", Problem: key, Rev: rev, Agent: "stub", Why: strings.Repeat("why ", 50)})
		}
	}
	full := Fold(lines, "site", "")
	if got := Bound(full, -1); len(got.Problems) != 60 || got.Truncated != "" || got.Problems[1].Summary != "" {
		t.Errorf("unbounded view changed the ledger")
	}
	for _, max := range []int{0, 2048} {
		got := Bound(full, max)
		limit := max
		if limit == 0 {
			limit = DefaultLedgerBytes
		}
		data, _ := json.Marshal(got)
		if len(data) > limit {
			t.Errorf("max %d: %d bytes", max, len(data))
		}
		if got.Truncated == "" || len(got.Problems) == 0 || len(got.Problems) >= 60 {
			t.Errorf("max %d: %d problems, truncated %q", max, len(got.Problems), got.Truncated)
		}
		if got.Problems[0].Key != "cand:0059" || !got.Problems[0].Open || len(got.Problems[0].Attempts) != 1 {
			t.Errorf("max %d: newest open problem is not shown in full: %+v", max, got.Problems[0])
		}
		if closed := got.Problems[1]; closed.Open || len(closed.Attempts) != 0 || closed.Summary != "1 attempt(s); active at rev0058 by stub" {
			t.Errorf("max %d: closed problem = %+v", max, closed)
		}
		if !strings.Contains(got.Text(), "truncated: ") {
			t.Errorf("text does not say it is truncated")
		}
	}
	if len(full.Problems[1].Attempts) != 1 {
		t.Errorf("Bound changed its input")
	}
}

func TestLedgerForViewer(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip(err)
	}
	lines := ledgerStory()
	lines[2].Detail = ledgerCand + " checks passed; see " + filepath.Join(home, "notes.txt") + " for T-42"
	lines[3].Why = "asked by " + ledgerOwner
	full := Fold(lines, "site", "")

	if got := ForViewer(full, true, ledgerOwner); fmt.Sprint(got) != fmt.Sprint(full) {
		t.Errorf("the owner's view is filtered")
	}
	got := ForViewer(full, false, ledgerMember)
	data, _ := json.Marshal(got)
	for _, leak := range []string{"T-42", "todo:", home, "/var/folders", "/home/op", ledgerOwner, "other@example.test"} {
		if strings.Contains(string(data), leak) {
			t.Errorf("member view leaks %q: %s", leak, data)
		}
	}
	var mine, aliased int
	for _, p := range got.Problems {
		if strings.HasPrefix(p.Key, "p-") && len(p.Key) == 12 {
			aliased++
		}
		for _, a := range p.Attempts {
			if a.Line != "" {
				t.Errorf("the owner's line is shown: %+v", a)
			}
			if a.Principal == ledgerMember {
				mine++
			} else if a.Principal != "" {
				t.Errorf("another principal is shown: %+v", a)
			}
		}
	}
	if aliased != len(got.Problems) || mine != 1 {
		t.Errorf("aliased %d of %d problems, own attempts %d", aliased, len(got.Problems), mine)
	}
	// The alias is stable, and the content a member needs is kept.
	again := ForViewer(Fold(lines[:4], "site", ""), false, "cloud:z@example.test")
	wish := got.Problems[2]
	if again.Problems[0].Key != wish.Key {
		t.Errorf("alias is not stable: %q then %q", again.Problems[0].Key, wish.Key)
	}
	a := wish.Attempts[0]
	if a.Request != "add a footer" || a.Standing != StandingSuperseded || a.Rev != "rev1" || a.Evidence != "shape-evidence:rev1" ||
		a.Agent != "stub" || !strings.Contains(a.Observed, "checks passed; see ~/notes.txt for "+wish.Key) {
		t.Errorf("member's wish attempt = %+v", a)
	}
	if f := got.Problems[0].Attempts[0]; !strings.HasSuffix(f.Observed, "build failed in index.md") {
		t.Errorf("host path kept in %q", f.Observed)
	}
	if full.Problems[2].Key != "todo:T-42" || full.Problems[2].Attempts[0].Line != ledgerOwner {
		t.Errorf("ForViewer changed its input")
	}
}

func TestServiceLedgerReadsTheDecisionLog(t *testing.T) {
	home := t.TempDir()
	for _, d := range ledgerStory() {
		if err := state.LogAuto(home, d); err != nil {
			t.Fatal(err)
		}
	}
	owned := Service{App: App{Name: "site", Owner: ledgerOwner}, Options: Options{Home: home}}
	for _, tc := range []struct {
		name   string
		caller Caller
		full   bool
		key    string
	}{
		{"operator", Caller{}, true, "todo:T-42"},
		{"owner", Caller{Principal: ledgerOwner}, false, "todo:T-42"},
	} {
		l, err := owned.Ledger(tc.caller, tc.full, "", -1)
		if err != nil || len(l.Problems) != 3 || l.Problems[2].Key != tc.key {
			t.Errorf("%s: %v %+v", tc.name, err, l)
		}
	}
	member, err := owned.Ledger(Caller{Principal: ledgerMember, Tier: "member"}, false, "", 0)
	if err != nil || len(member.Problems) != 3 || !strings.HasPrefix(member.Problems[2].Key, "p-") {
		t.Errorf("member: %v %+v", err, member)
	}
	// With no owner each person shapes their own line and reads only it.
	perPerson := Service{App: App{Name: "site"}, Options: Options{Home: home}}
	own, err := perPerson.Ledger(Caller{Principal: ledgerMember}, false, "", -1)
	if err != nil || len(own.Problems) != 1 || own.Problems[0].Attempts[0].Principal != ledgerMember {
		t.Errorf("per-person: %v %+v", err, own)
	}
}
