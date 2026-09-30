package weave

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
)

// Story #1205: a successful `sprint end` scores the manager and appends ONE
// rated manage event to the ladder ledger.

// scorecardEndFixture is a started sprint #1 owned by Ada (claude:opus5 in the
// seeded fleet) with every bashy store under one scratch home.
func scorecardEndFixture(t *testing.T) (home string) {
	t.Helper()
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, ".bashy", "sprint"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, ".bashy", "room"))
	t.Setenv("BASHY_AGENTIC", "")
	t.Setenv("WEAVE_CONDUCTOR", "Ada")
	seedLiveAgent(t, "Ada")
	if out, code := runSprint(t, "add", "scorecard test"); code != 0 {
		t.Fatalf("add exit=%d: %s", code, out)
	}
	if out, code := runSprint(t, "start", "1", "--owner", "Ada", "--for", "1h"); code != 0 {
		t.Fatalf("start exit=%d: %s", code, out)
	}
	// The manager's own `end` carries its lease token; without it the
	// should-phase records a bypass, which the scorecard must (and does) zero.
	raw := readSprintLeaseToken(1, "Ada")
	if raw == "" {
		t.Fatal("start minted no lease token for Ada")
	}
	t.Setenv(sprintLeaseTokenEnv, raw)
	return home
}

func scorecardAppend(t *testing.T, events ...ladder.Event) {
	t.Helper()
	st, err := ladder.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range events {
		if err := st.Append(e); err != nil {
			t.Fatalf("append %+v: %v", e, err)
		}
	}
}

func scorecardManageEvents(t *testing.T) []ladder.Event {
	t.Helper()
	st, err := ladder.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	events, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	var out []ladder.Event
	for _, e := range events {
		if e.Kind == ladder.EventKindManage {
			out = append(out, e)
		}
	}
	return out
}

func scorecardThread(t *testing.T, home, kind string) []weaveComment {
	t.Helper()
	q, err := loadWeaveQueue(filepath.Join(home, ".bashy", "sprint"))
	if err != nil {
		t.Fatal(err)
	}
	var out []weaveComment
	for _, c := range findWeaveStory(q, 1).Thread {
		if c.Kind == kind {
			out = append(out, c)
		}
	}
	return out
}

func scorecardRating(st ladder.DutyStanding) ladder.Rating {
	return ladder.Rating{R: st.R, RD: st.RD, Vol: ladder.InitialVol}
}

