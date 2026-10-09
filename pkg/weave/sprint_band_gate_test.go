package weave

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

func TestBandGateAdmissionRules(t *testing.T) {
	for _, tc := range []struct {
		seed     int
		eligible bool
		reason   string
	}{
		{1, false, "current L1"}, {3, false, "current L3"}, {4, true, "current L4"}, {5, true, "current L5"},
	} {
		ok, why := bandGateAdmission(ladder.Profile{Provisional: 5}, ladder.Lines{}, 1, tc.seed)
		if ok != tc.eligible || why != tc.reason {
			t.Fatalf("seed %d: %v %q", tc.seed, ok, why)
		}
	}
}

func TestBandGateCloneAndSeed(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_LADDER_SEASON", "1")
	cat := pinFleetWith(t)
	if err := cat.SaveModel(fleet.Model{Name: "gate-model", Band: 4}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-a", Tool: "tool-a", Model: "gate-model"}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-a-w25", ClonedFrom: "agent-a", Tool: "tool-a", Model: "gate-model"}); err != nil {
		t.Fatal(err)
	}
	ok, why, err := sprintManagerEligibility("agent-a-w25")
	if err != nil || !ok || !strings.Contains(why, "current L4") {
		t.Fatalf("clone: %v %q %v", ok, why, err)
	}
	st, err := ladder.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(ladder.Event{Kind: ladder.EventKindSeat, Agent: "tool-a:gate-model", Season: 1, Provisional: 4}); err != nil {
		t.Fatal(err)
	}
	ok, why, err = sprintManagerEligibility("agent-a-w25")
	if err != nil || !ok || !strings.Contains(why, "current L4") {
		t.Fatalf("clone evidence: %v %q %v", ok, why, err)
	}
}

func TestBandGateEligibilityUsesCurrentSeason(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_LADDER_SEASON", "1")
	cat := pinFleetWith(t)
	if err := cat.SaveModel(fleet.Model{Name: "gate-low", Band: 2}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-b", Tool: "tool-b", Model: "gate-low"}); err != nil {
		t.Fatal(err)
	}
	st, err := ladder.OpenStore("")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Append(ladder.Event{Kind: ladder.EventKindSeat, Agent: "tool-b:gate-low", Season: 2, Provisional: 4}); err != nil {
		t.Fatal(err)
	}
	if ok, why, err := sprintManagerEligibility("agent-b"); err != nil || ok || !strings.Contains(why, "current L2") {
		t.Fatalf("season 1: %v %q %v", ok, why, err)
	}
	t.Setenv("BASHY_LADDER_SEASON", "2")
	if ok, why, err := sprintManagerEligibility("agent-b"); err != nil || ok || !strings.Contains(why, "current L2") {
		t.Fatalf("season 2: %v %q %v", ok, why, err)
	}
}

func TestBandGateShouldMustOverride(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	cat := pinFleetWith(t)
	if err := cat.SaveModel(fleet.Model{Name: "gate-low", Band: 3}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-b", Tool: "tool-b", Model: "gate-low"}); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "add", "gate fixture"); code != 0 {
		t.Fatalf("add: %d %s", code, out)
	}
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	if err := checkSprintManagerBand(cmd, 1, "agent-b"); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr.String(), "manager fallback: agent-b is L3 (no L4 free)") {
		t.Fatal(stderr.String())
	}
	snapshot, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range snapshot.Thread {
		if item.Kind == "fallback" && strings.Contains(item.Body, `"seat":"manager"`) {
			found = true
		}
	}
	if !found {
		t.Fatal("manager fallback absent from thread")
	}
}

func TestSprintStartAndTakeUseBandGateNotLeaseGuard(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	cat := pinFleetWith(t)
	if err := cat.SaveModel(fleet.Model{Name: "gate-low", Band: 3}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-b", Tool: "tool-b", Model: "gate-low"}); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "add", "gate fixture"); code != 0 {
		t.Fatalf("add: %d %s", code, out)
	}
	out, code := runSprint(t, "start", "1", "--owner", "agent-b", "--for", "1h")
	if code != 0 || !strings.Contains(out, "manager fallback: agent-b is L3") {
		t.Fatalf("start: %d %s", code, out)
	}
}

func TestTakeStillChecksTheManagerBandAfterIdentityResolution(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	cat := pinFleetWith(t)
	if err := cat.SaveModel(fleet.Model{Name: "gate-low", Band: 2}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-b", Tool: "tool-b", Model: "gate-low"}); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "add", "take gate fixture"); code != 0 {
		t.Fatalf("add: %d %s", code, out)
	}
	out, code := runSprint(t, "take", "1", "--owner", "agent-b")
	if code != 0 || !strings.Contains(out, "manager fallback: agent-b is L2") {
		t.Fatalf("take: %d %s", code, out)
	}
}

// Legacy evidence is keyed by tool:model and no writer stamps a family yet, so
// the seat pool must keep reading it: switching every catalog agent to its
// family key made the scheduler and the band gate see seed-only agents.
func TestSeatPoolReadsLegacyEvidenceForAFamilyWithoutFamilyEvents(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_LADDER_SEASON", "1")
	cat := pinFleetWith(t)
	pool, _, err := seatPool("", nil, time.Now())
	if err != nil || len(pool) == 0 {
		t.Fatalf("empty seat pool: %v", err)
	}
	agent := pool[0].Agent
	if _, ok, err := cat.FamilyOf(agent); err != nil || !ok {
		t.Fatalf("%s has no family: %v", agent, err)
	}
	binding, _, _, err := cat.Binding(agent)
	if err != nil {
		t.Fatal(err)
	}
	events := []ladder.Event{{ID: "legacy-1", Kind: ladder.EventKindDelivery, Agent: binding.MatrixKey(), Duty: ladder.DutyCode, Points: 3, Outcome: 1, Season: 1, At: time.Now()}}
	pool, _, err = seatPool("", events, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pool {
		if e.Agent == agent {
			if e.Successes != 1 || e.CodingStoriesThisSeason != 1 {
				t.Fatalf("legacy evidence ignored: successes=%d stories=%d", e.Successes, e.CodingStoriesThisSeason)
			}
			return
		}
	}
	t.Fatalf("%s left the seat pool", agent)
}
