package weave

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

func TestBandGateAdmissionRules(t *testing.T) {
	lines := ladder.Lines{L3Code: 1000, L4Code: 1200, L4Manage: 1400}
	certs := []ladder.Certificate{}
	for _, kind := range []ladder.CertKind{ladder.CertL1, ladder.CertL2, ladder.CertL3, ladder.CertSteer, ladder.CertManager, ladder.CertReview} {
		certs = append(certs, ladder.Certificate{Kind: kind, ModelVersion: "v1", Season: 1})
	}
	base := ladder.Profile{ModelVersion: "v1", Certs: certs, Standings: map[ladder.Duty]ladder.DutyStanding{ladder.DutyCode: {R: 1600, RD: 50, Events: 8}, ladder.DutyManage: {R: 1550, RD: 50, Events: 8}}}
	cases := []struct {
		name     string
		profile  ladder.Profile
		peg      int
		eligible bool
		rule     string
	}{
		{"derived", base, 1, true, "derived"},
		{"provisional", ladder.Profile{Provisional: 4}, 1, true, "provisional"},
		{"play-up", ladder.Profile{ModelVersion: "v1", Certs: certs[:4], Standings: map[ladder.Duty]ladder.DutyStanding{ladder.DutyCode: {R: 1400, RD: 50, Events: 8}, ladder.DutyManage: {R: 1450, RD: 50, Events: 8}}}, 1, true, "play-up"},
		{"seed", ladder.Profile{}, 4, true, "seed"},
		{"established below", ladder.Profile{Standings: map[ladder.Duty]ladder.DutyStanding{ladder.DutyManage: {R: 1000, RD: 50, Events: 8}}}, 4, false, ""},
		{"missing gates", ladder.Profile{}, 1, false, "certificate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, why := bandGateAdmission(tc.profile, lines, 1, tc.peg)
			if ok != tc.eligible || !strings.Contains(why, tc.rule) {
				t.Fatalf("got %v %q", ok, why)
			}
		})
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
	if err != nil || !ok || !strings.Contains(why, "seed") {
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
	if err != nil || !ok || !strings.Contains(why, "provisional") {
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
	if ok, why, err := sprintManagerEligibility("agent-b"); err != nil || ok || !strings.Contains(why, "certificate") {
		t.Fatalf("season 1: %v %q %v", ok, why, err)
	}
	t.Setenv("BASHY_LADDER_SEASON", "2")
	if ok, why, err := sprintManagerEligibility("agent-b"); err != nil || !ok || !strings.Contains(why, "provisional") {
		t.Fatalf("season 2: %v %q %v", ok, why, err)
	}
}

func TestBandGateShouldMustOverride(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	cat := pinFleetWith(t)
	if err := cat.SaveModel(fleet.Model{Name: "gate-low", Band: 2}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-b", Tool: "tool-b", Model: "gate-low"}); err != nil {
		t.Fatal(err)
	}
	out, code := runSprint(t, "add", "gate fixture")
	if code != 0 {
		t.Fatalf("add: %d %s", code, out)
	}
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.Flags().Bool("override", false, "")
	cmd.Flags().String("reason", "", "")
	t.Setenv("BASHY_SPRINT_ENFORCE", "should")
	if err := checkSprintManagerBand(cmd, 1, "agent-b"); err != nil || !strings.Contains(stderr.String(), "WARN:") {
		t.Fatalf("should: %v %q", err, stderr.String())
	}
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	if err := checkSprintManagerBand(cmd, 1, "agent-b"); err == nil || !strings.Contains(err.Error(), "--override --reason") {
		t.Fatalf("must: %v", err)
	}
	if err := cmd.Flags().Set("override", "true"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Flags().Set("reason", "operator decision"); err != nil {
		t.Fatal(err)
	}
	if err := checkSprintManagerBand(cmd, 1, "agent-b"); err != nil {
		t.Fatalf("override: %v", err)
	}
	snapshot, err := sprintOwnerSnapshot(1)
	if err != nil {
		t.Fatal(err)
	}
	var gates, overrides int
	for _, item := range snapshot.Thread {
		if item.Kind == "band-gate" {
			gates++
		}
		if item.Kind == "override" {
			overrides++
		}
	}
	if gates != 2 || overrides != 1 {
		t.Fatalf("thread gates=%d overrides=%d", gates, overrides)
	}
}

func TestSprintStartAndTakeUseBandGateNotLeaseGuard(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	t.Setenv("BASHY_SPRINT_DIR", t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_SPRINT_LEASE_TOKEN", "wrong")
	t.Setenv("BASHY_SPRINT_ENFORCE", "must")
	cat := pinFleetWith(t)
	if err := cat.SaveModel(fleet.Model{Name: "gate-low", Band: 2}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "agent-b", Tool: "tool-b", Model: "gate-low"}); err != nil {
		t.Fatal(err)
	}
	if out, code := runSprint(t, "add", "gate fixture"); code != 0 {
		t.Fatalf("add: %d %s", code, out)
	}
	for _, args := range [][]string{
		{"start", "1", "--owner", "agent-b", "--for", "1h"},
		{"take", "1", "--owner", "agent-b"},
	} {
		out, code := runSprint(t, args...)
		if code == 0 || !strings.Contains(out, "manager gate:") {
			t.Fatalf("%s: code=%d output=%q", args[0], code, out)
		}
		if strings.Contains(out, "lease token") || strings.Contains(out, "detected bypass") {
			t.Fatalf("%s incorrectly lease-gated: %q", args[0], out)
		}
	}
}
