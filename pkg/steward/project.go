// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"
)

// Every type in this file is a READ-ONLY PROJECTION of the journal.
//
// None of them is persisted as a source of truth, none of them can be edited, and
// each is a pure function of the entries it was derived from. That is what makes them
// safe: a view cannot drift from the journal, because a view has no state of its own
// to drift with. The moment one of these becomes writable, the system has two truths
// and the older one starts lying.
//
// The board below is a practical Kanban — lanes, priorities, owners, blockers, next
// actions, links out to issues and weave runs — and it is STILL only a projection.
// Every field folds the latest value recorded by a workstream.update entry. "Set this
// to p0" is a fact that someone set it, at a time, under an epoch, by name; it is not
// a cell somebody overwrote.

// Lane is the Kanban column. Derived from the recorded lane, with two overrides the
// projection insists on: a closed strand is done, and a strand with live blockers is
// blocked no matter what lane anybody typed. A board that let you park a blocked item
// in "in-progress" would be a board that hides the only thing worth looking at.
type Lane string

const (
	LaneBacklog    Lane = "backlog"
	LaneReady      Lane = "ready"
	LaneInProgress Lane = "in-progress"
	LaneBlocked    Lane = "blocked"
	LaneReview     Lane = "review"
	LaneDone       Lane = "done"
)

// laneOrder is the left-to-right order of the board.
var laneOrder = []Lane{LaneBacklog, LaneReady, LaneInProgress, LaneBlocked, LaneReview, LaneDone}

func (l Lane) Valid() bool { return slices.Contains(laneOrder, l) }

func laneNames() []string {
	out := make([]string, 0, len(laneOrder))
	for _, l := range laneOrder {
		out = append(out, string(l))
	}
	return out
}

func laneRank(l Lane) int {
	if i := slices.Index(laneOrder, l); i >= 0 {
		return i
	}
	return len(laneOrder)
}

// Priority is the urgency of a strand. p0 is "the host is on fire".
type Priority string

const (
	P0 Priority = "p0"
	P1 Priority = "p1"
	P2 Priority = "p2"
	P3 Priority = "p3"
)

var priorityOrder = []Priority{P0, P1, P2, P3}

func (p Priority) Valid() bool { return slices.Contains(priorityOrder, p) }

// priorityRank sorts p0 first and UNSET last. Unset sorts last rather than in the
// middle: an unprioritized item is one nobody has triaged, and it should not jump the
// queue ahead of something a human explicitly called p2.
func priorityRank(p Priority) int {
	if i := slices.Index(priorityOrder, p); i >= 0 {
		return i
	}
	return len(priorityOrder)
}

// WorkstreamState is a strand's lifecycle position — NOT its outcome. A closed
// workstream whose evidence never arrived is closed AND unknown, and collapsing those
// two axes into one is how a board starts reporting wishes as facts.
type WorkstreamState string

const (
	WorkstreamOpen   WorkstreamState = "open"
	WorkstreamClosed WorkstreamState = "closed"
)

// Confidence grades how well a strand's reported outcome is actually BACKED. The
// three-way split between verified, asserted, and unknown is the point of the whole
// package, so read them together:
type Confidence string

const (
	// ConfidenceVerified — somebody went and CHECKED, and their attestation is in the
	// journal (a KindVerification entry naming this outcome's exact hash). This is the
	// ONLY way to reach verified. Attaching a reference to your own claim does not
	// promote it, no matter how checkable the reference looks.
	ConfidenceVerified Confidence = "verified"

	// ConfidenceAsserted — an outcome was claimed AND references were supplied, but
	// nobody has checked them. This is where an honest, diligent, entirely unverified
	// agent report lands, and it is deliberately NOT "verified".
	//
	// The distinction is the one that matters most in practice. "command:go test ./..."
	// records that an agent SAYS it ran the tests. It does not record that the tests
	// ran, that they passed, or that they exist. A model producing a confident summary
	// with a plausible command string attached is the single most common way a fabricated
	// success enters a system that means well — the reference is exactly as easy to
	// generate as the prose. So a reference buys you AUDITABILITY (a human knows where
	// to look) and nothing else. Only a verification buys you truth.
	ConfidenceAsserted Confidence = "asserted"

	// ConfidenceUnknown — nothing established the outcome one way or the other (a
	// success claimed with no evidence at all, or an outcome recorded as unknown).
	ConfidenceUnknown Confidence = "unknown"

	// ConfidenceDegraded — an outcome was self-declared degraded.
	ConfidenceDegraded Confidence = "degraded"

	// ConfidenceRefuted — somebody checked and found the claim FALSE. A verification
	// can move a strand backwards, and only backwards: refuting a success is believed,
	// "verifying" a failure into a success is not something this projection will do
	// from a single attestation.
	ConfidenceRefuted Confidence = "refuted"
)

