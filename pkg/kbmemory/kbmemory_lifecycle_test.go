package kbmemory

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/kblife"
)

// modes is every storage mode a memory target can take.
func modes(t *testing.T, dir string) map[string]struct{ target, session string } {
	s := owner(t, dir)
	return map[string]struct{ target, session string }{
		"persistent-direct": {loc("sqlite:file:" + filepath.Join(dir, "direct.db")), ""},
		"persistent-shared": {loc("sqlite:file:" + filepath.Join(dir, "shared.db")), s},
		"memory":            {loc("sqlite:mem:" + s + "/life"), ""},
	}
}

func state(t *testing.T, env Envelope) kblife.State {
	t.Helper()
	var r struct {
		State kblife.State `json:"state"`
	}
	b, _ := json.Marshal(env.Result)
	_ = json.Unmarshal(b, &r)
	return r.State
}

func inspectRev(t *testing.T, target, session, id string) (Record, int64, kblife.State) {
	t.Helper()
	env := must(t, Request{Verb: "inspect", Target: target, Session: session, ID: id})
	return record(t, env), *env.Revision, state(t, env)
}

func TestSupersedeAndForget(t *testing.T) {
	dir := isolate(t)
	at := time.Date(2026, 10, 18, 3, 0, 0, 0, time.UTC)
	pin(t, func() time.Time { return at })
	for name, tc := range modes(t, dir) {
		t.Run(name, func(t *testing.T) {
			tg, ss := tc.target, tc.session
			must(t, Request{Verb: "init", Target: tg, Session: ss})
			a := record(t, must(t, Request{Verb: "add", Target: tg, Session: ss, Body: str("old"), SourceRef: "file:a.md"}))
			b := record(t, must(t, Request{Verb: "add", Target: tg, Session: ss, Body: str("new")}))
			sup := Request{Verb: "supersede", Target: tg, Session: ss, ID: a.ID, IfRevision: 1, Replacement: b.ID, ReplacementRevision: 1}

			// Invalid inputs and stale preconditions change nothing.
			for _, bad := range []struct {
				want string
				req  Request
			}{
				{"invalid_input", Request{Verb: "supersede", Target: tg, Session: ss, ID: a.ID, IfRevision: 1}},
				{"invalid_input", Request{Verb: "supersede", Target: tg, Session: ss, ID: a.ID, IfRevision: 1, Replacement: a.ID, ReplacementRevision: 1}},
				{"conflict", Request{Verb: "supersede", Target: tg, Session: ss, ID: a.ID, IfRevision: 2, Replacement: b.ID, ReplacementRevision: 1}},
				{"conflict", Request{Verb: "supersede", Target: tg, Session: ss, ID: a.ID, IfRevision: 1, Replacement: b.ID, ReplacementRevision: 7}},
				{"not_found", Request{Verb: "supersede", Target: tg, Session: ss, ID: a.ID, IfRevision: 1, Replacement: "mem-missing", ReplacementRevision: 1}},
				{"invalid_input", Request{Verb: "forget", Target: tg, Session: ss, ID: a.ID, IfRevision: 1}},
				{"conflict", Request{Verb: "forget", Target: tg, Session: ss, ID: a.ID, IfRevision: 3, Reason: "r"}},
				{"invalid_input", Request{Verb: "update", Target: tg, Session: ss, ID: a.ID, IfRevision: 1, Body: str("x"), Reason: "r"}},
			} {
				if c := code(t, bad.req); c != bad.want {
					t.Fatalf("%s %+v = %s, want %s", bad.req.Verb, bad.req, c, bad.want)
				}
			}
			for _, id := range []string{a.ID, b.ID} {
				if r, rev, st := inspectRev(t, tg, ss, id); rev != 1 || st != kblife.StateValid || r.ValidTo != "" || r.ValidFrom != "" {
					t.Fatalf("refused supersede left %s at rev %d state %s %+v", id, rev, st, r)
				}
			}

			env := must(t, sup)
			if *env.Revision != 2 {
				t.Fatalf("supersede revision %d", *env.Revision)
			}
			stamp := at.Format(time.RFC3339)
			old, oldRev, oldState := inspectRev(t, tg, ss, a.ID)
			rep, repRev, repState := inspectRev(t, tg, ss, b.ID)
			if oldRev != 2 || repRev != 2 || old.ValidTo != stamp || old.SupersededBy != b.ID || old.Status != "superseded" ||
				rep.ValidFrom != stamp || rep.Supersedes != a.ID || oldState != kblife.StateSuperseded || repState != kblife.StateValid {
				t.Fatalf("supersede: old %+v rev %d %s; new %+v rev %d %s", old, oldRev, oldState, rep, repRev, repState)
			}
			// Audit: body, provenance and creation survive.
			if old.Body != "old" || old.Source.Ref != "file:a.md" || old.Source.Episode != "ep-test" || old.Created != a.Created {
				t.Fatalf("supersede lost audit fields: %+v", old)
			}
			// A superseded record cannot be superseded again.
			c := record(t, must(t, Request{Verb: "add", Target: tg, Session: ss, Body: str("third")}))
			if got := code(t, Request{Verb: "supersede", Target: tg, Session: ss, ID: a.ID, IfRevision: 2, Replacement: c.ID, ReplacementRevision: 1}); got != "invalid_input" {
				t.Fatalf("re-supersede = %s", got)
			}

			fg := must(t, Request{Verb: "forget", Target: tg, Session: ss, ID: b.ID, IfRevision: 2, Reason: " wrong "})
			if *fg.Revision != 3 || state(t, fg) != kblife.StateForgotten {
				t.Fatalf("forget: %+v", fg)
			}
			gone, rev, st := inspectRev(t, tg, ss, b.ID)
			tb := gone.Tombstone
			if rev != 3 || st != kblife.StateForgotten || tb == nil || tb.Ref != "memory:"+b.ID || tb.Reason != "wrong" || tb.By == "" ||
				tb.Episode != "ep-test" || !tb.At.Equal(at) || gone.Body != "new" || gone.Supersedes != a.ID {
				t.Fatalf("forgotten record %+v rev %d %s", gone, rev, st)
			}
			if got := code(t, Request{Verb: "forget", Target: tg, Session: ss, ID: b.ID, IfRevision: 3, Reason: "again"}); got != "invalid_input" {
				t.Fatalf("second forget = %s", got)
			}
		})
	}
}

