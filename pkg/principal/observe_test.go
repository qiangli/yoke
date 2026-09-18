package principal

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// obsEnv is the hermetic test env with the three observation stores rooted
// in a temp dir, so tests plant traces one at a time.
func obsEnv(t *testing.T) Env {
	t.Helper()
	e := testEnv(t)
	root := t.TempDir()
	e.BoardDir = filepath.Join(root, "mb")
	e.MeetDir = filepath.Join(root, "meet")
	e.RoomDir = filepath.Join(root, "room")
	return e
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The defect this store fixes: a live seat with a board cursor and a hundred
// posts answered "names nothing on this host" because whois read catalogs
// only. An observed-only name must resolve, marked as observed and inferred
// so a caller can tell it from a declared identity.
func TestObservedOnlyNameResolvesWithSourceObserved(t *testing.T) {
	env := obsEnv(t)
	writeFile(t, filepath.Join(env.BoardDir, "seen", "codex-profile-c"), "33\n")
	writeFile(t, filepath.Join(env.BoardDir, "posts.jsonl"), strings.Join([]string{
		`{"schema_version":"bashy-mb-v1","seq":1,"at":"2026-08-25T10:00:00Z","from":"codex-profile-c","topic":"mb","body":"polling"}`,
		`{"schema_version":"bashy-mb-v1","seq":2,"at":"2026-08-25T11:00:00Z","from":"someone-else","topic":"mb","body":"noise"}`,
		`{"schema_version":"bashy-mb-v1","seq":3,"at":"2026-08-25T12:00:00Z","from":"codex-profile-c","topic":"mb","body":"still here"}`,
	}, "\n")+"\n")
	r, _ := testResolver(t, env)

	ans := r.Resolve("codex-profile-c")
	if !ans.Resolved {
		t.Fatal("an observed-only name must resolve")
	}
	if ans.Ambiguous() {
		t.Fatalf("expected one match, got %v", ans.Kinds())
	}
	res := ans.Matches[0]
	if res.Kind != KindAgent {
		t.Fatalf("kind = %q, want agent", res.Kind)
	}
	if res.Source != SourceObserved {
		t.Fatalf("source = %q, want %q", res.Source, SourceObserved)
	}
	if res.Confidence != Inferred {
		t.Fatalf("an observed identity is inferred, not %q", res.Confidence)
	}
	if len(res.Facts) < 2 {
		t.Fatalf("expected cursor + posts evidence, got %+v", res.Facts)
	}
	best, ok := res.Best()
	if !ok {
		t.Fatal("an observed seat must have a live contact (the board)")
	}
	if best.Method != "mb" || !strings.Contains(best.Address, "codex-profile-c") {
		t.Fatalf("best = %+v, want a directed board post", best)
	}

	// The typed forms behave: agent:name resolves, person:name does not.
	if got := r.Resolve("agent:codex-profile-c"); !got.Resolved {
		t.Fatal("agent:codex-profile-c must resolve")
	}
	if got := r.Resolve("person:codex-profile-c"); got.Resolved {
		t.Fatalf("person:codex-profile-c resolved: %+v", got.Matches)
	}
}

// A weave worker's only trace is often its bus subscription file.
func TestBusSubscriptionAloneIsObservation(t *testing.T) {
	env := obsEnv(t)
	writeFile(t, filepath.Join(env.RoomDir, "subs", "agy-gemini3.6-flash-w444.json"), "{}\n")
	r, _ := testResolver(t, env)
	ans := r.Resolve("agy-gemini3.6-flash-w444")
	if !ans.Resolved || ans.Matches[0].Source != SourceObserved {
		t.Fatalf("a subscribed worker must resolve as observed: %+v", ans)
	}
}

// A catalog name is unchanged: same single match, declared fleet identity,
// spawn-first contact ladder for a target with no trace on this host.
func TestCatalogNameUnchangedByObservationSource(t *testing.T) {
	fleettest.Ring(t)
	env := obsEnv(t)
	r, cat := testResolver(t, env)
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	ans := r.Resolve("007")
	if !ans.Resolved || ans.Ambiguous() {
		t.Fatalf("007 = %+v", ans)
	}
	res := ans.Matches[0]
	if res.Source != SourceFleet || res.Confidence != Declared {
		t.Fatalf("a catalog entry is fleet/declared, got %q/%q", res.Source, res.Confidence)
	}
	if len(res.Contacts) != 2 || res.Contacts[0].Method != "cli" || res.Contacts[1].Method != "chat" {
		t.Fatalf("a non-live catalog agent keeps the cli-first ladder: %+v", res.Contacts)
	}
	for _, f := range res.Facts {
		if f[0] == "live" {
			t.Fatalf("no trace, yet marked live: %+v", res.Facts)
		}
	}
}

// The ranking defect: cli (spawn a NEW instance, cost 10) outranked chat
// (cost 20) even for an agent already running here. A fresh trace must rank
// the async channels above cli.
func TestRankingPrefersAsyncForALiveTarget(t *testing.T) {
	fleettest.Ring(t)
	env := obsEnv(t)
	r, cat := testResolver(t, env)
	if err := cat.SaveAgent(fleet.Agent{Name: "smarty", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	// A cursor written now = the seat polled just now.
	writeFile(t, filepath.Join(env.BoardDir, "seen", "smarty"), "112\n")

	res := r.Resolve("smarty").Matches[0]
	if res.Contacts[0].Method == "cli" {
		t.Fatalf("resolver recommends SPAWNING a live agent: %+v", res.Contacts)
	}
	mb := contactOf(t, res, "mb")
	if !mb.Live || mb.Source != SourceObserved {
		t.Fatalf("the board contact for a live seat = %+v", mb)
	}
	chat := contactOf(t, res, "chat")
	cli := contactOf(t, res, "cli")
	if chat.Cost >= cli.Cost {
		t.Fatalf("chat (cost %d) must rank cheaper than cli (cost %d) for a live target", chat.Cost, cli.Cost)
	}
	var sawLive bool
	for _, f := range res.Facts {
		if f[0] == "live" {
			sawLive = true
		}
	}
	if !sawLive {
		t.Fatalf("liveness must be stated as a fact: %+v", res.Facts)
	}

	// The same seat gone quiet for two days is not live: back to cli-first.
	stale := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(filepath.Join(env.BoardDir, "seen", "smarty"), stale, stale); err != nil {
		t.Fatal(err)
	}
	res = r.Resolve("smarty").Matches[0]
	if res.Contacts[0].Method != "cli" {
		t.Fatalf("a stale trace must not rank async first: %+v", res.Contacts)
	}
}

// A meet's human seat is person evidence, and a name observed as both an
// agent seat and a human is ambiguity to surface — the exit-3 rule, applied
// to observation exactly as to the catalogs.
func TestMeetRosterKindsAndAmbiguity(t *testing.T) {
	env := obsEnv(t)
	writeFile(t, filepath.Join(env.MeetDir, "2026-08-25-triage-abcd", "state.json"),
		`{"schema":"bashy-meet-v1","id":"2026-08-25-triage-abcd","participants":["worker-9"],"secretary":"worker-9","human":"pat","status":"open"}`)
	r, _ := testResolver(t, env)

	if got := r.Resolve("worker-9"); !got.Resolved || got.Matches[0].Kind != KindAgent {
		t.Fatalf("a roster participant is an observed agent: %+v", got)
	}
	pat := r.Resolve("pat")
	if !pat.Resolved || pat.Matches[0].Kind != KindPerson || pat.Matches[0].Source != SourceObserved {
		t.Fatalf("a meet human is an observed person: %+v", pat)
	}

	// Now "pat" also polls the board — two kinds, one name.
	writeFile(t, filepath.Join(env.BoardDir, "seen", "pat"), "1\n")
	both := r.Resolve("pat")
	if !both.Ambiguous() {
		t.Fatalf("agent+person observation must surface as ambiguity, got %v", both.Kinds())
	}
}

// Traces never overrule a catalog: a name the fleet declares resolves from
// the catalog even when the board knows it too, and observation must not add
// a second match for it.
func TestCatalogShadowsObservation(t *testing.T) {
	env := obsEnv(t)
	r, cat := testResolver(t, env)
	if err := cat.SaveAgent(fleet.Agent{Name: "007", Tool: "claude", Model: "fable"}); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(env.BoardDir, "seen", "007"), "5\n")
	ans := r.Resolve("007")
	if ans.Ambiguous() {
		t.Fatalf("observation duplicated a catalog name: %v", ans.Kinds())
	}
	if ans.Matches[0].Source != SourceFleet {
		t.Fatalf("the declared identity must win: %q", ans.Matches[0].Source)
	}
}

// A hostile name must not walk out of the stores.
func TestObservationRejectsPathShapedNames(t *testing.T) {
	env := obsEnv(t)
	r, _ := testResolver(t, env)
	for _, q := range []string{"../../etc/passwd", `a\b`, "x/y"} {
		if ans := r.Resolve(q); ans.Resolved {
			t.Fatalf("Resolve(%q) resolved: %+v", q, ans.Matches)
		}
	}
}