// Workstream is one strand of work, as the journal describes it: a Kanban card whose
// every field was recorded, by somebody, under an epoch.
type Workstream struct {
	Name  string          `json:"name"`
	Title string          `json:"title,omitempty"`
	State WorkstreamState `json:"state"`
	Lane  Lane            `json:"lane"`

	Priority   Priority  `json:"priority,omitempty"`
	Owner      string    `json:"owner,omitempty"`
	Agents     []string  `json:"agents,omitempty"`
	Blockers   []string  `json:"blockers,omitempty"`
	NextAction string    `json:"next_action,omitempty"`
	NextAt     time.Time `json:"next_at,omitzero"`
	Links      []Link    `json:"links,omitempty"`

	Outcome    Outcome    `json:"outcome,omitempty"`
	Confidence Confidence `json:"confidence"`

	Entries       int `json:"entries"`
	EvidenceCount int `json:"evidence_count"`
	Decisions     int `json:"decisions"`
	Verifications int `json:"verifications"`

	OpenedAt    time.Time `json:"opened_at,omitzero"`
	UpdatedAt   time.Time `json:"updated_at,omitzero"`
	LastSummary string    `json:"last_summary,omitempty"`

	// Unproven lists, in the strand's own words, what could not be established. Prose,
	// because the whole value of an unknown is knowing WHICH claim is unproven — a bare
	// count is a number nobody can act on.
	Unproven []string `json:"unproven,omitempty"`

	// outcomeSeq/outcomeHash track the entry whose outcome is currently folded, so a
	// verification can attach to exactly it. Unexported: not part of the board digest.
	outcomeSeq  uint64
	outcomeHash string
}

// Board is the derived state of every workstream, plus the journal coordinates it was
// derived from. Watermark + Digest make a board CITABLE: two agents comparing boards
// can tell instantly whether they are looking at the same history.
type Board struct {
	SchemaVersion string       `json:"schema_version"`
	Workstreams   []Workstream `json:"workstreams"`
	Watermark     uint64       `json:"watermark"`
	Digest        string       `json:"digest"`

	// Degraded is true when ANY strand's outcome could not be established. Surfaced at
	// the top level so a status check cannot miss it by only reading the happy rows.
	Degraded bool `json:"degraded"`
	// Asserted counts strands whose outcome rests on references NOBODY HAS CHECKED.
	// It is not an error — it is the normal state of honest work — but it is never
	// allowed to look like "verified", and the headline says so out loud.
	Asserted int `json:"asserted"`
	// Blocked counts strands with live blockers.
	Blocked int `json:"blocked"`
}

