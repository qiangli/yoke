package weave

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

// bandGateAdmission reports a manager's current band. BASHY_SPRINT_ENFORCE
// controls lease-token checks; a lower manager band records a fallback even
// in must mode.
func bandGateAdmission(p ladder.Profile, _ ladder.Lines, _ int, peg int) (bool, string) {
	band := peg
	if band < 1 {
		band = 1
	}
	if band >= 4 {
		return true, fmt.Sprintf("current L%d", band)
	}
	return false, fmt.Sprintf("current L%d", band)
}

func sprintManagerEligibility(owner string) (bool, string, error) {
	cat := fleetCatalog()
	a, ok := cat.Agent(owner)
	if !ok {
		return false, "agent not in fleet", nil
	}
	for a.ClonedFrom != "" {
		parent, found := cat.Agent(a.ClonedFrom)
		if !found {
			break
		}
		a = parent
	}
	events, err := sprintAssignReadEvents()
	if err != nil {
		return false, "", err
	}
	pool, _, err := seatPool("", events, time.Now())
	if err != nil {
		return false, "", err
	}
	for _, e := range pool {
		if e.Agent == a.Name {
			return e.Band >= 4, fmt.Sprintf("current L%d", e.Band), nil
		}
	}
	seed := a.Band
	if m, found := cat.Model(a.Model); found {
		seed = m.Band
	}
	key := a.Tool + ":" + a.Model
	state := seatBand(seed, events, key)
	return state.Band >= 4, fmt.Sprintf("current L%d", state.Band), nil
}

func checkSprintManagerBand(cmd *cobra.Command, id int64, owner string) error {
	ok, why, err := sprintManagerEligibility(owner)
	if err != nil {
		return fmt.Errorf("manager gate: %w", err)
	}
	if why == "agent not in fleet" || why == "agent not in available fleet" {
		return fmt.Errorf("manager gate: %s", why)
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return err
	}
	err = withWeaveQueueLock(dir, func(q *weaveQueue) error {
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		if !ok {
			band := 1
			fmt.Sscanf(why, "current L%d", &band)
			seatRecordFallback(s, seatFallbackEvent{Seat: "manager", WantedBand: 4, ChosenAgent: owner, Band: band})
		} else {
			weaveStoryAppend(s, owner, "band-gate", why)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !ok {
		fmt.Fprintf(cmd.ErrOrStderr(), "manager fallback: %s is L%s (no L4 free)\n", owner, strings.TrimPrefix(why, "current L"))
		return nil
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "manager gate: %s eligible (%s)\n", owner, why)
	return nil
}
