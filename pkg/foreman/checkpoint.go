package foreman

import (
	"fmt"
	"strings"
)

// CheckpointSchemaVersion is the wire contract of Session.Checkpoint.
const CheckpointSchemaVersion = "bashy-foreman-checkpoint-v1"

// The continuation budget.
//
// These are BYTE ceilings at the composition boundary, not token estimates:
// bytes are deterministic across models and are what the artifact stores. The
// values are the initial ratchet from the coordination context-efficiency plan
// (512 B inline preview, 8 KiB status/list snapshot); telemetry may move them.
// What must not move is the shape: every constant here is a constant, so the
// rendered continuation has a ceiling that does not depend on how long the
// session has been running.
const (
	// ContinuationBudget caps the checkpoint + recent window that a prompt
	// carries in place of the whole history. It is enforced after rendering,
	// not merely implied by the per-item limits.
	ContinuationBudget = 8 * 1024
	// RecentWindow is how many of the latest turns are quoted (as previews).
	RecentWindow = 6
	// EntryPreviewBytes bounds one quoted recent turn.
	EntryPreviewBytes = 512
	// LastResultBytes bounds the last agent result, which gets more room than a
	// recent turn because it is what the next instruction usually reacts to.
	LastResultBytes = 1024
	// MaxDecisions is how many of the operator's latest instructions are listed
	// as accepted decisions; DecisionPreviewBytes bounds each.
	MaxDecisions         = 5
	DecisionPreviewBytes = 256
	// DependencyPreviewBytes bounds a DAG dependency's quoted output; the full
	// output is referenced by history seq.
	DependencyPreviewBytes = 256
)

// HistoryRef is the stable reference to the complete history artifact as of
// the moment a checkpoint was taken.
type HistoryRef struct {
	Path    string `json:"path"`
	Entries int64  `json:"entries"`
	Bytes   int64  `json:"bytes"`
	Digest  string `json:"digest,omitempty"`
}

// Checkpoint is the bounded continuation of a session: what a fresh agent (or
// a fresh turn on the non-steerable path) needs to carry on, and no more.
//
// It is a PROJECTION. Every quoted text is a preview that names its history
// seq; the verbatim turn lives in the artifact at History.Path. Nothing here is
// written back as truth, and nothing here is a substitute for the artifact
// when a turn must be verified.
type Checkpoint struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Goal          string `json:"goal"`
	Status        string `json:"status"`
	CurrentStep   string `json:"current_step,omitempty"`
	Seq           int64  `json:"seq,omitempty"`
	Digest        string `json:"digest,omitempty"`

	// Decisions are the operator's latest accepted instructions (tell and
	// mid-turn steers), oldest first; DecisionsTotal is how many there were.
	Decisions      []HistoryEntry `json:"decisions,omitempty"`
	DecisionsTotal int            `json:"decisions_total"`
	Blockers       []string       `json:"blockers,omitempty"`
	LastResult     *HistoryEntry  `json:"last_result,omitempty"`
	Recent         []HistoryEntry `json:"recent,omitempty"`
	History        HistoryRef     `json:"history"`
}

// Checkpoint takes the bounded continuation of the session as it is now.
func (s *Session) Checkpoint() Checkpoint {
	return buildCheckpoint(s.State(), s.store, s.hist.snapshot())
}

func buildCheckpoint(st State, store Store, snap continuitySnapshot) Checkpoint {
	cp := Checkpoint{
		SchemaVersion:  CheckpointSchemaVersion,
		ID:             st.ID,
		Goal:           st.Goal,
		Status:         st.Status,
		CurrentStep:    st.CurrentStep,
		Seq:            st.Seq,
		Digest:         st.Digest,
		Decisions:      snap.decisions,
		DecisionsTotal: snap.decisionsTotal,
		Recent:         snap.recent,
		History: HistoryRef{
			Path:    store.HistoryPath(),
			Entries: snap.entries,
			Bytes:   snap.bytes,
			Digest:  snap.digest,
		},
	}
	if snap.lastResult.Seq > 0 {
		r := snap.lastResult
		cp.LastResult = &r
	}
	if b := strings.TrimSpace(st.Blocker); b != "" {
		cp.Blockers = append(cp.Blockers, b)
	}
	if st.Paused && st.Blocker != "paused by operator" {
		cp.Blockers = append(cp.Blockers, "paused by operator")
	}
	if st.Stopped {
		cp.Blockers = append(cp.Blockers, "stopped: "+firstNonEmpty(st.StopReason, "no reason recorded"))
	}
	return cp
}

// render is the prompt section that replaced the whole-history replay. It is
// empty for a session with nothing to continue from, and never longer than
// ContinuationBudget otherwise: the recent window shrinks first, then the last
// result goes, and as a last resort the text itself is bounded — in that
// order, because the references and the blockers are the part that must
// survive.
func (cp Checkpoint) render() string {
	if cp.History.Entries == 0 && len(cp.Blockers) == 0 {
		return ""
	}
	recent := cp.Recent
	withResult := true
	for {
		out := cp.renderWith(recent, withResult)
		if len(out) <= ContinuationBudget {
			return out
		}
		switch {
		case len(recent) > 0:
			recent = recent[1:]
		case withResult:
			withResult = false
		default:
			return preview(out, ContinuationBudget)
		}
	}
}

func (cp Checkpoint) renderWith(recent []HistoryEntry, withResult bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Session checkpoint (%s — a projection; the record is the history artifact below):\n", CheckpointSchemaVersion)
	fmt.Fprintf(&b, "Status: %s", firstNonEmpty(cp.Status, StatusIdle))
	if strings.TrimSpace(cp.CurrentStep) != "" {
		fmt.Fprintf(&b, "  Step: %s", oneLine(preview(cp.CurrentStep, DecisionPreviewBytes)))
	}
	b.WriteByte('\n')
	if len(cp.Decisions) > 0 {
		fmt.Fprintf(&b, "Accepted decisions (last %d of %d, oldest first):\n", len(cp.Decisions), cp.DecisionsTotal)
		for _, d := range cp.Decisions {
			fmt.Fprintf(&b, "- [seq %d] %s\n", d.Seq, oneLine(d.Text))
		}
	}
	if len(cp.Blockers) > 0 {
		b.WriteString("Blockers:\n")
		for _, bl := range cp.Blockers {
			fmt.Fprintf(&b, "- %s\n", oneLine(bl))
		}
	}
	if withResult && cp.LastResult != nil {
		fmt.Fprintf(&b, "Last agent result [seq %d, %d bytes]:\n%s\n", cp.LastResult.Seq, cp.LastResult.Bytes, cp.LastResult.Text)
	}
	if len(recent) > 0 {
		fmt.Fprintf(&b, "Recent turns (last %d of %d):\n", len(recent), cp.History.Entries)
		for _, e := range recent {
			fmt.Fprintf(&b, "[seq %d %s] %s\n", e.Seq, e.Role, e.Text)
		}
	}
	fmt.Fprintf(&b, "History artifact: %s (%d entries, %d bytes", cp.History.Path, cp.History.Entries, cp.History.Bytes)
	if cp.History.Digest != "" {
		fmt.Fprintf(&b, ", %s", cp.History.Digest)
	}
	b.WriteString(") — verbatim turns by seq; it is never replayed here.\n")
	return b.String()
}

func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
