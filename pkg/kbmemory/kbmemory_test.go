package kbmemory

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/dblifecycle"
	"github.com/qiangli/yoke/pkg/ref"
	"github.com/spf13/cobra"
)

func isolate(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", filepath.Join(dir, "coord"))
	t.Setenv("BASHY_DB_SESSION_DIR", filepath.Join(dir, "sessions"))
	t.Setenv("BASHY_KB_DIR", filepath.Join(dir, "kb"))
	t.Setenv("BASHY_EPISODE", "ep-test")
	t.Setenv("BASHY_PARENT_EPISODE", "ep-parent")
	return dir
}

// owner serves an explicit in-process owner session, the same server
// `bashy db session start` runs, and returns its session ID.
func owner(t *testing.T, dir string) string {
	t.Helper()
	ready := filepath.Join(dir, "ready")
	ctx, stop := context.WithCancel(context.Background())
	served := make(chan error, 1)
	go func() { served <- dblifecycle.Serve(ctx, ready, 0) }()
	t.Cleanup(func() { stop(); <-served })
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		if b, err := os.ReadFile(ready); err == nil && strings.HasPrefix(string(b), "s-") {
			return string(b)
		}
	}
	t.Fatal("owner did not become ready")
	return ""
}

// loc spells a db target as the shared S11 locator.
func loc(target string) string { return (ref.Locator{Scheme: "db", Target: target}).String() }

func str(s string) *string { return &s }

// pin sets the operation clock for the rest of the test.
func pin(t *testing.T, now func() time.Time) {
	t.Helper()
	old := clock
	clock = now
	t.Cleanup(func() { clock = old })
}

func must(t *testing.T, req Request) Envelope {
	t.Helper()
	env := Run(context.Background(), req)
	if !env.OK || env.SchemaVersion != ResultSchema {
		t.Fatalf("%s: %+v", req.Verb, env.Error)
	}
	return env
}

func code(t *testing.T, req Request) string {
	t.Helper()
	env := Run(context.Background(), req)
	if env.OK || env.Error == nil || env.SchemaVersion != ErrorSchema {
		t.Fatalf("%s: want an error envelope, got %+v", req.Verb, env)
	}
	return env.Error.Code
}

func record(t *testing.T, env Envelope) Record {
	t.Helper()
	var r struct {
		Record Record `json:"record"`
	}
	b, _ := json.Marshal(env.Result)
	if err := json.Unmarshal(b, &r); err != nil || r.Record.ID == "" {
		t.Fatalf("no record in %s (%v)", b, err)
	}
	return r.Record
}