// TestLifecycleReplay: an owner-session retry of the same supersede or
// forget replays the committed outcome — no second mutation, the original
// instant — even after the clock moved on.
func TestLifecycleReplay(t *testing.T) {
	dir := isolate(t)
	at := time.Date(2026, 10, 18, 4, 0, 0, 0, time.UTC)
	now := at
	pin(t, func() time.Time { return now })
	for name, tc := range modes(t, dir) {
		if tc.session == "" && name == "persistent-direct" {
			continue // request IDs are an owner guarantee (refused directly)
		}
		t.Run(name, func(t *testing.T) {
			now = at
			tg, ss := tc.target, tc.session
			must(t, Request{Verb: "init", Target: tg, Session: ss})
			a := record(t, must(t, Request{Verb: "add", Target: tg, Session: ss, Body: str("a")}))
			b := record(t, must(t, Request{Verb: "add", Target: tg, Session: ss, Body: str("b")}))
			sup := Request{Verb: "supersede", Target: tg, Session: ss, ID: a.ID, IfRevision: 1, Replacement: b.ID, ReplacementRevision: 1, RequestID: name + "-sup"}
			first := must(t, sup)
			now = at.Add(time.Hour)
			again := must(t, sup)
			if *again.Revision != *first.Revision || record(t, again).ValidTo != at.Format(time.RFC3339) {
				t.Fatalf("supersede replay %+v", again)
			}
			fg := Request{Verb: "forget", Target: tg, Session: ss, ID: b.ID, IfRevision: 2, Reason: "r", RequestID: name + "-fg"}
			f1 := must(t, fg)
			now = at.Add(2 * time.Hour)
			f2 := must(t, fg)
			if *f2.Revision != *f1.Revision || !record(t, f2).Tombstone.At.Equal(record(t, f1).Tombstone.At) {
				t.Fatalf("forget replay %+v vs %+v", f2, f1)
			}
			if _, rev, _ := inspectRev(t, tg, ss, b.ID); rev != 3 {
				t.Fatalf("replay mutated: rev %d", rev)
			}
		})
	}
	if c := code(t, Request{Verb: "forget", Target: loc("sqlite:file:" + filepath.Join(dir, "direct.db")), ID: "mem-x", IfRevision: 1, Reason: "r", RequestID: "d"}); c != "unsupported" {
		t.Fatalf("direct request-id forget = %s", c)
	}
}

