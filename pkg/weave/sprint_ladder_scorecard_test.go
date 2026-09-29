package weave

import (
	"math"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
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
	season := sprintScorecardSeason(time.Now())
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

func TestSprintScorecardSeasonIsCalendarWeeks(t *testing.T) {
	for _, c := range []struct {
		at   time.Time
		want int
	}{
		{time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC), 1},
		{time.Date(2026, 9, 28, 0, 0, 0, 0, time.UTC), 1},
		{time.Date(2026, 10, 4, 23, 59, 0, 0, time.UTC), 1},
		{time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), 2},
		{time.Date(2026, 10, 19, 12, 0, 0, 0, time.UTC), 4},
	} {
		if got := sprintScorecardSeason(c.at); got != c.want {
			t.Errorf("season(%s) = %d, want %d", c.at, got, c.want)
		}
	}
}