func TestSprintEndScorecardRecordsOneManageEvent(t *testing.T) {
	home := scorecardEndFixture(t)
	at := time.Now().UTC().Add(-time.Hour)
	prior := ladder.Event{ID: "p1", At: at.Add(-time.Hour), Season: 1, Kind: ladder.EventKindDelivery,
		Agent: "agent-a", Story: "prior", Points: 3, Outcome: 1, Sprint: 7}
	scorecardAppend(t, prior,
		ladder.Event{ID: "d1", At: at, Season: 1, Kind: ladder.EventKindDelivery,
			Agent: "agent-a", Story: "s1", Points: 3, Outcome: 1, Sprint: 1},
		ladder.Event{ID: "d2", At: at, Season: 1, Kind: ladder.EventKindDelivery,
			Agent: "agent-b", Story: "s2", Points: 2, Outcome: 0.5, Sprint: 1},
	)

	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("end exit=%d: %s", code, out)
	}

	// Independent expectation: ratings are the ledger BEFORE the sprint; the
	// pool's best agent (agent-a, the only one with a prior rated event) is
	// what agent-b's assignment is measured against.
	season := ladder.SeasonOf(time.Now())
	pre := ladder.Replay([]ladder.Event{prior}, season)
	aR := scorecardRating(pre.Agents["agent-a"].Standings[ladder.DutyCode])
	r3, _ := ladder.StoryInitialRating(3)
	r2, _ := ladder.StoryInitialRating(2)
	assignments := []ladder.Assignment{
		{Story: "s1", Points: 3, StoryRating: r3, Agent: "agent-a", AgentRating: aR, BestAvailable: aR, Outcome: 1, Rated: true},
		{Story: "s2", Points: 2, StoryRating: r2, Agent: "agent-b", AgentRating: ladder.NewRating(), BestAvailable: aR, Outcome: 0.5, Rated: true},
	}
	want, err := ladder.ComputeScorecard(ladder.ScorecardInput{
		Assignments: assignments, HygieneChecksPassed: 1, HygieneChecksTotal: 1,
	}, ladder.DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	if want.Regret <= 0 {
		t.Fatalf("fixture must exercise regret, got %v", want.Regret)
	}

	got := scorecardManageEvents(t)
	if len(got) != 1 {
		t.Fatalf("want exactly one manage event, got %d: %+v", len(got), got)
	}
	e := got[0]
	if e.Agent != "claude:opus5" || e.Duty != ladder.DutyManage || e.Sprint != 1 || e.Season != season {
		t.Fatalf("manage event identity wrong: %+v", e)
	}
	if math.Abs(e.Score-want.Score) > 1e-9 {
		t.Fatalf("Score = %v, want %v", e.Score, want.Score)
	}
	if opp := ladder.SprintOpponent(assignments); math.Abs(e.Opponent.R-opp.R) > 1e-9 || e.Opponent.RD != opp.RD {
		t.Fatalf("Opponent = %+v, want %+v", e.Opponent, opp)
	}
	if !strings.Contains(e.Note, "delivery=") || !strings.Contains(e.Note, "hygiene=1.00") {
		t.Fatalf("Note must carry the compact components: %q", e.Note)
	}
	for _, s := range []string{"scorecard", "expected", "regret", "unrated 0", "supervisor instructions: 0", "auto-fails: none"} {
		if !strings.Contains(out, s) {
			t.Fatalf("end output must print the scorecard (%q missing):\n%s", s, out)
		}
	}
	if th := scorecardThread(t, home, "scorecard"); len(th) != 1 || !strings.Contains(th[0].Body, "regret") {
		t.Fatalf("want one scorecard thread event with the summary, got %+v", th)
	}
}

func TestSprintEndScorecardBypassForcesZero(t *testing.T) {
	home := scorecardEndFixture(t)
	scorecardAppend(t, ladder.Event{ID: "d1", At: time.Now().UTC(), Season: 1, Kind: ladder.EventKindDelivery,
		Agent: "agent-a", Story: "s1", Points: 3, Outcome: 1, Sprint: 1})
	dir := filepath.Join(home, ".bashy", "sprint")
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	weaveStoryAppend(findWeaveStory(q, 1), "Ada", "bypass", "sprint accept")
	weaveStoryAppend(findWeaveStory(q, 1), "operator", "instruct", "wrap up")
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}

	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("end exit=%d: %s", code, out)
	}
	got := scorecardManageEvents(t)
	if len(got) != 1 || got[0].Score != 0 || !strings.Contains(got[0].Note, string(ladder.AutoFailDetectedBypass)) {
		t.Fatalf("a detected bypass must record Score 0 with the auto-fail named: %+v", got)
	}
	if !strings.Contains(out, "auto-fails: "+string(ladder.AutoFailDetectedBypass)) || !strings.Contains(out, "supervisor instructions: 1") {
		t.Fatalf("output must report the auto-fail and the instruction count:\n%s", out)
	}
}

func TestSprintEndScorecardNoDeliveriesIsNeutral(t *testing.T) {
	scorecardEndFixture(t)
	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("end exit=%d: %s", code, out)
	}
	got := scorecardManageEvents(t)
	if len(got) != 1 {
		t.Fatalf("want one manage event, got %+v", got)
	}
	// delivery 0.5 (neutral), review 1, breakdown 1, efficiency 0.5 (not
	// recorded), hygiene 1.
	if want := 0.35*0.5 + 0.25 + 0.15 + 0.15*0.5 + 0.10; math.Abs(got[0].Score-want) > 1e-9 {
		t.Fatalf("Score = %v, want %v\n%s", got[0].Score, want, out)
	}
	if !strings.Contains(out, "no rated assignments") || !strings.Contains(got[0].Note, "delivery=0.50") {
		t.Fatalf("neutral delivery must be noted:\nnote=%q\n%s", got[0].Note, out)
	}
}