// TestIndependentProcessLifecycle: separate client processes race the same
// supersede (two replacements for one record) and the same forget, in every
// storage mode; exactly one wins and the record is never half-superseded.
// Process-level; the manager runs it on the remote gate (skipped under -short).
func TestIndependentProcessLifecycle(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level: remote gate")
	}
	dir := isolate(t)
	for name, tc := range modes(t, dir) {
		t.Run(name, func(t *testing.T) {
			tg, ss, w := tc.target, tc.session, 10*time.Second
			must(t, Request{Verb: "init", Target: tg, Session: ss})
			old := record(t, must(t, Request{Verb: "add", Target: tg, Session: ss, Body: str("old")}))
			r1 := record(t, must(t, Request{Verb: "add", Target: tg, Session: ss, Body: str("r1")}))
			r2 := record(t, must(t, Request{Verb: "add", Target: tg, Session: ss, Body: str("r2")}))
			envs := parallelChildren(t,
				func() Envelope {
					return child(t, "ep-a", Request{Verb: "supersede", Target: tg, Session: ss, ID: old.ID, IfRevision: 1, Replacement: r1.ID, ReplacementRevision: 1, Wait: w})
				},
				func() Envelope {
					return child(t, "ep-b", Request{Verb: "supersede", Target: tg, Session: ss, ID: old.ID, IfRevision: 1, Replacement: r2.ID, ReplacementRevision: 1, Wait: w})
				})
			if won, other := codes(envs); won != 1 || len(other) != 1 || (other[0] != "conflict" && other[0] != "busy" && other[0] != "invalid_input") {
				t.Fatalf("supersede race: %d won, %v", won, other)
			}
			winner, loser := r1, r2
			if !envs[0].OK {
				winner, loser = r2, r1
			}
			o, _, _ := inspectRev(t, tg, ss, old.ID)
			if _, rev, _ := inspectRev(t, tg, ss, loser.ID); rev != 1 || o.SupersededBy != winner.ID {
				t.Fatalf("half-applied supersede: old %+v, loser rev %d", o, rev)
			}
			envs = parallelChildren(t,
				func() Envelope {
					return child(t, "ep-a", Request{Verb: "forget", Target: tg, Session: ss, ID: loser.ID, IfRevision: 1, Reason: "a", Wait: w})
				},
				func() Envelope {
					return child(t, "ep-b", Request{Verb: "forget", Target: tg, Session: ss, ID: loser.ID, IfRevision: 1, Reason: "b", Wait: w})
				})
			if won, other := codes(envs); won != 1 || len(other) != 1 || (other[0] != "conflict" && other[0] != "busy" && other[0] != "invalid_input") {
				t.Fatalf("forget race: %d won, %v", won, other)
			}
		})
	}
}

// TestLifecycleCLIAndMCP: supersede and forget take their flags through the
// one command, with the contract exit codes.
func TestLifecycleCLIAndMCP(t *testing.T) {
	dir := isolate(t)
	target := loc("sqlite:file:cli.db")
	run := MCPCommand().Run
	ctx := context.Background()
	if _, _, exit := run(ctx, []string{"init", target}, "", dir); exit != 0 {
		t.Fatalf("init exit %d", exit)
	}
	ids := []string{}
	for _, body := range []string{"a", "b"} {
		out, _, exit := run(ctx, []string{"--json", "--body", body, "add", target}, "", dir)
		var env Envelope
		if exit != 0 || json.Unmarshal([]byte(out), &env) != nil {
			t.Fatalf("add: %d %s", exit, out)
		}
		ids = append(ids, record(t, env).ID)
	}
	if out, _, exit := run(ctx, []string{"--json", "--if-revision", "1", "forget", target, ids[0]}, "", dir); exit != 2 || !strings.Contains(out, `"code":"invalid_input"`) {
		t.Fatalf("forget without reason: %d %s", exit, out)
	}
	if out, _, exit := run(ctx, []string{"--json", "--if-revision", "1", "--replacement", ids[1], "--replacement-revision", "1", "supersede", target, ids[0]}, "", dir); exit != 0 || !strings.Contains(out, `"superseded_by":"`+ids[1]+`"`) {
		t.Fatalf("supersede: %d %s", exit, out)
	}
	if out, _, exit := run(ctx, []string{"--json", "--if-revision", "1", "--reason", "x", "forget", target, ids[1]}, "", dir); exit != 1 || !strings.Contains(out, `"code":"conflict"`) {
		t.Fatalf("stale forget: %d %s", exit, out)
	}
	if out, _, exit := run(ctx, []string{"--if-revision", "2", "--reason", "x", "forget", target, ids[1]}, "", dir); exit != 0 || !strings.Contains(out, "forget "+ids[1]) {
		t.Fatalf("forget: %d %s", exit, out)
	}
}
