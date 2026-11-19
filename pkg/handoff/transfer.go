// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package handoff

// S5 recovery transfer: what a replacement needs, composed from what exists.
//
// A handoff Record already carries continuity prose, the next action, blockers
// and the in-flight working tree across tools and hosts. What it did NOT carry
// — the S0 gap this file closes — is the recovery contract a replacement must
// be able to check BEFORE it retries:
//
//   - candidate identity: the EXACT source (repo, branch, base SHA, carried
//     patch digest) the work and the evidence belong to, so agent death or a
//     host interruption cannot silently swap what is being resumed;
//   - changed files: the file list derived from that tree, so the replacement
//     can see scope without re-deriving a diff;
//   - verification evidence: gate/test/probe outcomes carried BY VALUE with a
//     timestamp and the tree they ran against, so stale evidence (taken against
//     a different tree) is recognizable as stale;
//   - pending decisions: blockers plus approvals and cancellations carried BY
//     REFERENCE (an approval id/session/task pointer), which stay visible until
//     decided — a task pointer is bookkeeping, never proof an effect happened;
//   - next safe steps: the ordered retry plan, with steps that touch the
//     outside world gated on effect reconciliation;
//   - uncertain external effects: every outside effect whose outcome is not
//     known, with an explicit reconcile-before-retry gate. A task id alone is
//     never accepted as proof of exactly-once execution.
//
// What this file deliberately does NOT do:
//
//   - no new coordination store. Single ownership is the existing coord ledger:
//     the handoff take is serialized by ClaimKind ("handoff:<id>"), the sprint
//     seat by the sprint lease, the campaign by the baton lock. Transfer names
//     those claims so a replacement can find them; it never duplicates them.
//   - no portable vendor session state. Vendor transcripts stay where they are;
//     the record plus this transfer is the whole portable artifact.
//   - no S4 duplication. Worker readiness (exact binding, credential, capacity
//     with freshness) is S4's finding; Transfer carries it BY REFERENCE
//     (ReadinessRef) and renders a warning when it is absent or stale.
//
// Durability: identity, holder and status are always re-derived from the live
// Record (the stamp Claim writes). Annotations only — evidence, effects,
// decisions, next steps, readiness — persist as a sidecar
// (<handoff-id>.transfer.json) next to the record, written atomically like the
// record itself, so agent death or a host interruption loses nothing. The
// sidecar travels WITH the record (same directory, same scp/commit).

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// TransferSchemaVersion is the wire contract for the sidecar. Additive changes
// only, for the same reason as the record schema: the transfer must outlive
// the process — and sometimes the host — that wrote it.
const TransferSchemaVersion = "bashy-transfer-v1"

// EffectStatus is the reconciliation state of one external effect.
type EffectStatus string

const (
	// EffectApplied means the effect was observed in the outside world after
	// the interruption (re-checked, not assumed).
	EffectApplied EffectStatus = "applied"
	// EffectAbsent means the outside world was checked and the effect is NOT
	// there — a retry may attempt it.
	EffectAbsent EffectStatus = "absent"
	// EffectUncertain means nobody knows. Retry of the owning step is refused
	// until this is reconciled to applied or absent.
	EffectUncertain EffectStatus = "uncertain"
)

// DecisionStatus is the lifecycle of one pending decision.
type DecisionStatus string

const (
	DecisionPending   DecisionStatus = "pending"
	DecisionDecided   DecisionStatus = "decided"
	DecisionCancelled DecisionStatus = "cancelled"
)

// Candidate is the exact source identity under recovery. Every field is a
// fact about the artifact, not about who ran it: a replacement on another
// host re-resolves paths and re-derives the digest rather than trusting them.
type Candidate struct {
	Repo       string `json:"repo"`
	Branch     string `json:"branch,omitempty"`
	BaseSHA    string `json:"base_sha,omitempty"`
	HeadSHA    string `json:"head_sha,omitempty"`
	WorkSHA    string `json:"work_sha,omitempty"`
	HandoffID  string `json:"handoff_id,omitempty"`
	SprintText string `json:"sprint_story,omitempty"`
	WeaveIssue int64  `json:"weave_issue,omitempty"`
	Workspace  string `json:"workspace,omitempty"`
}

// Complete reports whether the candidate names a verifiable source. Without a
// repo and a base SHA there is nothing to bind evidence to.
func (c Candidate) Complete() bool {
	return c.Repo != "" && c.BaseSHA != ""
}