// lifecycle exercises init/select/add/inspect/update against one target.
func lifecycle(t *testing.T, target, session string) {
	fixed := func() time.Time { return time.Date(2026, 10, 18, 1, 2, 3, 0, time.UTC) }
	pin(t, fixed)
	if c := code(t, Request{Verb: "select", Target: target, Session: session}); c != "not_found" {
		t.Fatalf("select before init = %s, want not_found", c)
	}
	ini := must(t, Request{Verb: "init", Target: target, Session: session})
	stores := ini.Result.(map[string]any)["stores"].([]any)
	if len(stores) != 1 || stores[0].(map[string]any)["id"] != ini.Target.ID {
		t.Fatalf("init must report its store location: %+v", ini.Result)
	}
	if c := code(t, Request{Verb: "init", Target: target, Session: session}); c != "exists" {
		t.Fatalf("second init = %s, want exists", c)
	}
	must(t, Request{Verb: "select", Target: target, Session: session})

	added := must(t, Request{Verb: "add", Target: target, Session: session, Type: str("fact"), Title: str("t"), Body: str(" the body \n"),
		Tags: &[]string{"a", "a", " b "}, Repos: &[]string{"yoke"}, SourceRef: "file:notes.md"})
	r := record(t, added)
	if *added.Revision != 1 || r.Status != "candidate" || r.Form != "note" || r.Body != "the body" || strings.Join(r.Tags, ",") != "a,b" ||
		r.Scope.Repos[0] != "yoke" || r.Source.Ref != "file:notes.md" || r.Source.Episode != "ep-test" || r.Source.ParentEpisode != "ep-parent" ||
		r.Source.Tool == "" || r.Created != "2026-10-18T01:02:03Z" || r.Updated != r.Created {
		t.Fatalf("added record %+v rev %d", r, *added.Revision)
	}
	got := record(t, must(t, Request{Verb: "inspect", Target: target, Session: session, ID: r.ID}))
	if got.ID != r.ID || got.Body != r.Body || got.Created != r.Created {
		t.Fatalf("inspect %+v, want %+v", got, r)
	}
	if c := code(t, Request{Verb: "inspect", Target: target, Session: session, ID: "mem-missing"}); c != "not_found" {
		t.Fatalf("inspect missing = %s", c)
	}
	if c := code(t, Request{Verb: "update", Target: target, Session: session, ID: r.ID, Body: str("x")}); c != "invalid_input" {
		t.Fatalf("update without --if-revision = %s", c)
	}
	pin(t, func() time.Time { return fixed().Add(time.Hour) })
	up := must(t, Request{Verb: "update", Target: target, Session: session, ID: r.ID, IfRevision: 1, Body: str("new"), OS: str("linux")})
	u := record(t, up)
	if *up.Revision != 2 || u.ID != r.ID || u.Body != "new" || u.Scope.OS != "linux" || u.Scope.Repos[0] != "yoke" || u.Created != r.Created ||
		u.Updated != "2026-10-18T02:02:03Z" || u.Status != "candidate" || u.Source.Episode != "ep-test" {
		t.Fatalf("updated %+v rev %d", u, *up.Revision)
	}
	if c := code(t, Request{Verb: "update", Target: target, Session: session, ID: r.ID, IfRevision: 1, Body: str("lost")}); c != "conflict" {
		t.Fatalf("stale update = %s, want conflict", c)
	}
	if c := code(t, Request{Verb: "add", Target: target, Session: session, Type: str("vibe"), Body: str("b")}); c != "invalid_input" {
		t.Fatalf("bad type = %s", c)
	}
}

func TestPersistentLifecycle(t *testing.T) {
	dir := isolate(t)
	target := loc("sqlite:file:" + filepath.Join(dir, "mem.db"))
	lifecycle(t, target, "")
	// The existing kb stores are never touched by memory collections.
	if _, err := os.Stat(filepath.Join(dir, "kb")); !os.IsNotExist(err) {
		t.Fatalf("kb store was created: %v", err)
	}
}

func TestMemoryModeLifecycleThroughOwner(t *testing.T) {
	dir := isolate(t)
	s := owner(t, dir)
	lifecycle(t, loc("sqlite:mem:"+s+"/scratch"), "")
	// No disk backing appears for an in-memory collection.
	if entries, _ := filepath.Glob(filepath.Join(dir, "*.db")); len(entries) != 0 {
		t.Fatalf("memory mode wrote %v", entries)
	}
}

func TestPersistentThroughExplicitSharedOwner(t *testing.T) {
	dir := isolate(t)
	lifecycle(t, loc("sqlite:file:"+filepath.Join(dir, "shared.db")), owner(t, dir))
}

func TestNothingImplicit(t *testing.T) {
	dir := isolate(t)
	missing := filepath.Join(dir, "missing.db")
	for _, verb := range []string{"select", "add"} {
		if c := code(t, Request{Verb: verb, Target: loc("sqlite:file:" + missing), Body: str("b")}); c != "not_found" && c != "invalid_input" {
			t.Fatalf("%s on missing target = %s", verb, c)
		}
	}
	if _, err := os.Stat(missing); !os.IsNotExist(err) {
		t.Fatal("a missing target was created")
	}
	if c := code(t, Request{Verb: "add", Body: str("b")}); c != "invalid_input" {
		t.Fatalf("no target = %s", c)
	}
	if c := code(t, Request{Verb: "supersede", Target: loc("sqlite:file:" + missing)}); c != "invalid_input" {
		t.Fatalf("supersede must not exist here (284 M6) = %s", c)
	}
	if c := code(t, Request{Verb: "select", Target: loc("bbolt:file:" + missing)}); c != "unsupported" {
		t.Fatalf("non-document engine = %s", c)
	}
	if c := code(t, Request{Verb: "init", Target: loc("sqlite:file:" + missing), RequestID: "r1"}); c != "unsupported" {
		t.Fatalf("request-id without owner = %s", c)
	}
}

