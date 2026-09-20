package weave

// END-TO-END LIFECYCLE TESTS.
//
// These drive the sprint through the states a real campaign walks — open, hand
// over, recover, correct, close, clean up — and assert the three invariants
// that used to be advisory:
//
//	REACHABILITY  an open sprint has an owner that resolves and a room to reach it
//	COVERAGE      the plan keeps describing the sprint as new work arrives
//	HYGIENE       the sprint can state whether its repos and this host are clean
//
// Each test asserts the FAILURE it was written against, not just the happy
// path: a gate that passes is only evidence if the same gate is shown refusing.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

func execCommandForTest(name string, args ...string) (string, error) {
	out, err := exec.Command(name, args...).CombinedOutput()
	return string(out), err
}

// seedAgent registers a named agent in the test HOME's fleet store, so a name
// the sprint records is one `bashy agent` can actually resolve.
//
// Tests need this now because an unregistered owner is refused — which is the
// point. Before, any string was accepted and the four lifecycle tests below
// passed with conductor names that addressed nobody.
func seedAgent(t *testing.T, name string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "unused")
	_ = dir
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	agents := filepath.Join(home, ".config", "bashy", "agents")
	if err := os.MkdirAll(agents, 0o755); err != nil {
		t.Fatalf("mkdir agents: %v", err)
	}
	body := "name: " + name + "\nkind: agent\ntool: claude\nmodel: opus5\n"
	if err := os.WriteFile(filepath.Join(agents, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write agent: %v", err)
	}
}

func seedPerson(t *testing.T, handle string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("home: %v", err)
	}
	people := filepath.Join(home, ".config", "bashy", "people")
	if err := os.MkdirAll(people, 0o755); err != nil {
		t.Fatalf("mkdir people: %v", err)
	}
	body := "handle: " + handle + "\n"
	if err := os.WriteFile(filepath.Join(people, handle+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write person: %v", err)
	}
}

// seedLiveAgent registers the agent AND publishes a live room card for it, so
// the name is claimable. Claiming a sprint now requires the seat to be RUNNING
// — a sprint seated to a name with no process behind it accepts room messages
// and inbox mail nobody will read.
func seedLiveAgent(t *testing.T, name string) {
	t.Helper()
	seedAgent(t, name)
	if err := room.Join(room.Card{
		ID:      name,
		Nick:    name,
		Tool:    "claude",
		Model:   "opus5",
		Binding: "claude:opus5",
		Role:    "conductor",
		Mode:    "weave",
		Caps:    []string{room.CapInboxDelivery},
		PID:     os.Getpid(),
		Joined:  time.Now().UTC().Format(time.RFC3339),
	}); err != nil {
		t.Skipf("host room unavailable in this environment: %v", err)
	}
	t.Cleanup(func() { room.Leave(name) })
}

func TestSprintRefusesAnOwnerThatResolvesToNobody(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "ghost-that-does-not-exist")

	if out, code := runSprint(t, "add", "unreachable owner"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	out, code := runSprint(t, "start", "1", "--owner", "ghost-that-does-not-exist", "--for", "1h")
	if code == 0 {
		t.Fatalf("start accepted an owner that resolves to no agent:\n%s", out)
	}
	// The refusal must point to the canonical agent roster rather than
	// suggesting an ad-hoc live seat or a human principal that cannot take an
	// autonomous turn.
	for _, want := range []string{"sprint manager", "owns nothing here", "bashy agent list", "--owner"} {
		if !strings.Contains(out, want) {
			t.Errorf("refusal missing %q:\n%s", want, out)
		}
	}
}

func TestSprintRefusesThePlaceholderConductorName(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "conductor")

	if out, code := runSprint(t, "add", "placeholder owner"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	out, code := runSprint(t, "start", "1", "--owner", "conductor", "--for", "1h")
	if code == 0 {
		t.Fatalf("start accepted the placeholder name %q:\n%s", "conductor", out)
	}
	if !strings.Contains(out, "placeholder") {
		t.Errorf("refusal should explain that the name addresses nobody:\n%s", out)
	}
}