// Identity is the stable one-line identity of the candidate: what the work and
// the evidence belong to. Two transfers with different identities are different
// candidates, however similar their task pointers look.
func (c Candidate) Identity() string {
	head := shortSHA(c.HeadSHA)
	if head == "" {
		head = "no-tip"
	}
	work := c.WorkSHA
	if len(work) > 12 {
		work = work[:12]
	}
	if work == "" {
		work = "no-work"
	}
	return fmt.Sprintf("%s@%s..%s/%s", c.Repo, shortSHA(c.BaseSHA), head, work)
}

// ChangedFile names one file in the candidate tree and how it got there.
type ChangedFile struct {
	Path string `json:"path"`
	// Kind is one of: modified, added, deleted, untracked, not-carried (named
	// in the record but too large to travel — fetch from the origin host).
	Kind string `json:"kind"`
}

// Evidence is one verification outcome carried BY VALUE: the command, its
// exit, an excerpt of its output, WHEN it ran, WHO ran it, and the work it
// ran against. A replacement trusts it only when TreeSHA matches the
// candidate work digest — evidence taken against another tree is history,
// not proof.
type Evidence struct {
	Kind    string    `json:"kind"`
	Command string    `json:"command,omitempty"`
	Exit    *int      `json:"exit,omitempty"`
	Output  string    `json:"output,omitempty"`
	TreeSHA string    `json:"tree_sha,omitempty"`
	At      time.Time `json:"at"`
	By      string    `json:"by,omitempty"`
}

// Binds reports whether this evidence was taken against the given candidate
// work digest. Unbound evidence (no TreeSHA) is honestly old-style: kept
// visible, never trusted as current.
func (e Evidence) Binds(workSHA string) bool {
	return e.TreeSHA != "" && workSHA != "" && e.TreeSHA == workSHA
}

// Fresh reports whether the evidence is younger than maxAge.
func (e Evidence) Fresh(maxAge time.Duration) bool {
	if e.At.IsZero() {
		return false
	}
	return time.Since(e.At) <= maxAge
}

// Decision is one pending decision the replacement must see: a blocker from
// the record, an approval waiting on a human (carried BY REFERENCE — an id,
// never vendor session state), a review verdict, or a cancellation notice.
// Decided and cancelled rows stay visible; only pending rows gate the retry.
type Decision struct {
	Kind     string         `json:"kind"`
	Summary  string         `json:"summary"`
	Ref      string         `json:"ref,omitempty"`
	Status   DecisionStatus `json:"status"`
	RaisedBy string         `json:"raised_by,omitempty"`
	At       time.Time      `json:"at,omitempty"`
}

// NextStep is one ordered retry step. Steps that touch the outside world set
// RequiresReconciliation and must not be attempted while the retry gate
// (VerifyRetrySafe) is closed.
type NextStep struct {
	Step                   string `json:"step"`
	RequiresReconciliation bool   `json:"requires_reconciliation,omitempty"`
	Done                   bool   `json:"done,omitempty"`
}

// Effect is one external effect whose outcome must be KNOWN before the owning
// step is retried. The record of a task — a task id, a "submitted" state, a
// launch receipt — is never the outcome: only a re-check of the outside world
// moves Uncertain to Applied or Absent.
type Effect struct {
	ID        string       `json:"id"`
	Op        string       `json:"op"`
	Detail    string       `json:"detail,omitempty"`
	Status    EffectStatus `json:"status"`
	CheckedAt time.Time    `json:"checked_at,omitempty"`
	CheckedBy string       `json:"checked_by,omitempty"`
}

// Ownership names the existing ledger claims that keep two replacements from
// holding one recovery. The transfer never grants ownership — the replacement
// still takes it through the ledger (`bashy resume --claim` serializes takes
// on the handoff id; the sprint story and seat lease stay with their own
// ledgers); these refs say WHERE to take it and who holds it.
type Ownership struct {
	HandoffClaim string `json:"handoff_claim,omitempty"`
	Holder       string `json:"holder,omitempty"`
	SeatRole     string `json:"seat_role,omitempty"`
}

// ReadinessRef carries S4's worker-readiness finding BY REFERENCE: the exact
// binding the replacement will run as, why it is considered ready, and how
// fresh that finding is. A transfer without one renders an explicit warning —
// readiness is checked before assignment, not assumed from the handoff.
type ReadinessRef struct {
	Binding   string    `json:"binding,omitempty"`
	Reason    string    `json:"reason,omitempty"`
	CheckedAt time.Time `json:"checked_at,omitempty"`
	Source    string    `json:"source,omitempty"`
}