// ProjectBoard derives the board from a sequence of entries, grading each strand's
// confidence with sc.
//
// SC IS THE TRUSTED VERIFIER, AND A NIL SC PROMOTES NOTHING. That is not a degenerate
// case, it is the DEFAULT: a host that injected no VerificationVerifier has no way to
// check whether any claim came true, so its board reports every claimed success as
// asserted. Passing the checker in — rather than reading it off a Store — is what keeps
// this a pure function: the same entries and the same checker always give the same board,
// and a projection can still be taken of a journal nobody holds a handle to.
//
// Promotion is NOT decided by anything in the entries themselves. A Seal read out of the
// journal is bytes on the agent's filesystem; only sc can say whether it is one the
// verifier actually issued, for this exact claim. See sealPromotes.
//
// TRANSCRIPTS ARE SKIPPED ENTIRELY. Nothing about the board may depend on a
// non-authoritative artifact, which is why deleting every transcript on the host
// leaves this function's output bit-identical.
func ProjectBoard(entries []Entry, sc SealChecker) Board {
	byName := map[string]*Workstream{}
	var order []string
	bySeq := entriesBySeq(entries)

	// Pass 1: fold the strands.
	for _, e := range entries {
		if !e.Kind.Authoritative() {
			continue // transcripts derive nothing, by contract
		}
		name := e.Workstream
		if name == "" {
			continue // seat / reconcile / repair / checkpoint events are host-level, not rows
		}
		ws, ok := byName[name]
		if !ok {
			ws = &Workstream{Name: name, State: WorkstreamOpen, Lane: LaneBacklog, Confidence: ConfidenceUnknown}
			byName[name] = ws
			order = append(order, name)
		}

		at := parseTime(e.Time)
		if ws.OpenedAt.IsZero() {
			ws.OpenedAt = at
		}
		ws.UpdatedAt = at
		ws.Entries++
		ws.EvidenceCount += len(e.Evidence)
		if e.Summary != "" && e.Kind != KindWorkstreamUpdate {
			ws.LastSummary = e.Summary
		}

		switch e.Kind {
		case KindWorkstreamOpen:
			if e.Summary != "" {
				ws.Title = e.Summary
			}
			if ws.Lane == LaneBacklog {
				ws.Lane = LaneReady
			}
		case KindWorkstreamUpdate:
			if e.Update != nil {
				applyUpdate(ws, *e.Update)
			}
		case KindDecision:
			ws.Decisions++
		case KindWorkstreamClose:
			ws.State = WorkstreamClosed
		case KindVerification:
			ws.Verifications++
			continue // a verification does not itself become the strand's outcome
		}

		// Fold the outcome. The EFFECTIVE outcome is used, never the claimed one: this
		// is the line that stops an unevidenced "success" from becoming a green row.
		if eff := e.EffectiveOutcome(); eff != "" {
			ws.Outcome = eff
			ws.outcomeSeq, ws.outcomeHash = e.Seq, e.Hash
			switch eff {
			case OutcomeUnknown, OutcomeDegraded:
				ws.Confidence = confidenceFor(eff)
				ws.Unproven = append(ws.Unproven, unprovenReason(e))
			default:
				// Evidence present, but a reference is a pointer, not a check. The most it
				// can earn on its own is "asserted" — see ConfidenceAsserted.
				ws.Confidence = ConfidenceAsserted
			}
		}
	}

	// Pass 2: apply verifications. Separate pass because an attestation may arrive
	// before or after other entries, and it must bind to the outcome the board actually
	// ended up folding — by HASH, so it cannot vouch for bytes it never saw.
	for _, e := range entries {
		if e.Kind != KindVerification || e.Verifies == nil {
			continue
		}
		ws, ok := byName[e.Workstream]
		if !ok || ws.outcomeHash == "" || e.Verifies.TargetHash != ws.outcomeHash {
			continue // it attested to something other than this strand's current outcome
		}
		switch e.Verifies.Result {
		case OutcomeSuccess:
			// PROMOTION IS EARNED, NOT ASSERTED, AND NOT BY ANYTHING IN THIS FILE.
			//
			// A verification promotes only when the injected verifier RE-CHECKS its seal and
			// says it is one it issued for this exact claim. Nothing else does: not the prose
			// --method, not a digest the caller attached to evidence it chose, not an
			// approved-looking attestation struct it filled in itself. Every one of those is
			// bytes the agent wrote, and the board is a projection of a FILE the agent can
			// write — so a check that only reads the file checks the forger's own work.
			//
			// This is the second of two locks on the same door (Attest holds the first), and
			// it is the one that matters, because it grades records it did not write —
			// including a verification that appeared in the journal without ever going
			// through Attest at all, which is precisely the case worth defending against.
			if !sealPromotes(e, bySeq[e.Verifies.TargetSeq], sc) {
				ws.Unproven = append(ws.Unproven, "seq "+itoa(ws.outcomeSeq)+
					": a success verification at seq "+itoa(e.Seq)+" was recorded, but no trusted verifier vouched for it — "+
					"it promotes nothing")
				continue
			}
			ws.Confidence = ConfidenceVerified
		case OutcomeFailed:
			// Degradation travels one way, and this is the direction it travels in.
			ws.Confidence = ConfidenceRefuted
			ws.Outcome = OutcomeFailed
			ws.Unproven = append(ws.Unproven, "seq "+itoa(ws.outcomeSeq)+": REFUTED by verification at seq "+itoa(e.Seq))
		case OutcomeUnknown:
			ws.Confidence = ConfidenceUnknown
			ws.Unproven = append(ws.Unproven, "seq "+itoa(ws.outcomeSeq)+": checked at seq "+itoa(e.Seq)+", and the check could not establish it either")
		}
	}

	b := Board{SchemaVersion: SchemaVersion}
	sort.Strings(order)
	for _, name := range order {
		ws := byName[name]
		// Lane overrides the projection insists on, in this order.
		switch {
		case ws.State == WorkstreamClosed:
			ws.Lane = LaneDone
		case len(ws.Blockers) > 0:
			ws.Lane = LaneBlocked
		}
		if ws.Outcome == OutcomeUnknown || ws.Outcome == OutcomeDegraded {
			b.Degraded = true
		}
		if ws.Confidence == ConfidenceAsserted {
			b.Asserted++
		}
		if ws.Lane == LaneBlocked {
			b.Blocked++
		}
		b.Workstreams = append(b.Workstreams, *ws)
	}
	// Sort the rows for display: lane, then priority, then name. Deterministic, so the
	// digest is too.
	sort.SliceStable(b.Workstreams, func(i, j int) bool {
		a, c := b.Workstreams[i], b.Workstreams[j]
		if la, lc := laneRank(a.Lane), laneRank(c.Lane); la != lc {
			return la < lc
		}
		if pa, pc := priorityRank(a.Priority), priorityRank(c.Priority); pa != pc {
			return pa < pc
		}
		return a.Name < c.Name
	})

	if n := len(entries); n > 0 {
		b.Watermark = entries[n-1].Seq
	}
	b.Digest = boardDigest(b.Workstreams)
	return b
}