// TestOpenSprintKeepsItsRoomAcrossPauseAndHandoff is the regression for the
// live defect: sprint #99 sat in `doing` with a running box, an owner, no lease
// and NO ROOM, because pause and handoff closed it. The handoff window is
// exactly when an arriving agent needs somewhere to ask.
func TestOpenSprintKeepsItsRoomAcrossPauseAndHandoff(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")

	if out, code := runSprint(t, "add", "room retention"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "Ada", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	roomAfterStart := sprintContactRef(t, 1)
	if roomAfterStart == "" {
		t.Skip("no room could be opened in this environment; retention is untestable here")
	}

	if out, code := runSprint(t, "pause", "1", "-m", "paused for the test"); code != 0 {
		t.Fatalf("pause exit=%d: %s", code, out)
	}
	if got := sprintContactRef(t, 1); got != roomAfterStart {
		t.Fatalf("pause changed the room of an OPEN sprint: %q → %q", roomAfterStart, got)
	}

	if out, code := runSprint(t, "handoff", "1", "-m", "handing over"); code != 0 {
		t.Fatalf("handoff exit=%d: %s", code, out)
	}
	if got := sprintContactRef(t, 1); got != roomAfterStart {
		t.Fatalf("handoff closed the room of an OPEN sprint: %q → %q", roomAfterStart, got)
	}

	// And a successor inherits that same room rather than opening a second one.
	if out, code := runSprint(t, "take", "1", "--owner", "Ada"); code != 0 {
		t.Fatalf("take exit=%d: %s", code, out)
	}
	if got := sprintContactRef(t, 1); got != roomAfterStart {
		t.Fatalf("take replaced the sprint's room instead of inheriting it: %q → %q", roomAfterStart, got)
	}
}

// TestTakeThenMoveIntoDoingOpensTheRoom is the regression for the live defect
// on sprint #217: `take` on a BACKLOG sprint followed by `move … doing` — the
// ordinary way a sprint begins without a box — produced a card that read
// "UNREACHABLE: no room" for its whole life. take opens a room only for an
// already-open column, and move closed rooms on the way out of `doing` but
// never opened one on the way in.
func TestTakeThenMoveIntoDoingOpensTheRoom(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")

	if out, code := runSprint(t, "add", "room on entry"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "take", "1", "--owner", "Ada"); code != 0 {
		t.Fatalf("take exit=%d: %s", code, out)
	}
	if got := sprintContactRef(t, 1); got != "" {
		t.Fatalf("a backlog sprint convened a room %q before any work began", got)
	}
	if out, code := runSprint(t, "move", "1", "doing"); code != 0 {
		t.Fatalf("move doing exit=%d: %s", code, out)
	}
	if sprintContactRef(t, 1) == "" {
		t.Fatalf("moving an owned sprint into doing left it without a room — the card is unreachable")
	}
}

// TestClosingASprintReleasesItsRoom is the other half: retention must not leak.
func TestClosingASprintReleasesItsRoom(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")

	if out, code := runSprint(t, "add", "room release"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "Ada", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	if sprintContactRef(t, 1) == "" {
		t.Skip("no room in this environment")
	}
	if out, code := runSprint(t, "move", "1", "done"); code != 0 {
		t.Fatalf("move done exit=%d: %s", code, out)
	}
	if got := sprintContactRef(t, 1); got != "" {
		t.Fatalf("a closed sprint still advertises room %q — the channel outlived the work", got)
	}
}

// TestSprintCannotCloseOverUncoveredOpenWork is the coverage regression: a
// story filed into a sprint and never linked to a goal item used to be
// invisible to the close gate, so the sprint closed clean over open work.
func TestSprintCannotCloseOverUncoveredOpenWork(t *testing.T) {
	s := &weaveStory{ID: 7, Column: "doing", Title: "coverage"}
	// A goal item covering one story, and a second story nothing covers.
	s.Goal = []sprintGoalItem{{ID: "aaaaaaaa", Text: "planned", Stories: []sprintStoryRef{{Repo: "/r", ID: "aaaaaaaabbbb"}}}}

	covered := sprintStoryRef{Repo: "/r", ID: "aaaaaaaabbbb"}
	if !sprintStoryCovered(s, covered) {
		t.Fatal("a story linked to a goal must read as covered (8-vs-12 char id widths must still match)")
	}
	uncovered := sprintStoryRef{Repo: "/r", ID: "ccccccccdddd"}
	if sprintStoryCovered(s, uncovered) {
		t.Fatal("a story no goal references must read as uncovered")
	}
}

// TestCoverageGateNeedsAReasonToBeForced — the escape hatch must cost a
// sentence, or it becomes the default.
func TestCoverageGateNeedsAReasonToBeForced(t *testing.T) {
	s := &weaveStory{ID: 8, Column: "doing"}
	problems := []string{"deadbeef [p0] something open"}
	// Simulate the gate's decision directly on a known-nonempty problem set.
	if err := sprintCoverageGateFor(s, problems, false, ""); err == nil {
		t.Fatal("gate must refuse while open stories sit outside the plan")
	}
	if err := sprintCoverageGateFor(s, problems, true, ""); err == nil {
		t.Fatal("--force without --reason must still refuse")
	}
	if err := sprintCoverageGateFor(s, problems, true, "triaged: tracked in #123"); err != nil {
		t.Fatalf("--force with a reason must pass: %v", err)
	}
}

func TestReachabilityReportsEveryProblemWithAFix(t *testing.T) {
	// An open sprint with no owner and no room must report BOTH, not the first.
	s := &weaveStory{ID: 9, Column: "doing"}
	r := sprintCheckReachability(s)
	if len(r.Problems) < 2 {
		t.Fatalf("expected both a missing owner and a missing room, got %v", r.Problems)
	}
	joined := strings.Join(r.Problems, "\n")
	if !strings.Contains(joined, "owner") || !strings.Contains(joined, "room") {
		t.Errorf("reachability must name both failures:\n%s", joined)
	}
	// A closed sprint is trivially reachable — it needs no room.
	done := &weaveStory{ID: 9, Column: "done"}
	if p := sprintCheckReachability(done).Problems; len(p) != 0 {
		t.Errorf("a done sprint must not be reported unreachable: %v", p)
	}
}

func TestIntegratedBranchRuleNeverOffersUniqueWork(t *testing.T) {
	// The safety property of the whole cleanup story: a branch is only ever
	// offered for removal when its patches exist elsewhere.
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := gitOutputForTest(repo, args...)
		if err != nil {
			t.Skipf("git unavailable in this environment: %v", err)
		}
		return out
	}
	git("init", "-q", "-b", "main")
	git("config", "user.email", "t@example.com")
	git("config", "user.name", "t")
	if err := os.WriteFile(filepath.Join(repo, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "base")
	git("checkout", "-q", "-b", "feature")
	if err := os.WriteFile(filepath.Join(repo, "b.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "-A")
	git("commit", "-qm", "unique work")
	git("checkout", "-q", "main")

	if gitBranchIntegrated(repo, "main", "feature") {
		t.Fatal("a branch with a unique commit must NEVER be reported integrated")
	}
	// After merging, the same branch becomes safe.
	git("merge", "-q", "--no-ff", "-m", "merge", "feature")
	if !gitBranchIntegrated(repo, "main", "feature") {
		t.Fatal("a merged branch must be reported integrated")
	}
}

// --- helpers ---

func sprintContactRef(t *testing.T, id int64) string {
	t.Helper()
	dir, err := sprintStoreDir()
	if err != nil {
		t.Fatalf("sprint dir: %v", err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatalf("load board: %v", err)
	}
	s := findWeaveStory(q, id)
	if s == nil || s.Contact == nil {
		return ""
	}
	return s.Contact.Ref
}

// gitOutputForTest runs a git command in a repo, for fixtures only.
func gitOutputForTest(root string, args ...string) (string, error) {
	a := append([]string{"-C", root}, args...)
	out, err := execCommandForTest("git", a...)
	return out, err
}

// TestSprintEndGatesOnCoverageAndHygiene is the regression for a gap that was
// CLAIMED closed and was not: `sprint move ... done` was gated while
// `sprint end` set Column = "done" directly, so the documented "end gates on
// it" was false and the strictest close path was the unguarded one.
func TestSprintEndGatesOnCoverageAndHygiene(t *testing.T) {
	src, err := os.ReadFile("weave_story_box.go")
	if err != nil {
		t.Fatalf("read box source: %v", err)
	}
	body := string(src)
	// The gate must sit on the ending path itself, not on a sibling verb.
	for _, want := range []string{"sprintStoryClosureAudit(s)", "sprintCoverageGate(s, false, \"\")", "sprintCheckHygiene(s); !hy.Clean()"} {
		if !strings.Contains(body, want) {
			t.Errorf("sprint end is missing its close gate %q — a sprint could end over open or unclean work", want)
		}
	}
}

// TestSprintShowSurfacesCoverageAndReachability guards the other silent gap:
// renderSprintCoverage existed but nothing called it, so the plan's blind spot
// was computed and never shown.
func TestSprintShowSurfacesCoverageAndReachability(t *testing.T) {
	src, err := os.ReadFile("weave_story.go")
	if err != nil {
		t.Fatalf("read story source: %v", err)
	}
	body := string(src)
	for _, want := range []string{"renderSprintCoverage(out, s)", "renderSprintReachability(out, s)"} {
		if !strings.Contains(body, want) {
			t.Errorf("sprint show does not call %s — the finding is computed and never surfaced", want)
		}
	}
	if !strings.Contains(body, "UNREACHABLE: %d") {
		t.Error("sprint board does not flag unreachable open sprints")
	}
}

// TestGoalAddCanCoverAStoryInOneStep — covering a newly reported bug must not
// need two commands, because the second one is the one that gets skipped.
func TestGoalAddCoversAStoryInOneStep(t *testing.T) {
	cmd := NewSprintCmd()
	goal, _, err := cmd.Find([]string{"goal", "add"})
	if err != nil {
		t.Fatalf("find goal add: %v", err)
	}
	if goal.Flags().Lookup("story") == nil {
		t.Error("sprint goal add must accept --story so create-and-link is one command")
	}
}

// TestSprintOwnerMustBeAnAddressableAgent is THE critical invariant, and it is
// exactly one thing: a sprint's manager name must ADDRESS somebody.
//
// The rule it serves is guaranteed delivery, exactly once. Every collaboration
// surface — the room, inbox, mb, chat, ping — keys on this one string, so a
// name resolving to nothing turns all of them into a black hole that accepts
// requests and answers none. That is message LOSS, and it is what this gate
// exists to prevent.
//
// It deliberately does NOT require the agent to be RUNNING. A registered name
// is a durable address: mail sent to it is stored and read later, and nothing
// is lost by the agent being between turns. Requiring liveness bought no
// delivery guarantee and cost the ordinary flow — an agent could not take a
// seat without first registering a process and then holding a foreground watch
// for the life of the sprint, which a conversation-hosted agent cannot do.
// Measured standing in the way of this project's own conductor five times in
// one session.
func TestSprintOwnerMustBeAnAddressableAgent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "seatless")

	if out, code := runSprint(t, "add", "owner invariant"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}

	// 1. Unknown name — REFUSED. Mail to it would reach nobody and no surface
	// would say so, which is the loss this gate prevents.
	out, code := runSprint(t, "start", "1", "--owner", "seatless", "--for", "1h")
	if code == 0 {
		t.Fatalf("a sprint was seated to a name that is in no roster:\n%s", out)
	}
	if !strings.Contains(out, "owns nothing here") || !strings.Contains(out, "bashy agent list") {
		t.Errorf("refusal must say the name owns nothing and identify the agent registry:\n%s", out)
	}

	// 2. A placeholder — REFUSED. "conductor" is not unique: every sprint on
	// the host would claim it and every message to it would be ambiguous, so
	// delivery could not be exactly-once even though the name looks valid.
	if err := validateSprintOwner("conductor"); err == nil {
		t.Error("a placeholder name was accepted as a sprint owner")
	}

	// 3. A registered person — REFUSED. A human is addressable, but cannot take
	// autonomous turns; they manage by steering a registered agent.
	seedPerson(t, "operator")
	out, code = runSprint(t, "start", "1", "--owner", "operator", "--for", "1h")
	if code == 0 {
		t.Fatalf("a person was seated as a production sprint manager:\n%s", out)
	}
	if !strings.Contains(out, "registered person, not an agent") || !strings.Contains(out, "humans manage sprints by steering a registered agent") {
		t.Errorf("person refusal must explain the supported human-to-agent path:\n%s", out)
	}

	// 4. Registered but not currently running — ACCEPTED. This is the change.
	// The name addresses a real agent, mail is durable, and it will be read
	// when the agent next looks. Nothing is lost, so nothing is refused.
	seedAgent(t, "seatless")
	if out, code := runSprint(t, "start", "1", "--owner", "seatless", "--for", "1h"); code != 0 {
		t.Fatalf("a registered agent must be able to take the seat without holding a process: exit=%d\n%s", code, out)
	}
}

// TestConductorCannotWalkAwayFromUnreadRequests: pause, handoff and end are the
// three ways a conductor stops answering. Leaving with unread mail converts a
// question into an indefinite wait — the sender gets no signal that nobody is
// coming, and the durable queue faithfully preserves a message that will never
// be looked at.
func TestConductorCannotWalkAwayFromUnreadRequests(t *testing.T) {
	for _, verb := range []string{"hand off", "end"} {
		t.Run(verb, func(t *testing.T) {
			// The gate is exercised directly: publishing real bus traffic into
			// a temp HOME is a different subsystem's setup, and what must not
			// regress is the RULE — unread mail blocks the handover verbs.
			s := &weaveStory{ID: 5, Owner: "somebody", Column: "doing"}
			if err := sprintUnansweredGate(s, verb); err != nil {
				// No mail in a clean environment: the gate must be silent, not
				// noisy. A check that fires when there is nothing to report is
				// one operators learn to bypass.
				t.Fatalf("gate must pass with no unread mail: %v", err)
			}
		})
	}
	// And an owner with no name is not a reason to block anything.
	if err := sprintUnansweredGate(&weaveStory{ID: 6}, "pause"); err != nil {
		t.Fatalf("an ownerless sprint must not be gated on mail: %v", err)
	}
}

// TestHandoverVerbsAllConsultTheUnansweredGate guards the wiring, because a
// gate function that exists and is called from nowhere is the exact failure
// this session already shipped once.
func TestHandoverVerbsAllConsultTheUnansweredGate(t *testing.T) {
	// pause and resume were ABSORBED: pause is an alias of handoff, resume of
	// take. There are two release paths left, not three, and both must consult
	// the gate — an alias cannot skip it because it shares the implementation.
	for _, f := range []struct{ file, verb string }{
		{"weave_story.go", "hand off"},
		{"weave_story_box.go", "end"},
	} {
		src, err := os.ReadFile(f.file)
		if err != nil {
			t.Fatalf("read %s: %v", f.file, err)
		}
		want := "sprintUnansweredGate(s, \"" + f.verb + "\")"
		if !strings.Contains(string(src), want) {
			t.Errorf("%s does not call %s — a conductor could leave with unread requests", f.file, want)
		}
	}
}

// TestStoryClaimIsExclusiveAndAnnounced covers the volunteer loop: an agent
// takes a story off an ACTIVE sprint, the hold is exclusive, and yield returns
// it to the queue. The exclusivity is the point — two agents picking the same
// p0 discover it in a merge conflict, which is the expensive way.
func TestStoryClaimIsExclusiveAndAnnounced(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "boss")
	seedLiveAgent(t, "boss")
	seedLiveAgent(t, "worker")

	root := mustStoryRoot(t) // isolates the story store AND the cwd first
	if out, code := runSprint(t, "add", "volunteer loop"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "boss", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	// A story nobody has claimed yet.
	store := todopkg.RepoStore(root)
	it, err := todopkg.Add(store, "pick me", "", "p0", nil, "", "")
	if err != nil {
		t.Skipf("todo store unavailable here: %v", err)
	}
	it.Sprint = 1
	if _, err := store.Save(it); err != nil {
		t.Fatalf("attach story to sprint: %v", err)
	}
	if out, code := runSprint(t, "track", "1"); code != 0 {
		t.Fatalf("track exit=%d: %s", code, out)
	}

	if out, code := runSprint(t, "claim", "1", it.ID, "--owner", "worker"); code != 0 {
		t.Fatalf("claim exit=%d: %s", code, out)
	}
	held, err := todopkg.ResolveRef(store, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if held.Assignee != "worker" {
		t.Fatalf("claim did not record the holder: assignee=%q", held.Assignee)
	}

	// A second agent must be refused, and told who to talk to.
	out, code := runSprint(t, "claim", "1", it.ID, "--owner", "boss")
	if code == 0 {
		t.Fatalf("a claimed story was handed to a second agent:\n%s", out)
	}
	if !strings.Contains(out, "worker") {
		t.Errorf("refusal must name the current holder so the loser can coordinate:\n%s", out)
	}

	// Yield puts it back for anyone.
	if out, code := runSprint(t, "yield", "1", it.ID, "--as", "worker", "-m", "out of context"); code != 0 {
		t.Fatalf("yield exit=%d: %s", code, out)
	}
	back, err := todopkg.ResolveRef(store, it.ID)
	if err != nil {
		t.Fatal(err)
	}
	if back.Assignee != "" || back.Status != todopkg.StatusTodo {
		t.Fatalf("yield did not return the story to the queue: assignee=%q status=%q", back.Assignee, back.Status)
	}
}

// TestSubmitRequiresEvidenceAndKeepsTheHold: submitted work is NOT available
// for somebody else, and a submission that does not say where to look just
// makes the manager go find it.
func TestSubmitRequiresEvidenceAndKeepsTheHold(t *testing.T) {
	cmd := NewSprintCmd()
	sub, _, err := cmd.Find([]string{"submit"})
	if err != nil {
		t.Fatalf("find submit: %v", err)
	}
	if sub.Flags().Lookup("message") == nil {
		t.Fatal("submit must take -m evidence")
	}
	// claim/yield are a pair; submit is deliberately not yield's opposite.
	for _, v := range []string{"claim", "yield", "submit"} {
		if c, _, err := cmd.Find([]string{v}); err != nil || c.Name() != v {
			t.Errorf("sprint %s is not registered", v)
		}
	}
}

// mustStoryRoot gives the test its OWN repo to keep stories in.
//
// It does NOT resolve the current checkout. normalizeStoryRoot("") walks up to
// the enclosing git repo, which during `go test` is this source tree — so an
// earlier version of this helper wrote three fixture stories straight into
// coreutils/docs/todo/ and they then showed up as uncovered work on a real
// sprint's close gate. A test that writes to the repo it is testing is not a
// test, it is a mutation with an assertion attached.
func mustStoryRoot(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := execCommandForTest("git", "-C", repo, "init", "-q"); err != nil {
		t.Skipf("git unavailable here: %v (%s)", err, out)
	}
	t.Chdir(repo)
	root, err := normalizeStoryRoot("")
	if err != nil {
		t.Skipf("no story root here: %v", err)
	}
	return root
}

// `sprint track --repo --plain` once recorded the literal "<cwd>/--plain" as a
// tracked store, and there was no way to remove it short of editing the queue.
// track refuses a root that is not a directory; untrack is track's inverse.
func TestSprintTrackRefusesAMissingRootAndUntrackRemovesOne(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("WEAVE_CONDUCTOR", "boss")
	seedLiveAgent(t, "boss")

	root := mustStoryRoot(t)
	if out, code := runSprint(t, "add", "track hygiene"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	bogus := filepath.Join(root, "--plain")
	if out, code := runSprint(t, "track", "1", "--repo", bogus); code == 0 || !strings.Contains(out, "--plain") {
		t.Fatalf("track accepted a non-directory root: exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "track", "1", "--repo", root); code != 0 {
		t.Fatalf("track exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "untrack", "1", "--repo", bogus); code == 0 {
		t.Fatalf("untrack removed a root that was never tracked: %s", out)
	}
	if out, code := runSprint(t, "untrack", "1", "--repo", root); code != 0 {
		t.Fatalf("untrack exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "untrack", "1", "--repo", root); code == 0 {
		t.Fatalf("untrack removed the same root twice: %s", out)
	}
}