// Stale reports whether the readiness finding is older than maxAge, or was
// never made. A missing finding is stale: nothing says the binding works.
func (r *ReadinessRef) Stale(maxAge time.Duration) bool {
	if r == nil || r.CheckedAt.IsZero() {
		return true
	}
	return time.Since(r.CheckedAt) > maxAge
}

// Transfer is the S5 recovery envelope for one handoff record.
type Transfer struct {
	SchemaVersion string        `json:"schema_version"`
	HandoffID     string        `json:"handoff_id"`
	CreatedAt     time.Time     `json:"created_at"`
	Candidate     Candidate     `json:"candidate"`
	Changed       []ChangedFile `json:"changed,omitempty"`
	Evidence      []Evidence    `json:"evidence,omitempty"`
	Decisions     []Decision    `json:"decisions,omitempty"`
	Next          []NextStep    `json:"next,omitempty"`
	Effects       []Effect      `json:"effects,omitempty"`
	// NoExternalEffects is an EXPLICIT declaration by the author that the
	// interrupted work touched nothing outside the candidate tree. It defaults
	// to false: an undeclared scope reads as unknown, and an unknown scope
	// keeps the retry gate closed. There is no "no effects were recorded, so
	// there were none" shortcut.
	NoExternalEffects bool          `json:"no_external_effects,omitempty"`
	Cancellations     []Decision    `json:"cancellations,omitempty"`
	Owner             Ownership     `json:"owner,omitempty"`
	Readiness         *ReadinessRef `json:"readiness,omitempty"`
}

// TransferDecl is the additive declaration a Record carries for its S5
// recovery metadata. It does NOT duplicate the annotations — the sidecar
// stays the single home of evidence, effects, decisions, next steps and
// readiness — it only states what the reader must find there, so a portable
// export that copied the record without its sidecar (or with a damaged one)
// is caught instead of resuming with dropped uncertain-effect state. The
// effect ids pin the declared scope: a sidecar that lost or gained an effect
// no longer matches its record. Statuses are deliberately NOT pinned here —
// reconciling an effect advances its status in the sidecar alone — so only
// applied and absent ever read as known (see UncertainEffects).
type TransferDecl struct {
	SchemaVersion string `json:"schema_version"`
	// Effects are the declared external-effect ids, in declaration order.
	Effects []string `json:"effects,omitempty"`
	// NoExternalEffects mirrors the author's explicit scope declaration.
	NoExternalEffects bool `json:"no_external_effects,omitempty"`
}

// Declared reports whether the record carries transfer metadata. Records
// written before the transfer envelope have none and read as legacy.
func (r *Record) Declared() bool { return r.Transfer != nil }

// Decl builds the declaration matching the given annotations: the exact
// scope a reader must find in the sidecar.
func (t *Transfer) Decl() *TransferDecl {
	d := &TransferDecl{SchemaVersion: TransferSchemaVersion, NoExternalEffects: t.NoExternalEffects}
	for _, e := range t.Effects {
		d.Effects = append(d.Effects, e.ID)
	}
	return d
}

// maxEvidenceOutput caps one carried output excerpt. The transfer travels like
// the record (scp, mesh, issue comment); it must not swallow a 2 GB log.
const maxEvidenceOutput = 2000