func TestSprintEndNoScorecardWritesNothing(t *testing.T) {
	home := scorecardEndFixture(t)
	out, code := runSprint(t, "end", "1", "--no-scorecard")
	if code != 0 {
		t.Fatalf("end exit=%d: %s", code, out)
	}
	if got := scorecardManageEvents(t); len(got) != 0 {
		t.Fatalf("--no-scorecard must write no manage event: %+v", got)
	}
	if !strings.Contains(out, "scorecard skipped") {
		t.Fatalf("the skip must be said:\n%s", out)
	}
	if th := scorecardThread(t, home, "scorecard"); len(th) != 1 || !strings.Contains(th[0].Body, "skipped") {
		t.Fatalf("the skip must be logged on the thread: %+v", th)
	}
}

func TestSprintEndScorecardUnresolvableManagerWritesNothing(t *testing.T) {
	scorecardEndFixture(t)
	scorecardAppend(t, ladder.Event{ID: "d1", At: time.Now().UTC(), Season: 1, Kind: ladder.EventKindDelivery,
		Agent: "agent-a", Story: "s1", Points: 3, Outcome: 1, Sprint: 1})
	// The fleet registry no longer knows Ada: no tool:model to rate.
	empty := t.TempDir()
	prev := fleetCatalog
	fleetCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(empty)) }
	t.Cleanup(func() { fleetCatalog = prev })

	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("an unresolvable manager must never fail end: exit=%d: %s", code, out)
	}
	if got := scorecardManageEvents(t); len(got) != 0 {
		t.Fatalf("unresolvable manager must record nothing: %+v", got)
	}
	if !strings.Contains(out, "not recorded") || !strings.Contains(out, "Ada") {
		t.Fatalf("end must say why nothing was recorded:\n%s", out)
	}
}

// The season comes from ladder.SeasonOf (no local fallback), so its
// BASHY_LADDER_SEASON override reaches the recorded manage event.
func TestSprintEndScorecardSeasonIsLadderSeasonOf(t *testing.T) {
	scorecardEndFixture(t)
	t.Setenv("BASHY_LADDER_SEASON", "3")
	if out, code := runSprint(t, "end", "1"); code != 0 {
		t.Fatalf("end exit=%d: %s", code, out)
	}
	got := scorecardManageEvents(t)
	if len(got) != 1 || got[0].Season != 3 {
		t.Fatalf("manage event must carry ladder.SeasonOf's season 3: %+v", got)
	}
}

func scorecardSpecBlame() blame.Attribution {
	return blame.Attribution{Class: blame.ClassSpec, By: "reviewer",
		Evidence: []blame.Evidence{{Kind: blame.EvidenceContradiction, Ref: "thread-1", Note: "acceptance contradicts spec"}}}
}

