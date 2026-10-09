package weave

import (
	"bytes"
	"path/filepath"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
	"github.com/spf13/cobra"
)

func TestSeatPoolAndDutyBoardUseFamilyEvidenceKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_SPRINT_DIR", filepath.Join(home, "sprint"))
	cat := pinFleetWith(t)
	for _, err := range []error{
		cat.SaveTool(fleet.Tool{Name: "tool-a"}),
		cat.SaveModel(fleet.Model{Name: "model-a", Band: 3}),
		cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "model-a", Band: 3}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	family, ok, err := cat.FamilyOf("agent-a")
	if err != nil || !ok {
		t.Fatalf("family: %+v %t %v", family, ok, err)
	}
	now := time.Now().UTC()
	season := ladder.SeasonOf(now)
	events := []ladder.Event{{ID: "family-result", Season: season, Kind: ladder.EventKindDelivery, Agent: "tool-a:model-a", FamilyID: family.ID(), InstanceUUID: "uuid-a", SelectedBinding: "tool-a:model-a", SeedBand: 3, Duty: ladder.DutyCode, Points: 3, Outcome: 1, At: now}}
	pool, _, err := seatPool("", events, now)
	if err != nil {
		t.Fatal(err)
	}
	var entrant *ladder.Entrant
	for i := range pool {
		if pool[i].Agent == "agent-a" {
			entrant = &pool[i]
			break
		}
	}
	if entrant == nil {
		t.Fatal("agent-a missing from seat pool")
	}
	board := capability.ComputeDutyBoard(events, season, nil, []ladder.Duty{ladder.DutyCode})
	rows := board.Duties[string(ladder.DutyCode)]
	if len(rows) != 1 || rows[0].Agent != family.ID() || entrant.Band != rows[0].Band || entrant.Standings[ladder.DutyCode].Events != rows[0].Events {
		t.Fatalf("seat pool and duty board disagree for %s: entrant=%+v rows=%+v", family.ID(), entrant, rows)
	}
}

func TestSeatRecordAndBand(t *testing.T) {
	base := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	events := make([]ladder.Event, 0, 7)
	for i := 0; i < 5; i++ {
		events = append(events, ladder.Event{Kind: ladder.EventKindDelivery, Agent: "agent-a", Outcome: 1, At: base.Add(time.Duration(i) * time.Minute)})
	}
	events = append(events, ladder.Event{Kind: ladder.EventKindDelivery, Agent: "agent-a", Outcome: 0, Blame: blame.Attribution{Class: blame.ClassEnvironment}, At: base.Add(6 * time.Minute)})
	got := seatBand(1, events, "agent-a")
	if got.Band != 2 || got.Streak != 0 {
		t.Fatalf("band state = %+v", got)
	}
	successes, failures := seatRecord(events, "agent-a")
	if successes != 5 || failures != 0 {
		t.Fatalf("record = %d/%d", successes, failures)
	}
}

func TestSeatPanelShrinksToOdd(t *testing.T) {
	pool := []ladder.Entrant{{Agent: "agent-a", Band: 4, Free: true, Vendor: "tool-a"}, {Agent: "agent-b", Band: 3, Free: true, Vendor: "tool-b"}}
	got := seatPanel(pool, 5, "")
	if len(got) != 1 || got[0].Agent != "agent-a" {
		t.Fatalf("panel = %+v", got)
	}
}

func TestSeatAssignL5FallsBackToL1(t *testing.T) {
	pool := []ladder.Entrant{{Agent: "agent-a", Band: 1, Free: true}, {Agent: "agent-b", Band: 1, Free: false}}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	var fallback sprintAssignEvent
	launches := 0
	deps := sprintAssignDeps{
		seed:   func() (int64, error) { return 1, nil },
		launch: func(_ *cobra.Command, r sprintAssignLaunch) error { launches++; return nil },
		record: func(kind string, e sprintAssignEvent) error {
			if kind == "fallback" {
				fallback = e
			}
			return nil
		},
	}
	if err := sprintAssignDispatch(cmd, ladder.StoryTask{ID: "story-a", Duty: ladder.DutyCode, Band: 5}, pool, ladder.Lines{}, "", false, "repo", deps); err != nil {
		t.Fatal(err)
	}
	if launches != 1 || fallback.Seat != "worker" || fallback.WantedBand != 5 || fallback.Band != 1 || fallback.ChosenAgent != "agent-a" {
		t.Fatalf("launches=%d fallback=%+v", launches, fallback)
	}
}

func TestSeatReviewL5FallsBackToL4(t *testing.T) {
	pool := []ladder.Entrant{{Agent: "agent-a", Band: 5, Free: false}, {Agent: "agent-b", Band: 4, Free: true}}
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	var fallback sprintAssignEvent
	if err := sprintReviewDispatch(cmd, "story-a", ladder.DutyStanding{}, 5, "", pool, false, func(kind string, e sprintAssignEvent) error {
		if kind == "fallback" {
			fallback = e
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if fallback.Seat != "review" || fallback.WantedBand != 5 || fallback.Band != 4 || fallback.ChosenAgent != "agent-b" {
		t.Fatalf("fallback=%+v output=%s", fallback, out.String())
	}
}