// WorkDigest binds work and evidence to the exact carried candidate: the
// base commit it applies to, the diff itself, and every untracked file by
// path, mode and content (including the too-large marker, which names
// content the record deliberately did not carry — a changed marker is a
// changed candidate). The repo path joins the digest as recorded; a
// replacement on another host re-resolves it as a hint and re-derives the
// digest rather than trusting it. An empty digest means there is no work to
// bind — callers treat it as "nothing in flight", never as an identity.
func WorkDigest(ws WorkingState) string {
	if ws.Diff == "" && len(ws.Untracked) == 0 {
		return ""
	}
	h := sha256.New()
	for _, s := range []string{ws.Repo, ws.BaseSHA, ws.Diff} {
		h.Write([]byte(s))
		h.Write([]byte{0})
	}
	files := append([]UntrackedFile(nil), ws.Untracked...)
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	for _, f := range files {
		fmt.Fprintf(h, "%s\x00%d\x00%s\x00", f.Path, f.Mode, f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ChangedFilesOf derives the file list from a captured working tree: the
// diff headers plus the untracked entries. The diff itself stays in the
// Record; this is the scope summary, not a second copy of the work.
func ChangedFilesOf(ws WorkingState) []ChangedFile {
	var out []ChangedFile
	seen := map[string]bool{}
	for _, f := range diffFiles(ws.Diff) {
		if !seen[f.Path] {
			seen[f.Path] = true
			out = append(out, f)
		}
	}
	for _, u := range ws.Untracked {
		if seen[u.Path] {
			continue
		}
		seen[u.Path] = true
		kind := "untracked"
		if strings.HasPrefix(u.Content, "<<< NOT CARRIED:") {
			kind = "not-carried"
		}
		out = append(out, ChangedFile{Path: u.Path, Kind: kind})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// diffFiles parses `diff --git` / `---` / `+++` headers into file entries.
// Anything unparseable is skipped: the transfer must not invent scope.
func diffFiles(diff string) []ChangedFile {
	var out []ChangedFile
	var minus, plus string
	flush := func() {
		if plus == "" && minus == "" {
			return
		}
		path, kind := plus, "modified"
		if plus == "" || plus == "/dev/null" {
			path, kind = minus, "deleted"
		} else if minus == "" || minus == "/dev/null" {
			kind = "added"
		}
		path = strings.TrimPrefix(path, "b/")
		path = strings.TrimPrefix(path, "a/")
		if path != "" && path != "/dev/null" {
			out = append(out, ChangedFile{Path: path, Kind: kind})
		}
		minus, plus = "", ""
	}
	for _, line := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(line, "diff --git "):
			flush()
		case strings.HasPrefix(line, "--- "):
			minus = strings.TrimSpace(strings.TrimPrefix(line, "--- "))
		case strings.HasPrefix(line, "+++ "):
			plus = strings.TrimSpace(strings.TrimPrefix(line, "+++ "))
			flush()
		}
	}
	flush()
	return out
}

// Summarize derives the transfer base from a live Record. Identity, holder and
// status always come from the record — the stamp Claim writes — so a transfer
// composed from a stale copy still describes the current owner, never a ghost.
func Summarize(rec *Record) *Transfer {
	t := &Transfer{
		SchemaVersion: TransferSchemaVersion,
		HandoffID:     rec.ID,
		CreatedAt:     time.Now().UTC(),
		Candidate: Candidate{
			Repo:      rec.Work.Repo,
			Branch:    rec.Work.Branch,
			BaseSHA:   rec.Work.BaseSHA,
			WorkSHA:   WorkDigest(rec.Work),
			HandoffID: rec.ID,
			Workspace: rec.Work.Repo,
		},
		Changed: ChangedFilesOf(rec.Work),
		Owner: Ownership{
			HandoffClaim: string(ClaimRef(rec.ID).Kind) + ":" + ClaimRef(rec.ID).Name,
			SeatRole:     rec.SeatRole(),
		},
	}
	if rec.Links.Sprint != 0 {
		t.Candidate.SprintText = fmt.Sprintf("%d", rec.Links.Sprint)
	}
	if rec.Links.WeaveIssue != 0 {
		t.Candidate.WeaveIssue = int64(rec.Links.WeaveIssue)
	}
	if rec.Links.Workspace != "" {
		t.Candidate.Workspace = rec.Links.Workspace
	}
	if rec.ResumedBy != nil {
		t.Owner.Holder = refName(*rec.ResumedBy)
	}
	for _, b := range rec.Blockers {
		t.Decisions = append(t.Decisions, Decision{
			Kind:    "blocker",
			Summary: b,
			Status:  DecisionPending,
		})
	}
	if rec.NextAction != "" {
		t.Next = append(t.Next, NextStep{Step: rec.NextAction})
	}
	if rec.CancelledAt != nil {
		by := ""
		if rec.CancelledBy != nil {
			by = refName(*rec.CancelledBy)
		}
		t.Cancellations = append(t.Cancellations, Decision{
			Kind:     "cancellation",
			Summary:  "handoff " + rec.ID + " was explicitly cancelled — it must not be picked up",
			Status:   DecisionCancelled,
			RaisedBy: by,
			At:       *rec.CancelledAt,
		})
	}
	return t
}

// Overlay merges sidecar annotations (evidence, effects, decisions, next
// steps, readiness, scope declaration) onto a record-derived base. Identity,
// holder and cancellations always come from the base: annotations describe
// the recovery, they never re-identify the candidate or move the stamp.
func Overlay(base *Transfer, side *Transfer) *Transfer {
	if side == nil {
		return base
	}
	if len(side.Evidence) > 0 {
		base.Evidence = append(base.Evidence, side.Evidence...)
	}
	if len(side.Decisions) > 0 {
		base.Decisions = append(base.Decisions, side.Decisions...)
	}
	if len(side.Next) > 0 {
		base.Next = append(base.Next, side.Next...)
	}
	if len(side.Effects) > 0 {
		base.Effects = append(base.Effects, side.Effects...)
	}
	if len(side.Cancellations) > 0 {
		base.Cancellations = append(base.Cancellations, side.Cancellations...)
	}
	if side.NoExternalEffects {
		base.NoExternalEffects = true
	}
	if side.Readiness != nil {
		base.Readiness = side.Readiness
	}
	return base
}

// AttachEvidence carries one verification outcome into the transfer, capped
// and redacted like every other member-facing egress in this package.
func (t *Transfer) AttachEvidence(e Evidence) {
	if len(e.Output) > maxEvidenceOutput {
		e.Output = e.Output[len(e.Output)-maxEvidenceOutput:]
	}
	e.Output = redactText(e.Output)
	e.Command = redactText(e.Command)
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	t.Evidence = append(t.Evidence, e)
}

// AttachDecision records one pending decision or approval BY REFERENCE.
// The ref names where the decision lives (an approval id, a session, a task);
// resolving it is the replacement's job, and its mere existence proves nothing
// about outside effects.
func (t *Transfer) AttachDecision(d Decision) {
	if d.Status == "" {
		d.Status = DecisionPending
	}
	d.Summary = redactText(d.Summary)
	t.Decisions = append(t.Decisions, d)
	if d.Status == DecisionCancelled {
		t.Cancellations = append(t.Cancellations, d)
	}
}

// DeclareEffect registers one outside effect whose outcome is NOT yet known.
// New effects start uncertain: the author names the op, the replacement
// reconciles it.
func (t *Transfer) DeclareEffect(op, detail string) Effect {
	e := Effect{
		ID:     fmt.Sprintf("fx-%d", len(t.Effects)+1),
		Op:     redactText(op),
		Detail: redactText(detail),
		Status: EffectUncertain,
	}
	t.Effects = append(t.Effects, e)
	return e
}

// ReconcileEffect records the OBSERVED outcome of one effect: a re-check of
// the outside world, by a named checker, now. Only applied and absent are
// outcomes — every other status (uncertain, empty, or anything unrecognized)
// is refused, so an unknown status can never be laundered into a safe one.
// Unknown ids are refused too: reconciling means naming the effect and its
// observed outcome, never waving at the task.
func (t *Transfer) ReconcileEffect(id string, st EffectStatus, by string) error {
	if st != EffectApplied && st != EffectAbsent {
		return fmt.Errorf("reconciling %s as %q records nothing: re-check the outside world and mark it applied or absent", id, st)
	}
	for i, e := range t.Effects {
		if e.ID == id {
			t.Effects[i].Status = st
			t.Effects[i].CheckedAt = time.Now().UTC()
			t.Effects[i].CheckedBy = by
			return nil
		}
	}
	return fmt.Errorf("unknown effect %s: declare it before reconciling it", id)
}

// VerifyRetrySafe is the gate in front of every retry step that touches the
// outside world. It opens only when every declared effect has an observed
// outcome AND the scope itself was declared: with no effects and no explicit
// no-external-effects statement, the scope is unknown and the gate stays
// closed. A task id, a run state, or a launch receipt never opens it — only
// observed outcomes do.
// UncertainEffects returns the effects whose outside outcome is still
// unknown — the exact list a retry must reconcile first. Only applied and
// absent count as known: an empty or otherwise unrecognized status stays
// unsafe, so a corrupt or forward-versioned sidecar keeps the gate closed
// instead of reading as reconciled.
func (t *Transfer) UncertainEffects() []Effect {
	var out []Effect
	for _, e := range t.Effects {
		if e.Status != EffectApplied && e.Status != EffectAbsent {
			out = append(out, e)
		}
	}
	return out
}

func (t *Transfer) VerifyRetrySafe() error {
	if uncertain := t.UncertainEffects(); len(uncertain) > 0 {
		names := make([]string, 0, len(uncertain))
		for _, e := range uncertain {
			names = append(names, e.ID+":"+e.Op)
		}
		return fmt.Errorf("retry refused: %d external effect(s) uncertain — reconcile before retry: %s",
			len(names), strings.Join(names, ", "))
	}
	if len(t.Effects) == 0 && !t.NoExternalEffects {
		return fmt.Errorf("retry refused: external-effect scope was never declared — " +
			"declare effects (then reconcile them) or explicitly record no-external-effects")
	}
	return nil
}

// SafeToAttempt reports whether one next step may be attempted now. Local
// steps (no outside touch) are always attemptable; reconciling steps wait for
// the gate. A decided or cancelled decision never blocks; a pending approval
// blocks only the step that names it — visibility, not a global lock.
func (t *Transfer) SafeToAttempt(s NextStep) bool {
	if s.Done {
		return false
	}
	if !s.RequiresReconciliation {
		return true
	}
	return t.VerifyRetrySafe() == nil
}

// transferSidecar is the sidecar filename for one handoff id.
func transferSidecar(dir, handoffID string) string {
	return filepath.Join(dir, handoffID+".transfer.json")
}

// SaveTransfer persists the annotation sidecar atomically (temp + rename), the
// same discipline as Save: a reader — possibly another agent, possibly
// mid-crash — never sees a half-written transfer.
func SaveTransfer(dir string, t *Transfer) (string, error) {
	if t == nil || t.HandoffID == "" {
		return "", fmt.Errorf("cannot save a transfer without a handoff id")
	}
	t.SchemaVersion = TransferSchemaVersion
	redactTransfer(t)
	b, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	path := transferSidecar(dir, t.HandoffID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", err
	}
	return path, nil
}

// LoadTransfer reads one annotation sidecar. A missing sidecar is not an
// error condition for the caller — LoadTransfer reports it, and the caller
// falls back to the record-derived summary (effects unknown, gate closed).
func LoadTransfer(dir, handoffID string) (*Transfer, error) {
	b, err := os.ReadFile(transferSidecar(dir, handoffID))
	if err != nil {
		return nil, err
	}
	var t Transfer
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("%s: not a transfer sidecar: %w", handoffID, err)
	}
	if t.SchemaVersion == "" {
		return nil, fmt.Errorf("%s: missing schema_version (not a transfer sidecar)", handoffID)
	}
	return &t, nil
}

// Compose builds the full transfer for one record: the live record-derived
// base overlaid with whatever annotations the sidecar carries. Identity,
// holder and status always come from the record; a stale sidecar can add
// context but can never re-identify the candidate or move the stamp.
//
// Compose is the legacy-tolerant entry: a record that declares transfer
// metadata but whose sidecar is missing or damaged still returns the base
// here (with the gate closed). Agent paths that must NOT silently drop
// declared uncertain-effect state use ComposeChecked, which refuses instead.
func Compose(dir string, rec *Record) *Transfer {
	base := Summarize(rec)
	side, err := LoadTransfer(dir, rec.ID)
	if err != nil {
		return base
	}
	return Overlay(base, side)
}

// ComposeChecked builds the full transfer with the declaration enforced. A
// record that declares transfer metadata MUST have its sidecar present,
// parseable, addressed to it, and matching the declared scope — otherwise the
// export is incomplete (the record travelled without its annotations) and the
// composition is refused rather than returned with silently dropped effects.
// A legacy record (no declaration) composes exactly like Compose: whatever
// annotations exist are overlaid, and none are required.
func ComposeChecked(dir string, rec *Record) (*Transfer, error) {
	base := Summarize(rec)
	side, err := LoadTransfer(dir, rec.ID)
	if rec.Transfer == nil {
		if err != nil {
			return base, nil
		}
		return Overlay(base, side), nil
	}
	if err != nil {
		return nil, fmt.Errorf("handoff %s declares transfer metadata but %s is missing or unreadable: re-export the record WITH its sidecar (same directory, same scp/commit) instead of resuming without it: %v",
			rec.ID, transferSidecar(dir, rec.ID), err)
	}
	if err := checkSidecar(rec, side); err != nil {
		return nil, err
	}
	return Overlay(base, side), nil
}

// checkSidecar verifies that loaded annotations are the ones the record
// declares: addressed to this handoff, with exactly the declared effect
// scope. Statuses are not compared — reconciling advances them in the
// sidecar alone — but every other mismatch means the pair is not a whole
// export and must not be resumed from.
func checkSidecar(rec *Record, side *Transfer) error {
	if side.HandoffID != "" && side.HandoffID != rec.ID {
		return fmt.Errorf("handoff %s declares transfer metadata but its sidecar belongs to %s: re-export the record WITH its sidecar instead of resuming from a mixed pair",
			rec.ID, side.HandoffID)
	}
	want := append([]string(nil), rec.Transfer.Effects...)
	got := make([]string, 0, len(side.Effects))
	for _, e := range side.Effects {
		got = append(got, e.ID)
	}
	if len(want) != len(got) {
		return fmt.Errorf("handoff %s declares %d external effect(s) but its sidecar carries %d: re-export the record WITH its sidecar instead of resuming without it",
			rec.ID, len(want), len(got))
	}
	seen := map[string]bool{}
	for _, id := range want {
		seen[id] = true
	}
	for _, id := range got {
		if !seen[id] {
			return fmt.Errorf("handoff %s declares transfer metadata but its sidecar carries undeclared effect %s: re-export the record WITH its sidecar instead of resuming from a mixed pair",
				rec.ID, id)
		}
	}
	if side.NoExternalEffects != rec.Transfer.NoExternalEffects {
		return fmt.Errorf("handoff %s declares transfer metadata but its sidecar disagrees on external-effect scope: re-export the record WITH its sidecar instead of resuming from a mixed pair",
			rec.ID)
	}
	return nil
}

// PendingDecisions returns the decisions still awaiting an outcome: pending
// blockers/approvals/verdicts. Decided and cancelled rows stay visible in the
// transfer but do not gate the retry.
func (t *Transfer) PendingDecisions() []Decision {
	var out []Decision
	for _, d := range t.Decisions {
		if d.Status == DecisionPending {
			out = append(out, d)
		}
	}
	return out
}

// redactTransfer masks free-text annotation fields in place before persistence.
func redactTransfer(t *Transfer) {
	if t == nil {
		return
	}
	for i, e := range t.Evidence {
		t.Evidence[i].Command = redactText(e.Command)
		t.Evidence[i].Output = redactText(e.Output)
	}
	for i, d := range t.Decisions {
		t.Decisions[i].Summary = redactText(d.Summary)
	}
	for i, s := range t.Next {
		t.Next[i].Step = redactText(s.Step)
	}
	for i, e := range t.Effects {
		t.Effects[i].Op = redactText(e.Op)
		t.Effects[i].Detail = redactText(e.Detail)
	}
}

// Render formats the transfer as the brief a cold replacement reads first:
// what exactly it is resuming, what changed, what is proven (and against
// which tree), what still waits on whom, what is safe to do next, and who
// holds the seat. Statuses that retire work — cancelled, superseded, active —
// come from the record and are repeated here so they cannot be missed.
func Render(t *Transfer, rec *Record) string {
	var s strings.Builder
	fmt.Fprintf(&s, "# Recovery transfer — handoff %s\n\n", t.HandoffID)
	if rec != nil {
		fmt.Fprintf(&s, "Record status: %s", rec.Status())
		if t.Owner.Holder != "" {
			fmt.Fprintf(&s, " — held by %s", t.Owner.Holder)
		}
		s.WriteString("\n")
		if rec.SupersededBy != "" {
			fmt.Fprintf(&s, "Superseded by: %s\n", rec.SupersededBy)
		}
		s.WriteString("\n")
	}
	s.WriteString("## Candidate (exact source identity)\n")
	fmt.Fprintf(&s, "- identity: %s\n", t.Candidate.Identity())
	if t.Candidate.Branch != "" {
		fmt.Fprintf(&s, "- branch: %s\n", t.Candidate.Branch)
	}
	if t.Candidate.SprintText != "" {
		fmt.Fprintf(&s, "- sprint story: %s (task pointer — not effect proof)\n", t.Candidate.SprintText)
	}
	if t.Candidate.WeaveIssue != 0 {
		fmt.Fprintf(&s, "- weave run: #%d (task pointer — not effect proof)\n", t.Candidate.WeaveIssue)
	}
	if !t.Candidate.Complete() {
		s.WriteString("- INCOMPLETE: repo/base missing — re-resolve the source before touching anything\n")
	}
	s.WriteString("\n## Changed files\n")
	if len(t.Changed) == 0 {
		s.WriteString("nothing in flight; the tree was clean.\n")
	} else {
		for _, f := range t.Changed {
			fmt.Fprintf(&s, "- %s (%s)\n", f.Path, f.Kind)
		}
	}
	s.WriteString("\n## Verification evidence\n")
	if len(t.Evidence) == 0 {
		s.WriteString("none carried — verify the candidate yourself before retrying it.\n")
	} else {
		for _, e := range t.Evidence {
			bound := "UNBOUND (history, not proof)"
			if e.Binds(t.Candidate.WorkSHA) {
				bound = "bound to this candidate"
			}
			exit := "?"
			if e.Exit != nil {
				exit = fmt.Sprintf("%d", *e.Exit)
			}
			fmt.Fprintf(&s, "- [%s] %s exit=%s %s — %s by %s\n",
				e.Kind, e.Command, exit, bound, e.At.UTC().Format(time.RFC3339), e.By)
			if e.Output != "" {
				fmt.Fprintf(&s, "  %s\n", transferFirstLine(e.Output))
			}
		}
	}
	s.WriteString("\n## Pending decisions and approvals\n")
	pending := t.PendingDecisions()
	if len(pending) == 0 {
		s.WriteString("none pending.\n")
	} else {
		for _, d := range pending {
			ref := ""
			if d.Ref != "" {
				ref = " [" + d.Ref + "]"
			}
			fmt.Fprintf(&s, "- (%s) %s%s\n", d.Kind, d.Summary, ref)
		}
	}
	if len(t.Cancellations) > 0 {
		s.WriteString("\n## Cancellations (visible, kept for the record)\n")
		for _, c := range t.Cancellations {
			fmt.Fprintf(&s, "- %s\n", c.Summary)
		}
	}
	s.WriteString("\n## Next safe steps\n")
	if len(t.Next) == 0 {
		s.WriteString("no ordered steps recorded — derive them from the continuity brief.\n")
	} else {
		for _, n := range t.Next {
			mark := " "
			if n.Done {
				mark = "x"
			} else if !t.SafeToAttempt(n) {
				mark = "!"
			}
			gate := ""
			if n.RequiresReconciliation {
				gate = " (needs reconciled effects)"
			}
			fmt.Fprintf(&s, "- [%s] %s%s\n", mark, n.Step, gate)
		}
	}
	s.WriteString("\n## External effects and retry gate\n")
	if len(t.Effects) == 0 {
		if t.NoExternalEffects {
			s.WriteString("author declared: no external effects. Gate open on scope.\n")
		} else {
			s.WriteString("SCOPE UNKNOWN: no effects declared and no-external-effects not recorded. Gate closed.\n")
		}
	} else {
		for _, e := range t.Effects {
			fmt.Fprintf(&s, "- %s %s: %s\n", e.ID, e.Op, e.Status)
		}
	}
	if err := t.VerifyRetrySafe(); err != nil {
		fmt.Fprintf(&s, "GATE: CLOSED — %s\n", err)
	} else {
		s.WriteString("GATE: OPEN — effects reconciled, reconciling steps may be attempted.\n")
	}
	s.WriteString("\n## Ownership (take through the ledger, not this file)\n")
	if t.Owner.HandoffClaim != "" {
		fmt.Fprintf(&s, "- handoff take: %s", t.Owner.HandoffClaim)
		if t.Owner.Holder != "" {
			fmt.Fprintf(&s, " (held by %s — a second take is refused, not queued)", t.Owner.Holder)
		}
		s.WriteString("\n")
	}
	s.WriteString("- take with `bashy resume --claim` (retired records are refused; concurrent takes serialize, loser names the winner)\n")
	if t.Readiness != nil && t.Readiness.Binding != "" {
		fmt.Fprintf(&s, "\n## Replacement readiness (S4, by reference)\n- binding: %s — %s (checked %s via %s)\n",
			t.Readiness.Binding, t.Readiness.Reason,
			t.Readiness.CheckedAt.UTC().Format(time.RFC3339), t.Readiness.Source)
	} else {
		s.WriteString("\n## Replacement readiness (S4, by reference)\n- NO readiness finding attached — verify the replacement binding before assigning it.\n")
	}
	return s.String()
}

func shortSHA(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func transferFirstLine(s string) string {
	if i := strings.Index(s, "\n"); i >= 0 {
		return s[:i]
	}
	return s
}