// applyUpdate folds one Kanban update into a strand. Last writer wins per field —
// and Clear is how a field goes back to empty, because unblocking is as much of an
// event as blocking, and a field that can only ever be set is a field that rots.
func applyUpdate(ws *Workstream, u WorkstreamUpdate) {
	for _, f := range u.Clear {
		switch strings.ToLower(strings.TrimSpace(f)) {
		case "lane":
			ws.Lane = LaneBacklog
		case "priority":
			ws.Priority = ""
		case "owner":
			ws.Owner = ""
		case "agents":
			ws.Agents = nil
		case "blockers":
			ws.Blockers = nil
		case "next_action", "next-action", "next":
			ws.NextAction = ""
		case "next_at", "next-at":
			ws.NextAt = time.Time{}
		case "links":
			ws.Links = nil
		}
	}
	if u.Lane != "" {
		ws.Lane = u.Lane
	}
	if u.Priority != "" {
		ws.Priority = u.Priority
	}
	if u.Owner != "" {
		ws.Owner = u.Owner
	}
	if len(u.Agents) > 0 {
		ws.Agents = mergeUnique(ws.Agents, u.Agents)
	}
	if len(u.Blockers) > 0 {
		ws.Blockers = mergeUnique(ws.Blockers, u.Blockers)
	}
	if u.NextAction != "" {
		ws.NextAction = u.NextAction
	}
	if u.NextAt != "" {
		ws.NextAt = parseTime(u.NextAt)
	}
	for _, l := range u.Links {
		if !slices.Contains(ws.Links, l) {
			ws.Links = append(ws.Links, l)
		}
	}
}

func mergeUnique(have, add []string) []string {
	for _, a := range add {
		a = strings.TrimSpace(a)
		if a != "" && !slices.Contains(have, a) {
			have = append(have, a)
		}
	}
	return have
}

func confidenceFor(o Outcome) Confidence {
	if o == OutcomeDegraded {
		return ConfidenceDegraded
	}
	return ConfidenceUnknown
}

// unprovenReason explains, in one line, why a claim did not land — distinguishing
// "it said it could not tell" from "it claimed success and showed nothing".
func unprovenReason(e Entry) string {
	if e.Outcome == OutcomeSuccess && !e.HasEvidence() {
		return "seq " + itoa(e.Seq) + ": claimed success with no evidence — recorded as unknown"
	}
	s := "seq " + itoa(e.Seq) + ": " + string(e.Outcome)
	if e.Summary != "" {
		s += " — " + e.Summary
	}
	return s
}

// boardDigest is a content hash over the board's rows, so a checkpoint's
// reproducibility can be checked by comparing one string.
//
// Deterministic by construction: the rows are sorted and marshaled canonically, so
// the same entries always produce the same digest — on any host, in any process, in
// any order the entries happened to be written.
func boardDigest(ws []Workstream) string {
	b, _ := json.Marshal(ws)
	return digestOf(b)
}

func itoa(u uint64) string {
	if u == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for u > 0 {
		i--
		buf[i] = byte('0' + u%10)
		u /= 10
	}
	return string(buf[i:])
}

// Board returns the current board, derived live from the journal.
func (s *Store) Board() (Board, *Replay, error) {
	rep, err := s.Replay()
	if err != nil {
		return Board{}, nil, err
	}
	return ProjectBoard(rep.Entries, s.sealChecker()), rep, nil
}