func TestConcurrentUpdatesLoseNothing(t *testing.T) {
	dir := isolate(t)
	target := loc("sqlite:file:" + filepath.Join(dir, "c.db"))
	must(t, Request{Verb: "init", Target: target})
	r := record(t, must(t, Request{Verb: "add", Target: target, Body: str("v0")}))
	var wg sync.WaitGroup
	codes := make([]string, 8)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			env := Run(context.Background(), Request{Verb: "update", Target: target, ID: r.ID, IfRevision: 1, Body: str("v"), Wait: 5 * time.Second})
			if env.Error != nil {
				codes[i] = env.Error.Code
			}
		}(i)
	}
	wg.Wait()
	won := 0
	for _, c := range codes {
		switch c {
		case "":
			won++
		case "conflict", "busy":
		default:
			t.Fatalf("unexpected code %s", c)
		}
	}
	if won != 1 {
		t.Fatalf("%d updates won from one revision, want exactly 1 (%v)", won, codes)
	}
}

// TestCLIAndMCPParity drives the same verbs through the cobra command and
// the MCP runner: one parser, one envelope, stdin and files both accepted.
func TestCLIAndMCPParity(t *testing.T) {
	dir := isolate(t)
	target := loc("sqlite:file:mem.db")
	run := MCPCommand().Run
	out, errOut, exit := run(context.Background(), []string{"--json", "init", target}, "", dir)
	if exit != 0 || !strings.Contains(out, `"schema_version":"bashy-kb-memory-result-v1"`) || !strings.Contains(out, filepath.Join(dir, "mem.db")) {
		t.Fatalf("mcp init: %d %s %s", exit, out, errOut)
	}
	out, _, exit = run(context.Background(), []string{"--json", "-f", "-", "--tags", "x,y", "add", target}, "from stdin\n", dir)
	var env Envelope
	if exit != 0 || json.Unmarshal([]byte(out), &env) != nil {
		t.Fatalf("mcp add: %d %s", exit, out)
	}
	r := record(t, env)
	if r.Body != "from stdin" || r.Source.Ref != "" {
		t.Fatalf("stdin record %+v", r)
	}
	if err := os.WriteFile(filepath.Join(dir, "n.md"), []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := newCmd(context.Background, dir)
	var buf strings.Builder
	cmd.SetOut(&buf)
	cmd.SetArgs([]string{"add", target, "-f", "n.md", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	var cli Envelope
	if err := json.Unmarshal([]byte(buf.String()), &cli); err != nil {
		t.Fatal(err)
	}
	if fr := record(t, cli); fr.Body != "from file" || fr.Source.Ref != "file:n.md" || cli.SchemaVersion != env.SchemaVersion || cli.Op != env.Op {
		t.Fatalf("cli record %+v", cli)
	}
	out, _, exit = run(context.Background(), []string{"--json", "--if-revision", "9", "--body", "z", "update", target, r.ID}, "", dir)
	if exit != 1 || !strings.Contains(out, `"code":"conflict"`) || !strings.Contains(out, ErrorSchema) {
		t.Fatalf("mcp stale update: %d %s", exit, out)
	}
	if _, _, exit = run(context.Background(), []string{"--json", "bogus", target}, "", dir); exit != 2 {
		t.Fatalf("unknown verb exit %d", exit)
	}
}

// TestRequestIDReplay: a retried add/update with the same request ID returns
// the committed outcome through the owner's existing replay — never a second
// record, a fresh timestamp, or a conflict because a later update moved the
// record on. Reuse with other arguments is refused.
func TestRequestIDReplay(t *testing.T) {
	dir := isolate(t)
	s := owner(t, dir)
	target := loc("sqlite:file:" + filepath.Join(dir, "replay.db"))
	if c := code(t, Request{Verb: "init", Target: target, Session: s, RequestID: "i1"}); c != "unsupported" {
		t.Fatalf("init with request-id = %s, want unsupported", c)
	}
	must(t, Request{Verb: "init", Target: target, Session: s})
	tick := time.Date(2026, 10, 18, 1, 0, 0, 0, time.UTC)
	pin(t, func() time.Time { tick = tick.Add(time.Minute); return tick })
	add := Request{Verb: "add", Target: target, Session: s, Body: str("once"), RequestID: "a1"}
	first, again := must(t, add), must(t, add)
	r1, r2 := record(t, first), record(t, again)
	if r1.ID != r2.ID || r1.Created != r2.Created || *first.Revision != 1 || *again.Revision != 1 {
		t.Fatalf("add replay: %+v rev %d vs %+v rev %d", r1, *first.Revision, r2, *again.Revision)
	}
	if n := records(t, target, s); n != "1" {
		t.Fatalf("replayed add stored %s records", n)
	}
	other := add
	other.Body = str("different")
	if c := code(t, other); c != "conflict" {
		t.Fatalf("request-id reuse with other body = %s, want conflict", c)
	}

	up := Request{Verb: "update", Target: target, Session: s, ID: r1.ID, IfRevision: 1, Body: str("v2"), RequestID: "u1"}
	u1, u1again := must(t, up), must(t, up)
	if *u1.Revision != 2 || *u1again.Revision != 2 || record(t, u1).Updated != record(t, u1again).Updated {
		t.Fatalf("update replay: %+v / %+v", u1, u1again)
	}
	// The named regression: another request moves the record to revision 3
	// at a later clock; retrying u1 still returns u1's committed rev-2
	// outcome and mutates nothing.
	u3 := must(t, Request{Verb: "update", Target: target, Session: s, ID: r1.ID, IfRevision: 2, Body: str("v3"), RequestID: "u3"})
	if *u3.Revision != 3 || record(t, u3).Updated == record(t, u1).Updated {
		t.Fatalf("u3: %+v", u3)
	}
	retry := must(t, up)
	if rr := record(t, retry); *retry.Revision != 2 || rr.Body != "v2" || rr.Updated != record(t, u1).Updated {
		t.Fatalf("u1 retry after u3 = rev %d %+v, want the original rev-2 outcome", *retry.Revision, rr)
	}
	cur := must(t, Request{Verb: "inspect", Target: target, Session: s, ID: r1.ID})
	if *cur.Revision != 3 || record(t, cur).Body != "v3" {
		t.Fatalf("replay mutated the record: rev %d %+v", *cur.Revision, record(t, cur))
	}
	if c := code(t, Request{Verb: "update", Target: target, Session: s, ID: r1.ID, IfRevision: 1, Body: str("v2"), RequestID: "u2"}); c != "conflict" {
		t.Fatalf("fresh request on stale revision = %s, want conflict", c)
	}
}

func records(t *testing.T, target, session string) string {
	t.Helper()
	return fmt.Sprint(must(t, Request{Verb: "select", Target: target, Session: session}).Result.(map[string]any)["records"])
}

// TestRequestIDAcrossPrincipals: the owner's replay is scoped to the
// principal and its provenance. Another episode reusing a request ID is an
// independent request: an add writes its own record, and a stale update is a
// conflict that mutates nothing. Neither is claimed as a replay.
func TestRequestIDAcrossPrincipals(t *testing.T) {
	dir := isolate(t)
	s := owner(t, dir)
	target := loc("sqlite:mem:" + s + "/principals")
	must(t, Request{Verb: "init", Target: target})
	tick := time.Date(2026, 10, 18, 2, 0, 0, 0, time.UTC)
	pin(t, func() time.Time { tick = tick.Add(time.Minute); return tick })
	add := Request{Verb: "add", Target: target, Body: str("shared"), Tags: &[]string{"t"}, RequestID: "x1"}
	first := record(t, must(t, add))
	up := Request{Verb: "update", Target: target, ID: first.ID, IfRevision: 1, Body: str("v2"), RequestID: "xu"}
	must(t, up)

	t.Setenv("BASHY_EPISODE", "ep-other")
	other := record(t, must(t, add))
	if other.ID == first.ID || other.Source.Episode != "ep-other" {
		t.Fatalf("other-principal add = %+v, want an independent record", other)
	}
	if n := records(t, target, ""); n != "2" {
		t.Fatalf("records = %s, want 2", n)
	}
	if c := code(t, up); c != "conflict" {
		t.Fatalf("other-principal stale update = %s, want conflict", c)
	}
	if cur := must(t, Request{Verb: "inspect", Target: target, ID: first.ID}); *cur.Revision != 2 {
		t.Fatalf("stale update mutated: rev %d", *cur.Revision)
	}
}

// TestConcurrentSameRequest: concurrent retries of one logical add and one
// logical update return one committed outcome, in both storage modes.
func TestConcurrentSameRequest(t *testing.T) {
	dir := isolate(t)
	s := owner(t, dir)
	for name, tc := range map[string]struct{ target, session string }{
		"memory":     {loc("sqlite:mem:" + s + "/conc"), ""},
		"persistent": {loc("sqlite:file:" + filepath.Join(dir, "conc.db")), s},
	} {
		t.Run(name, func(t *testing.T) {
			must(t, Request{Verb: "init", Target: tc.target, Session: tc.session})
			base := record(t, must(t, Request{Verb: "add", Target: tc.target, Session: tc.session, Body: str("v1")}))
			reqs := []Request{
				{Verb: "add", Target: tc.target, Session: tc.session, Body: str("once"), RequestID: name + "-add", Wait: 5 * time.Second},
				{Verb: "update", Target: tc.target, Session: tc.session, ID: base.ID, IfRevision: 1, Body: str("v2"), RequestID: name + "-up", Wait: 5 * time.Second},
			}
			for _, req := range reqs {
				envs := make([]Envelope, 6)
				var wg sync.WaitGroup
				for i := range envs {
					wg.Add(1)
					go func(i int) { defer wg.Done(); envs[i] = Run(context.Background(), req) }(i)
				}
				wg.Wait()
				want, _ := json.Marshal(envs[0])
				for _, env := range envs {
					got, _ := json.Marshal(env)
					if !env.OK || string(got) != string(want) {
						t.Fatalf("%s retries diverged:\n%s\n%s", req.Verb, want, got)
					}
				}
			}
			if n := records(t, tc.target, tc.session); n != "2" {
				t.Fatalf("records = %s, want 2", n)
			}
			if cur := must(t, Request{Verb: "inspect", Target: tc.target, Session: tc.session, ID: base.ID}); *cur.Revision != 2 {
				t.Fatalf("concurrent retries applied the update %d times", *cur.Revision-1)
			}
		})
	}
}

// childEnv names the Request an independent child process runs.
const childEnv = "KBMEMORY_CHILD_REQUEST"

// TestChild is the independent client process the process tests exec.
func TestChild(t *testing.T) {
	raw := os.Getenv(childEnv)
	if raw == "" {
		t.Skip("child helper")
	}
	var req Request
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(Run(context.Background(), req))
	fmt.Printf("\nKBMEMORY-ENV %s\n", b)
}

// child runs req in a separate OS process (another principal when episode
// is set) and returns its envelope.
func child(t *testing.T, episode string, req Request) Envelope {
	t.Helper()
	raw, _ := json.Marshal(req)
	cmd := exec.Command(os.Args[0], "-test.run=^TestChild$", "-test.count=1")
	cmd.Env = append(os.Environ(), childEnv+"="+string(raw))
	if episode != "" {
		cmd.Env = append(cmd.Env, "BASHY_EPISODE="+episode)
	}
	out, err := cmd.CombinedOutput()
	for _, line := range strings.Split(string(out), "\n") {
		if rest, ok := strings.CutPrefix(line, "KBMEMORY-ENV "); ok {
			var env Envelope
			if json.Unmarshal([]byte(rest), &env) == nil {
				return env
			}
		}
	}
	t.Fatalf("child %s: %v\n%s", req.Verb, err, out)
	return Envelope{}
}

func parallelChildren(t *testing.T, runs ...func() Envelope) []Envelope {
	out := make([]Envelope, len(runs))
	var wg sync.WaitGroup
	for i, r := range runs {
		wg.Add(1)
		go func() { defer wg.Done(); out[i] = r() }()
	}
	wg.Wait()
	return out
}

func codes(envs []Envelope) (won int, other []string) {
	for _, e := range envs {
		if e.OK {
			won++
		} else if e.Error != nil {
			other = append(other, e.Error.Code)
		}
	}
	return won, other
}

// TestIndependentProcesses: separate client processes contend on one target
// in both storage modes — same request-ID add from one principal,
// same-revision update contention, and a same-principal retry after an
// intervening update. Process-level; the manager runs it on the remote gate
// (skipped under -short).
func TestIndependentProcesses(t *testing.T) {
	if testing.Short() {
		t.Skip("process-level: remote gate")
	}
	dir := isolate(t)
	s := owner(t, dir)
	for name, tc := range map[string]struct{ target, session string }{
		"memory":            {loc("sqlite:mem:" + s + "/procs"), ""},
		"persistent-shared": {loc("sqlite:file:" + filepath.Join(dir, "shared.db")), s},
		"persistent-direct": {loc("sqlite:file:" + filepath.Join(dir, "direct.db")), ""},
	} {
		t.Run(name, func(t *testing.T) {
			must(t, Request{Verb: "init", Target: tc.target, Session: tc.session})
			base := record(t, must(t, Request{Verb: "add", Target: tc.target, Session: tc.session, Body: str("v1")}))
			w := 10 * time.Second
			// Same-revision update contention: exactly one wins.
			envs := parallelChildren(t,
				func() Envelope {
					return child(t, "ep-a", Request{Verb: "update", Target: tc.target, Session: tc.session, ID: base.ID, IfRevision: 1, Body: str("a"), Wait: w})
				},
				func() Envelope {
					return child(t, "ep-b", Request{Verb: "update", Target: tc.target, Session: tc.session, ID: base.ID, IfRevision: 1, Body: str("b"), Wait: w})
				})
			if won, other := codes(envs); won != 1 || len(other) != 1 || (other[0] != "conflict" && other[0] != "busy") {
				t.Fatalf("same-revision contention: %d won, %v", won, other)
			}
			if tc.session == "" && strings.Contains(tc.target, "file") {
				// Request-ID replay is an owner guarantee; the direct path
				// refuses it rather than pretending.
				if e := child(t, "ep-a", Request{Verb: "add", Target: tc.target, Body: str("x"), RequestID: "p1"}); e.Error == nil || e.Error.Code != "unsupported" {
					t.Fatalf("direct request-id = %+v", e)
				}
				return
			}
			// Same request-ID add from one principal in two processes: one record.
			add := Request{Verb: "add", Target: tc.target, Session: tc.session, Body: str("same"), RequestID: name + "-add", Wait: w}
			envs = parallelChildren(t, func() Envelope { return child(t, "ep-a", add) }, func() Envelope { return child(t, "ep-a", add) })
			if won, other := codes(envs); won != 2 || record(t, envs[0]).ID != record(t, envs[1]).ID || record(t, envs[0]).Created != record(t, envs[1]).Created {
				t.Fatalf("same-ID add: %d ok %v %+v", won, other, envs)
			}
			if n := records(t, tc.target, tc.session); n != "2" {
				t.Fatalf("records = %s, want 2", n)
			}
			// Same principal retries u1 after another process's update.
			id := record(t, envs[0]).ID
			u1 := Request{Verb: "update", Target: tc.target, Session: tc.session, ID: id, IfRevision: 1, Body: str("u1"), RequestID: name + "-u1", Wait: w}
			first := child(t, "ep-a", u1)
			if !first.OK || *first.Revision != 2 {
				t.Fatalf("u1: %+v", first.Error)
			}
			if e := child(t, "ep-b", Request{Verb: "update", Target: tc.target, Session: tc.session, ID: id, IfRevision: 2, Body: str("u3"), RequestID: name + "-u3", Wait: w}); !e.OK {
				t.Fatalf("u3: %+v", e.Error)
			}
			retry := child(t, "ep-a", u1)
			if !retry.OK || *retry.Revision != 2 || record(t, retry).Updated != record(t, first).Updated {
				t.Fatalf("u1 retry after u3: %+v", retry)
			}
			if cur := must(t, Request{Verb: "inspect", Target: tc.target, Session: tc.session, ID: id}); *cur.Revision != 3 || record(t, cur).Body != "u3" {
				t.Fatalf("retry mutated: rev %d", *cur.Revision)
			}
		})
	}
}

// parent mounts the memory command under a kb-like root carrying kb's
// persistent store selectors, as bashy does.
func parent(dir string) *cobra.Command {
	root := &cobra.Command{Use: "kb", SilenceErrors: true, SilenceUsage: true,
		PersistentPreRunE: func(*cobra.Command, []string) error { return errors.New("kb store opened") }}
	root.PersistentFlags().String("dir", "", "")
	root.PersistentFlags().String("ring", "", "")
	root.PersistentFlags().Bool("repo", false, "")
	root.PersistentFlags().Bool("user", false, "")
	root.PersistentFlags().String("base-dir", "", "")
	root.AddCommand(newCmd(nil, dir))
	return root
}

func exitCode(err error) int {
	var ee *ExitError
	if errors.As(err, &ee) {
		return ee.Code
	}
	if err != nil {
		return -1
	}
	return 0
}

func TestCLIRefusalsAndCancellation(t *testing.T) {
	dir := isolate(t)
	target := loc("sqlite:file:mem.db")
	run := func(ctx context.Context, in io.Reader, out io.Writer, args ...string) error {
		root := parent(dir)
		root.SetArgs(args)
		root.SetIn(in)
		root.SetOut(out)
		root.SetErr(io.Discard)
		return root.ExecuteContext(ctx)
	}
	if err := run(context.Background(), nil, io.Discard, "memory", "init", target); err != nil {
		t.Fatalf("init under kb root: %v", err)
	}
	for _, flag := range []string{"--dir=x", "--user", "--repo", "--base-dir=x", "--ring=host"} {
		if c := exitCode(run(context.Background(), nil, io.Discard, "memory", flag, "select", target)); c != 2 {
			t.Fatalf("inherited %s exit %d, want 2", flag, c)
		}
	}
	// A stdin that never delivers is bounded by --timeout.
	pr, pw := io.Pipe()
	defer pw.Close()
	if c := exitCode(run(context.Background(), pr, io.Discard, "memory", "--timeout=200ms", "-f", "-", "add", target)); c != 124 {
		t.Fatalf("blocked stdin exit %d, want 124", c)
	}
	// The command's own context cancels it.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c := exitCode(run(ctx, nil, io.Discard, "memory", "--body=b", "add", target)); c != 130 {
		t.Fatalf("cancelled context exit %d, want 130", c)
	}
	// A result nobody received is not a success.
	if err := run(context.Background(), nil, failWriter{}, "memory", "--json", "select", target); exitCode(err) != 1 {
		t.Fatalf("write failure: %v", err)
	}
	// Help's locator spellings are the real escaped locators.
	if long := newCmd(nil, dir).Long; !strings.Contains(long, loc("sqlite:file:PATH")) || !strings.Contains(long, loc("sqlite:mem:SESSION/NAME")) {
		t.Fatalf("help locators are not the escaped S11 spelling:\n%s", long)
	}
	if _, err := dblifecycle.ParseTarget(loc("sqlite:file:" + filepath.Join(dir, "x.db"))); err != nil {
		t.Fatal(err)
	}
}

type failWriter struct{}

func (failWriter) Write([]byte) (int, error) { return 0, errors.New("broken pipe") }