// Story #1197/#1205: SpecFailures counts this sprint's failed deliveries with
// a valid spec-class attribution authored by the manager (an empty Author is
// the manager's by default; an empty manager counts every author).
func TestSprintScorecardInputCountsManagerSpecFailures(t *testing.T) {
	at := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	d := func(id, author string, sprint int, outcome float64, attr blame.Attribution) ladder.Event {
		return ladder.Event{ID: id, At: at, Season: 1, Kind: ladder.EventKindDelivery, Agent: "agent-a",
			Story: "s-" + id, Points: 2, Outcome: outcome, Sprint: sprint, Author: author, Blame: attr}
	}
	invalid := scorecardSpecBlame()
	invalid.Evidence[0].Note = ""
	env := blame.Attribution{Class: blame.ClassEnvironment, By: "reviewer",
		Evidence: []blame.Evidence{{Kind: blame.EvidenceQuota, Ref: "q"}}}
	events := []ladder.Event{
		d("mine", "tool:model-m", 1, 0, scorecardSpecBlame()),
		d("byname", "Ada", 1, 0, scorecardSpecBlame()),
		d("unauthored", "", 1, 0, scorecardSpecBlame()),
		d("other", "tool:model-x", 1, 0, scorecardSpecBlame()),
		d("invalid", "tool:model-m", 1, 0, invalid),
		d("env", "tool:model-m", 1, 0, env),
		d("othersprint", "tool:model-m", 2, 0, scorecardSpecBlame()),
	}
	if got := sprintScorecardInput(events, 1, 1, "tool:model-m", "Ada").SpecFailures; got != 3 {
		t.Fatalf("manager SpecFailures = %d, want 3 (mine, byname, unauthored)", got)
	}
	if got := sprintScorecardInput(events, 1, 1).SpecFailures; got != 4 {
		t.Fatalf("no manager: SpecFailures = %d, want 4 (every valid spec failure)", got)
	}
	corr := ladder.Event{ID: "c", At: at, Season: 1, Kind: ladder.EventKindCorrection, Supersedes: "mine"}
	if got := sprintScorecardInput(append(events, corr), 1, 1, "tool:model-m", "Ada").SpecFailures; got != 2 {
		t.Fatalf("a corrected delivery must not count: SpecFailures = %d, want 2", got)
	}
}

func TestSprintEndScorecardChargesManagerSpecFailure(t *testing.T) {
	scorecardEndFixture(t)
	scorecardAppend(t, ladder.Event{ID: "d1", At: time.Now().UTC(), Season: 1, Kind: ladder.EventKindDelivery,
		Agent: "agent-a", Story: "s1", Points: 3, Outcome: 0, Sprint: 1, Author: "claude:opus5", Blame: scorecardSpecBlame()})
	if out, code := runSprint(t, "end", "1"); code != 0 {
		t.Fatalf("end exit=%d: %s", code, out)
	}
	got := scorecardManageEvents(t)
	if len(got) != 1 || !strings.Contains(got[0].Note, "breakdown=0.80") {
		t.Fatalf("one manager spec failure must cost breakdown 0.2: %+v", got)
	}
}

func TestSprintScorecardRecordedCosts(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	var events []ladder.Event
	for i, cost := range []float64{200, 2, 8, 4, 100} {
		e := ladder.Event{ID: fmt.Sprint(i), At: at.Add(time.Duration(i) * time.Hour), Season: 1, Kind: ladder.EventKindDelivery, Sprint: i + 1, Agent: "agent-a", Story: fmt.Sprint(i), Points: 2, Outcome: 1, Cost: cost}
		e.CapsUsed.WallSeconds = 120
		events = append(events, e)
		if i < 4 {
			events = append(events, ladder.Event{ID: fmt.Sprintf("m%d", i), At: e.At.Add(time.Minute), Season: 1, Kind: ladder.EventKindManage, Sprint: i + 1})
		}
	}
	in := sprintScorecardInput(events, 5, 1)
	if in.CostPerPoint != 50 || in.ExpectedCostPerPoint != 2 || in.WallPerPoint != 60 || in.ExpectedWallPerPoint != 240 { // 2 pts: 8 min cap (owner caps 2026-09-30)
		t.Fatalf("recorded inputs: %+v", in)
	}
	// An unfinished sprint and corrected deliveries must not enter the baseline.
	events = append(events, ladder.Event{ID: "correction", Season: 1, Kind: ladder.EventKindCorrection, Supersedes: "2"})
	in = sprintScorecardInput(events, 5, 1)
	if in.ExpectedCostPerPoint != 1.5 {
		t.Fatalf("corrected baseline = %v", in.ExpectedCostPerPoint)
	}
}