// Filter selects entries from the log.
type Filter struct {
	Kinds      []Kind
	Workstream string
	Since      time.Time
	Until      time.Time
	Actor      string
	// DegradedOnly keeps only entries whose claim could not be established — the
	// "what do I not actually know?" query, which is the one a successor needs first.
	DegradedOnly bool
	Limit        int
}

// Match reports whether e passes the filter.
func (f Filter) Match(e Entry) bool {
	if len(f.Kinds) > 0 && !slices.Contains(f.Kinds, e.Kind) {
		return false
	}
	if f.Workstream != "" && e.Workstream != f.Workstream {
		return false
	}
	if f.Actor != "" && !strings.EqualFold(e.Actor.Name, f.Actor) {
		return false
	}
	if f.DegradedOnly && !e.Degraded() {
		return false
	}
	at := parseTime(e.Time)
	if !f.Since.IsZero() && at.Before(f.Since.UTC()) {
		return false
	}
	if !f.Until.IsZero() && at.After(f.Until.UTC()) {
		return false
	}
	return true
}

// Log returns the chronological entries matching a filter. Chronological because the
// journal is append-only: entry order IS time order, and a log that reordered them
// would be inventing a history the chain does not attest to.
//
// Limit keeps the LAST n matches (the recent tail is what a caller wants), and the
// result stays in chronological order.
func (s *Store) Log(f Filter) ([]Entry, *Replay, error) {
	rep, err := s.Replay()
	if err != nil {
		return nil, nil, err
	}
	var out []Entry
	for _, e := range rep.Entries {
		if f.Match(e) {
			out = append(out, e)
		}
	}
	if f.Limit > 0 && len(out) > f.Limit {
		out = out[len(out)-f.Limit:]
	}
	return out, rep, nil
}

// Conversation returns the decision and transcript entries: the record of what was
// decided and, where a transcript survives, how the room got there.
func (s *Store) Conversation(f Filter) ([]Entry, *Replay, error) {
	if len(f.Kinds) == 0 {
		f.Kinds = []Kind{KindDecision, KindTranscript}
	}
	return s.Log(f)
}

// StateChange is one transition in the seat's history: who held it, at what epoch,
// how they got it, and how they lost it.
type StateChange struct {
	Seq       uint64    `json:"seq"`
	At        time.Time `json:"at"`
	Kind      Kind      `json:"kind"`
	Actor     string    `json:"actor"`
	Epoch     uint64    `json:"epoch"`
	Summary   string    `json:"summary"`
	Rationale string    `json:"rationale,omitempty"`
	// Authz is the capability a takeover was performed under — grant, provenance, the
	// operator it named. Kept whole rather than flattened to a name, because
	// "authorized by qiangli" and "an operator ASSERTION naming qiangli, unattended,
	// with no receipt" are different facts and the second one is the true one.
	Authz *AuthzRef `json:"authz,omitempty"`
}

// History returns the seat's authority history and the checkpoints taken along the
// way — the "how did we get here" view, reconstructed entirely by replay.
func (s *Store) History() ([]StateChange, []CheckpointRef, *Replay, error) {
	rep, err := s.Replay()
	if err != nil {
		return nil, nil, nil, err
	}
	var changes []StateChange
	var cks []CheckpointRef
	for _, e := range rep.Entries {
		switch e.Kind {
		case KindSeatClaimed, KindSeatTakeover, KindSeatReleased:
			changes = append(changes, StateChange{
				Seq: e.Seq, At: parseTime(e.Time), Kind: e.Kind,
				Actor: holderName(e.Actor), Epoch: e.Epoch,
				Summary: e.Summary, Rationale: e.Rationale, Authz: e.Authz,
			})
		case KindCheckpoint:
			cks = append(cks, CheckpointRef{
				Seq: e.Seq, At: parseTime(e.Time), ID: e.Ref, Summary: e.Summary,
			})
		}
	}
	return changes, cks, rep, nil
}

// CheckpointRef is a checkpoint as the journal remembers it. The journal's memory is
// what counts: a checkpoint FILE can be deleted and re-derived, but the fact that a
// checkpoint was taken at a watermark is history.
type CheckpointRef struct {
	Seq     uint64    `json:"seq"`
	At      time.Time `json:"at"`
	ID      string    `json:"id"`
	Summary string    `json:"summary,omitempty"`
}
