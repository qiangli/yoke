package capability

import (
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ladder"
)

// DelegateToolName is the canonical tool prefix for delegated scoring identities.
const DelegateToolName = "genie-delegate"

// DelegateIdentity formats a delegated run's separate scoring identity:
// "genie-delegate:<tool>-<model>" with exactly one colon, ensuring outcomes
// are never merged into genie's own row (ycode:<model>) nor the vendor's row.
func DelegateIdentity(tool, model string) string {
	tool = strings.TrimSpace(tool)
	model = strings.TrimSpace(model)
	if strings.HasPrefix(tool, "genie-delegate:") {
		return tool
	}
	if model == "" && strings.Contains(tool, ":") {
		tool, model, _ = strings.Cut(tool, ":")
	}
	if model == "" && strings.Contains(tool, "-") {
		return fmt.Sprintf("%s:%s", DelegateToolName, tool)
	}
	return fmt.Sprintf("%s:%s-%s", DelegateToolName, tool, model)
}

// IsDelegateIdentity reports whether an identity is a genie-delegate identity.
func IsDelegateIdentity(identity string) bool {
	return strings.HasPrefix(identity, "genie-delegate:")
}

// ParseDelegateIdentity parses a "genie-delegate:<tool>-<model>" identity
// into tool and model.
func ParseDelegateIdentity(identity string) (tool, model string, ok bool) {
	if !IsDelegateIdentity(identity) {
		return "", "", false
	}
	rest := strings.TrimPrefix(identity, "genie-delegate:")
	tool, model, ok = strings.Cut(rest, "-")
	return tool, model, ok
}

// RecordDelegateRun writes exactly one ladder event for a delegated run under
// the genie-delegate:<tool>-<model> identity, ensuring no cross-crediting into
// genie's own ycode row or the vendor's tool:model row.
func RecordDelegateRun(tool, model string, ev ladder.Event, storePath ...string) error {
	agent := DelegateIdentity(tool, model)
	if err := ladderRecordCanonicalAgent(agent); err != nil {
		return err
	}
	ev.Agent = agent
	// Delegated work is isolated: never fold into ycode or vendor family.
	if ev.FamilyID != "" && !IsDelegateIdentity(ev.FamilyID) {
		ev.FamilyID = ""
	}
	if ev.Kind == "" {
		ev.Kind = ladder.EventKindDelivery
	}
	if ev.Duty == "" {
		ev.Duty = ladder.DutyCode
	}
	if ev.At.IsZero() {
		ev.At = time.Now().UTC()
	}
	if ev.Season < 1 {
		ev.Season = ladder.SeasonOf(ev.At)
	}
	if ev.ID == "" {
		ev.ID = ladderRecordNewID(ev.At)
	}
	if ev.Reviewer == "" {
		ev.Reviewer = ladderRecordReviewer()
	}
	if ev.Kind == ladder.EventKindDelivery && !ladder.ValidPoints(ev.Points) {
		ev.Points = ladder.Points(1)
	}

	path := ladder.DefaultStorePath()
	if len(storePath) > 0 && storePath[0] != "" {
		path = storePath[0]
	}
	st, err := ladder.OpenStore(path)
	if err != nil {
		return fmt.Errorf("record delegate: opening store: %w", err)
	}
	if err := st.Append(ev); err != nil {
		return fmt.Errorf("record delegate: appending event: %w", err)
	}
	return nil
}

// RecordDelegateEvent writes exactly one ladder event for a delegated run.
// If ev.Agent is not already a genie-delegate identity, tool and model are
// extracted from ev.Agent.
func RecordDelegateEvent(ev ladder.Event, storePath ...string) error {
	if IsDelegateIdentity(ev.Agent) {
		tool, model, _ := ParseDelegateIdentity(ev.Agent)
		return RecordDelegateRun(tool, model, ev, storePath...)
	}
	tool, model := ToolOf(ev.Agent), ModelOf(ev.Agent)
	return RecordDelegateRun(tool, model, ev, storePath...)
}

// RecordDelegateLedger writes one run ledger record for a delegated run under
// the genie-delegate:<tool>-<model> identity.
func RecordDelegateLedger(tool, model string, r RunRecord) error {
	r.Agent = DelegateIdentity(tool, model)
	return Append(r)
}
