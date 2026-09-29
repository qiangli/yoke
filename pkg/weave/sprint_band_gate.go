package weave

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/ladder"
	"github.com/spf13/cobra"
)

// bandGateAdmission applies the manager rules to a resolved binding's evidence.
func bandGateAdmission(p ladder.Profile, lines ladder.Lines, season, peg int) (bool, string) {
	// DeriveBand includes provisional seats, so name that rule first.
	if p.Provisional >= 4 {
		return true, "provisional seat"
	}
	band, misses := ladder.DeriveBand(p, lines, season)
	if band >= 4 {
		return true, "derived L4+"
	}
	manage := p.Standings[ladder.DutyManage]
	playStanding := manage
	if manage.Events == 0 {
		playStanding = p.Standings[ladder.DutyCode]
	}
	if band == 3 && lines.L4Manage > 0 && playStanding.RD > 0 && playStanding.Lower()+playStanding.RD >= lines.L4Manage {
		return true, "play-up candidate"
	}
	if !manage.Established() && peg >= 4 {
		return true, "seed fleet peg"
	}
	var reasons []string
	for _, miss := range misses {
		reasons = append(reasons, miss.Reason)
	}
	if band < 3 {
		reasons = append(reasons, "G4 requires manager and review certificates, conservative code and manage standings")
	}
	if lines.L4Manage <= 0 {
		reasons = append(reasons, "L4 manage line not yet fitted")
	} else if manage.Lower() < lines.L4Manage {
		reasons = append(reasons, fmt.Sprintf("conservative manage %.0f below L4 line %.0f", manage.Lower(), lines.L4Manage))
	}
	return false, strings.Join(reasons, "; ")
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
	peg := a.Band
	if _, _, model, bindErr := cat.Binding(a.Name); bindErr == nil && peg == 0 {
		peg = model.Band
	} else if model, found := cat.Model(a.Model); found && peg == 0 {
		peg = model.Band
	}
	path := ladder.DefaultStorePath()
	var events []ladder.Event
	if _, err := os.Stat(path); err == nil {
		store, err := ladder.OpenStore(path)
		if err != nil {
			return false, "", err
		}
		events, err = store.Read()
		if err != nil {
			return false, "", err
		}
	} else if !os.IsNotExist(err) {
		return false, "", err
	}
	season := ladder.SeasonOf(time.Now())
	rep := ladder.Replay(events, season)
	profile := ladder.Profile{}
	if rec := rep.Agents[a.MatrixKey()]; rec != nil {
		profile.Standings, profile.Certs, profile.Provisional = rec.Standings, rec.Certs, rec.Provisional
		if n := len(rec.Certs); n > 0 {
			profile.ModelVersion = rec.Certs[n-1].ModelVersion
		}
	}
	ok, why := bandGateAdmission(profile, capability.LadderLines(rep, season), season, peg)
	return ok, why, nil
}

// checkSprintManagerBand runs before a lease is acquired and records the verdict
// even when must-mode refuses the operation.
func checkSprintManagerBand(cmd *cobra.Command, id int64, owner string) error {
	ok, why, err := sprintManagerEligibility(owner)
	if err != nil {
		return fmt.Errorf("manager gate: %w", err)
	}
	override, _ := cmd.Flags().GetBool("override")
	reason, _ := cmd.Flags().GetString("reason")
	if override && strings.TrimSpace(reason) == "" {
		return fmt.Errorf("manager gate: --override requires --reason")
	}
	if !ok && override {
		why = why + "; override: " + strings.TrimSpace(reason)
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
		kind := "band-gate"
		if !ok && override {
			kind = "override"
		}
		weaveStoryAppend(s, owner, kind, why)
		return nil
	})
	if err != nil {
		return err
	}
	if ok {
		fmt.Fprintf(cmd.ErrOrStderr(), "manager gate: %s eligible (%s)\n", owner, why)
		return nil
	}
	if override {
		fmt.Fprintf(cmd.ErrOrStderr(), "manager gate: %s override (%s)\n", owner, why)
		return nil
	}
	message := fmt.Sprintf("manager gate: %s ineligible (%s); use --override --reason <reason>", owner, why)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("BASHY_SPRINT_ENFORCE")), "must") {
		return fmt.Errorf("%s", message)
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "WARN: "+message)
	return nil
}