func TestSprintScorecardEvidenceAndPenalty(t *testing.T) {
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	s := &weaveStory{ID: 1, Owner: "manager", Thread: []weaveComment{{At: at, Kind: "merge", Body: `{"story":"merged"}`}}}
	events := []ladder.Event{{ID: "reg", At: at, Season: 1, Kind: ladder.EventKindRegression, Story: "merged"}, {ID: "other", At: at, Season: 1, Kind: ladder.EventKindRegression, Story: "unmerged"}}
	in, notes := sprintScorecardEvidence(events, s, 1)
	if in.EscapedRegressions != 1 || in.FalseRejections != 0 {
		t.Fatalf("review inputs: %+v", in)
	}
	if !strings.Contains(strings.Join(notes, ";"), "panels") || !strings.Contains(strings.Join(notes, ";"), "prior sprint") {
		t.Fatalf("missing evidence notes: %v", notes)
	}
	in.StallDetectMinutes = []float64{20, 40}
	card, err := ladder.ComputeScorecard(in, ladder.DefaultScorecardWeights())
	if err != nil {
		t.Fatal(err)
	}
	before := card.Score
	sprintScorecardApplyEvidence(&card, in, notes)
	if math.Abs(card.Components["efficiency"]-0.4) > 1e-9 || math.Abs(card.Score-(before-0.015)) > 1e-9 {
		t.Fatalf("stall penalty: %+v", card)
	}
	if !strings.Contains(strings.Join(card.Notes, ";"), "30.0") {
		t.Fatal(card.Notes)
	}
	in.AutoFails = []ladder.AutoFail{ladder.AutoFailDetectedBypass}
	card, _ = ladder.ComputeScorecard(in, ladder.DefaultScorecardWeights())
	sprintScorecardApplyEvidence(&card, in, notes)
	if card.Score != 0 {
		t.Fatal("penalty changed auto-fail")
	}
}

