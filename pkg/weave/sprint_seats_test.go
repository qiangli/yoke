package weave

import (
	"bytes"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/qiangli/yoke/pkg/ladder/blame"
	"github.com/spf13/cobra"
)

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