func TestSprintScorecardLinkedRunStalls(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("HOME", home)
	at := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	repo := filepath.Join(home, "repo")
	link := sprintRun{Repo: "repo", ID: 2, Queue: "repo-test", Born: at.Add(-time.Hour)}
	run := &weaveItem{ID: 2, Register: "merged", State: "killed", Created: link.Born, StartedAt: link.Born, FinishedAt: at}
	dir := filepath.Join(weaveStateRoot(home), link.Queue)
	if err := saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{run}}); err != nil {
		t.Fatal(err)
	}
	s := &weaveStory{ID: 1, Owner: "manager", Runs: []sprintRun{link}, Thread: []weaveComment{
		{At: at.Add(30 * time.Minute), Author: "manager", Kind: "relaunch", Body: "relaunch repo#2"},
		{At: at.Add(5 * time.Minute), Author: "manager", Kind: "fail", Body: "repo#20 failed"},
		{At: at.Add(10 * time.Minute), Author: "worker", Kind: "fail", Body: "repo#2 failed"},
		{At: at.Add(20 * time.Minute), Author: "manager", Kind: "assign", Body: `{"run":2,"story":"merged"}`},
		{At: at.Add(time.Hour), Author: "manager", Kind: "merge", Body: `{"run":"repo#2","generation":"` + link.Queue + ":" + link.Born.UTC().Format(time.RFC3339Nano) + `"}`},
	}}
	events := []ladder.Event{{ID: "r", Kind: ladder.EventKindRegression, Season: 1, Story: "merged", At: at.Add(2 * time.Hour)}}
	in, _ := sprintScorecardEvidence(events, s, 1)
	if len(in.StallDetectMinutes) != 1 || in.StallDetectMinutes[0] != 20 || in.EscapedRegressions != 1 {
		t.Fatalf("linked evidence: %+v", in)
	}
	run.LogPath = filepath.Join(home, "log")
	if err := os.WriteFile(run.LogPath, []byte("progress"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(run.LogPath, at.Add(-10*time.Minute), at.Add(-10*time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := saveWeaveQueue(dir, &weaveQueue{Root: repo, Items: []*weaveItem{run}}); err != nil {
		t.Fatal(err)
	}
	in, _ = sprintScorecardEvidence(events, s, 1)
	if len(in.StallDetectMinutes) != 1 || in.StallDetectMinutes[0] != 30 {
		t.Fatalf("log progress: %+v", in)
	}
	s.Runs[0].Born = at
	in, notes := sprintScorecardEvidence(events, s, 1)
	if len(in.StallDetectMinutes) != 0 || in.EscapedRegressions != 0 || !strings.Contains(strings.Join(notes, ";"), "unavailable") {
		t.Fatalf("recycled run: %+v %v", in, notes)
	}
}

func TestSprintScorecardMeterEvidenceFilters(t *testing.T) {
	a := ladder.Event{ID: "a", Season: 1, Kind: ladder.EventKindDelivery, Sprint: 1, Agent: "agent-a", Story: "a", Points: 2, Outcome: 1, Cost: 4}
	a.CapsUsed.WallSeconds = 120
	b := a
	b.ID = "b"
	b.Story = "b"
	b.Points = 5
	b.Cost = 10
	b.CapsUsed.WallSeconds = 300
	ignored := a
	ignored.ID = "ignored"
	ignored.Outcome = 0
	ignored.Cost = 10000
	events := []ladder.Event{a, b, ignored}
	in := sprintScorecardInput(events, 1, 1)
	if in.CostPerPoint != 2 || in.WallPerPoint != 60 || in.ExpectedWallPerPoint != 1680.0/7 { // (480s + 1200s) / 7 points: caps 8m/20m (owner caps 2026-09-30)
		t.Fatalf("weighted meters: %+v", in)
	}
	b.Cost = 0
	b.CapsUsed.WallSeconds = 0
	in = sprintScorecardInput([]ladder.Event{a, b}, 1, 1)
	if in.CostPerPoint != 0 || in.WallPerPoint != 0 {
		t.Fatalf("partial meters rewarded: %+v", in)
	}
	in = sprintScorecardInput(append(events, ladder.Event{Kind: ladder.EventKindCorrection, Season: 1, Supersedes: "b"}), 1, 1)
	if in.CostPerPoint != 2 || in.ExpectedWallPerPoint != 240 { // 2 pts: 8 min cap (owner caps 2026-09-30)
		t.Fatalf("corrected meters: %+v", in)
	}
}

func TestSprintEndScorecardPersistsEvidenceNotes(t *testing.T) {
	home := scorecardEndFixture(t)
	at := time.Now().UTC()
	e := ladder.Event{ID: "d", At: at, Season: 1, Kind: ladder.EventKindDelivery, Sprint: 1, Agent: "agent-a", Story: "merged", Points: 2, Outcome: 1, Cost: 4}
	e.CapsUsed.WallSeconds = 3600
	scorecardAppend(t, e, ladder.Event{ID: "r", At: at, Season: 1, Kind: ladder.EventKindRegression, Agent: "agent-a", Story: "merged"})
	dir := filepath.Join(home, ".bashy", "sprint")
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	weaveStoryAppend(findWeaveStory(q, 1), "Ada", "merge", `{"story":"merged"}`)
	if err := saveWeaveQueue(dir, q); err != nil {
		t.Fatal(err)
	}
	out, code := runSprint(t, "end", "1")
	if code != 0 {
		t.Fatalf("end: %d %s", code, out)
	}
	got := scorecardManageEvents(t)
	if len(got) != 1 || !strings.Contains(got[0].Note, "efficiency=0.32") || !strings.Contains(got[0].Note, "review=0.00") || !strings.Contains(got[0].Note, "panels") || !strings.Contains(out, "prior sprint") {
		t.Fatalf("persisted evidence: %+v\n%s", got, out)
	}
}
