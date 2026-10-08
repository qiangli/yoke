// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
)

// A DEPUTY IS AN OCCUPANCY OF THE STEWARD POSITION, NOT ANOTHER role.Kind
// (docs/orchestration-roles.md §3a). It is one scope field on the steward seat:
// a holder instance UUID snapshot, OnBehalfOf the steward, a non-overlapping
// scope (listed sprints or an epic), a time box, revocation, and the steward's
// own epoch as its fence. Nothing here is a generic authority framework — the
// grant, the revoke and every act are ordinary journal entries behind the same
// replay, hash chain and epoch gate as everything else the steward writes.

// DeputyScope is what a deputy answers for: listed sprints XOR one epic.
type DeputyScope struct {
	Sprints []int  `json:"sprints,omitempty"`
	Epic    string `json:"epic,omitempty"`
}

// Label is the durable address qualifier for deputy:<scope>: comma-joined
// sorted sprint ids, or "epic:<name>".
func (s DeputyScope) Label() string {
	if epic := strings.TrimSpace(s.Epic); epic != "" {
		return "epic:" + epic
	}
	ns := append([]int(nil), s.Sprints...)
	sort.Ints(ns)
	parts := make([]string, len(ns))
	for i, n := range ns {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ",")
}

// Valid reports whether the scope is well-formed.
func (s DeputyScope) Valid() error {
	hasSprints := len(s.Sprints) > 0
	hasEpic := strings.TrimSpace(s.Epic) != ""
	switch {
	case hasSprints && hasEpic:
		return &ErrDeputyScope{Why: "scope: --sprints and --epic are mutually exclusive"}
	case !hasSprints && !hasEpic:
		return &ErrDeputyScope{Why: "scope: either --sprints or --epic is required"}
	}
	seen := map[int]bool{}
	for _, n := range s.Sprints {
		if n <= 0 {
			return &ErrDeputyScope{Why: fmt.Sprintf("scope: sprint %d is not positive", n)}
		}
		if seen[n] {
			return &ErrDeputyScope{Why: fmt.Sprintf("scope: sprint %d duplicated", n)}
		}
		seen[n] = true
	}
	return nil
}

// EpicMembership is the existing sprint→epic lookup (the sprint board's Epic
// field). The steward cannot see the board itself, so the host injects it; with
// none injected, any question that crosses epic and sprint-list scopes FAILS
// CLOSED rather than guessing that they are disjoint.
type EpicMembership interface {
	// EpicOf reports the epic a sprint belongs to ("" for none).
	EpicOf(sprint int) (string, error)
}

// EpicMembershipFunc adapts a function.
type EpicMembershipFunc func(sprint int) (string, error)

func (f EpicMembershipFunc) EpicOf(sprint int) (string, error) { return f(sprint) }

// ErrMembershipUnknown is the fail-closed answer when a scope question needs
// epic membership the host did not provide.
type ErrMembershipUnknown struct {
	Sprint int
	Epic   string
	Cause  error
}

func (e *ErrMembershipUnknown) Error() string {
	msg := fmt.Sprintf("deputy: cannot establish whether sprint %d belongs to epic %q", e.Sprint, e.Epic)
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	} else {
		msg += " — no epic membership lookup is wired; refusing rather than assuming disjoint"
	}
	return msg
}

func (s *Store) sprintInEpic(sprint int, epic string) (bool, error) {
	if s.epicMembership == nil {
		return false, &ErrMembershipUnknown{Sprint: sprint, Epic: epic}
	}
	got, err := s.epicMembership.EpicOf(sprint)
	if err != nil {
		return false, &ErrMembershipUnknown{Sprint: sprint, Epic: epic, Cause: err}
	}
	return strings.EqualFold(strings.TrimSpace(got), strings.TrimSpace(epic)), nil
}

// ActTarget names what an act is over: one sprint, or one epic as a whole.
type ActTarget struct {
	Sprint int    `json:"sprint,omitempty"`
	Epic   string `json:"epic,omitempty"`
}

func (t ActTarget) empty() bool { return t.Sprint <= 0 && strings.TrimSpace(t.Epic) == "" }

func (t ActTarget) String() string {
	if t.Sprint > 0 {
		return fmt.Sprintf("sprint %d", t.Sprint)
	}
	return "epic " + strings.TrimSpace(t.Epic)
}

// Workstream is the journal strand an act on this target is recorded under.
// It is DERIVED, never caller-chosen, so an act cannot claim one sprint as its
// scope and write into another sprint's strand.
func (t ActTarget) Workstream() string {
	if t.Sprint > 0 {
		return fmt.Sprintf("sprint-%d", t.Sprint)
	}
	return "epic-" + strings.TrimSpace(t.Epic)
}

// contains reports whether scope covers target. An epic target is covered only
// by the same epic; a sprint target under an epic scope needs membership.
func (s *Store) contains(scope DeputyScope, t ActTarget) (bool, error) {
	if epic := strings.TrimSpace(t.Epic); t.Sprint <= 0 && epic != "" {
		return strings.EqualFold(strings.TrimSpace(scope.Epic), epic), nil
	}
	if t.Sprint <= 0 {
		return false, nil
	}
	if slices.Contains(scope.Sprints, t.Sprint) {
		return true, nil
	}
	if epic := strings.TrimSpace(scope.Epic); epic != "" {
		return s.sprintInEpic(t.Sprint, epic)
	}
	return false, nil
}

// overlaps reports whether two scopes intersect. Mixed epic/sprint-list scopes
// are resolved through EpicMembership, and fail closed without it.
func (s *Store) overlaps(a, b DeputyScope) (bool, error) {
	ae, be := strings.TrimSpace(a.Epic), strings.TrimSpace(b.Epic)
	switch {
	case ae != "" && be != "":
		return strings.EqualFold(ae, be), nil
	case ae == "" && be == "":
		for _, n := range a.Sprints {
			if slices.Contains(b.Sprints, n) {
				return true, nil
			}
		}
		return false, nil
	}
	epic, sprints := ae, b.Sprints
	if epic == "" {
		epic, sprints = be, a.Sprints
	}
	for _, n := range sprints {
		in, err := s.sprintInEpic(n, epic)
		if err != nil || in {
			return in, err
		}
	}
	return false, nil
}

// Deputy is one scoped occupancy of the steward position.
type Deputy struct {
	ID           string        `json:"id"`
	Holder       principal.Ref `json:"holder"` // instance UUID snapshot (Episode); never a label
	HolderHandle string        `json:"holder_handle,omitempty"`
	OnBehalfOf   principal.Ref `json:"on_behalf_of"`
	Scope        DeputyScope   `json:"scope"`
	ScopeLabel   string        `json:"scope_label"`
	GrantedAt    time.Time     `json:"granted_at"`
	ExpiresAt    time.Time     `json:"expires_at"`
	Revoked      bool          `json:"revoked"`
	RevokedAt    *time.Time    `json:"revoked_at,omitempty"`
	Epoch        uint64        `json:"epoch"`
}

// Expired reports whether the deputy has passed its time box.
func (d Deputy) Expired(now time.Time) bool {
	return !d.ExpiresAt.IsZero() && !now.Before(d.ExpiresAt)
}

// Active reports whether the occupancy is in force: not revoked, not expired,
// and granted under the steward's CURRENT epoch.
func (d Deputy) Active(now time.Time, currentEpoch uint64) bool {
	return !d.Revoked && !d.Expired(now) && d.Epoch == currentEpoch
}

// inactiveReason is the fence a non-active occupancy hit.
func (d Deputy) inactiveReason(now time.Time, currentEpoch uint64) error {
	switch {
	case d.Revoked:
		return &ErrDeputyRevoked{ID: d.ID}
	case d.Expired(now):
		return &ErrDeputyExpired{ID: d.ID}
	case d.Epoch != currentEpoch:
		return &ErrDeputyFenced{ID: d.ID, Presented: d.Epoch, Current: currentEpoch}
	}
	return nil
}

// isHolder matches an actor to this occupancy by instance UUID only.
func (d Deputy) isHolder(actor principal.Ref) bool {
	id := instanceUUIDOf(actor)
	return id != "" && id == d.Holder.Episode
}

// instanceUUIDOf extracts a canonical instance UUID from a principal: its
// Episode, or the name of a dhnt:agent/<uuid> URN. A label never qualifies.
func instanceUUIDOf(r principal.Ref) string {
	if id, err := fleet.ParseInstanceUUID(strings.TrimSpace(r.Episode)); err == nil {
		return id
	}
	if r.URN != "" {
		if kind, name, _, err := principal.ParseURN(r.URN); err == nil && kind == principal.KindAgent {
			if id, err := fleet.ParseInstanceUUID(name); err == nil {
				return id
			}
		}
	}
	return ""
}

// InstanceRef is the principal for one instance UUID: the UUID is the name and
// the episode, so nothing about it inherits a reusable label.
func InstanceRef(uuid string) principal.Ref {
	return principal.Ref{URN: principal.InstanceURN(uuid), Kind: principal.KindAgent, Name: uuid, Episode: uuid}
}

// DeputyResolver resolves a handle or UUID to a holder instance snapshot at
// grant time.
type DeputyResolver interface {
	Resolve(handle string) (principal.Ref, error)
}

// InstanceResolver is the production DeputyResolver over the accepted
// fleet.InstanceStore: a UUID must name an existing, unretired record, and a
// handle/label must resolve to exactly one live instance. A UUID-shaped string
// is never trusted on its own.
type InstanceResolver struct{ Store *fleet.InstanceStore }

// Resolve implements DeputyResolver.
func (r InstanceResolver) Resolve(handle string) (principal.Ref, error) {
	handle = strings.TrimSpace(handle)
	if handle == "" {
		return principal.Ref{}, &ErrDeputyScope{Why: "holder handle is required"}
	}
	if r.Store == nil {
		return principal.Ref{}, &ErrDeputyScope{Why: "no instance store — a holder cannot be resolved"}
	}
	var inst fleet.Instance
	if _, perr := fleet.ParseInstanceUUID(handle); perr == nil {
		got, err := r.Store.Get(handle)
		if err != nil {
			return principal.Ref{}, fmt.Errorf("deputy: holder %q: %w", handle, err)
		}
		inst = got
	} else {
		got, ok, err := r.Store.ResolveHandle(handle)
		if err != nil {
			return principal.Ref{}, fmt.Errorf("deputy: holder %q: %w", handle, err)
		}
		if !ok {
			return principal.Ref{}, fmt.Errorf("deputy: holder %q names no live instance", handle)
		}
		inst = got
	}
	if !inst.Active() {
		return principal.Ref{}, fmt.Errorf("deputy: instance %s is retired (since %s) — a retired instance cannot hold an occupancy", inst.UUID, inst.Retired)
	}
	return InstanceRef(inst.UUID), nil
}

type deputyRecord struct {
	Deputy Deputy `json:"deputy"`
}

// deriveDeputies folds the journal into every deputy grant ever made, with
// revocations applied. Fencing/expiry are judged by the caller at a time.
func deriveDeputies(entries []Entry) map[string]Deputy {
	out := map[string]Deputy{}
	for _, e := range entries {
		switch e.Kind {
		case KindDeputyGranted:
			for _, ev := range e.Evidence {
				if ev.Kind != "deputy" || ev.Note != "granted" {
					continue
				}
				var rec deputyRecord
				if json.Unmarshal([]byte(ev.Ref), &rec) == nil && rec.Deputy.ID != "" {
					rec.Deputy.ScopeLabel = rec.Deputy.Scope.Label()
					out[rec.Deputy.ID] = rec.Deputy
				}
			}
		case KindDeputyRevoked:
			for _, ev := range e.Evidence {
				if ev.Kind != "deputy" || ev.Note != "revoke" {
					continue
				}
				if d, ok := out[ev.Ref]; ok {
					at := parseTime(e.Time)
					d.Revoked, d.RevokedAt = true, &at
					out[ev.Ref] = d
				}
			}
		}
	}
	return out
}

func sortedDeputies(m map[string]Deputy) []Deputy {
	out := make([]Deputy, 0, len(m))
	for _, d := range m {
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].GrantedAt.Equal(out[j].GrantedAt) {
			return out[i].GrantedAt.Before(out[j].GrantedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// ─── errors ───────────────────────────────────────────────────────────────────

type ErrDeputyOverlap struct {
	Scope     DeputyScope
	Conflicts []Deputy
}

func (e *ErrDeputyOverlap) Error() string {
	labels := make([]string, len(e.Conflicts))
	for i, d := range e.Conflicts {
		labels[i] = d.ScopeLabel + "(" + d.ID + ")"
	}
	return fmt.Sprintf("deputy: scope %q overlaps active %s — deputy scopes do not overlap", e.Scope.Label(), strings.Join(labels, ", "))
}

type ErrDeputyScope struct{ Why string }

func (e *ErrDeputyScope) Error() string { return "deputy: " + e.Why }

type ErrDeputyNotFound struct{ ID string }

func (e *ErrDeputyNotFound) Error() string { return fmt.Sprintf("deputy: no such deputy %q", e.ID) }

type ErrDeputyFenced struct {
	ID        string
	Presented uint64
	Current   uint64
}

func (e *ErrDeputyFenced) Error() string {
	return fmt.Sprintf("deputy %q is fenced — granted at epoch %d but the steward is at epoch %d; a steward takeover fences every deputy of the old epoch", e.ID, e.Presented, e.Current)
}

type ErrDeputyExpired struct{ ID string }

func (e *ErrDeputyExpired) Error() string { return fmt.Sprintf("deputy %q has expired", e.ID) }

type ErrDeputyRevoked struct{ ID string }

func (e *ErrDeputyRevoked) Error() string { return fmt.Sprintf("deputy %q has been revoked", e.ID) }

type ErrDeputyOutOfScope struct {
	Act    string
	Target ActTarget
	Scopes []string
}

func (e *ErrDeputyOutOfScope) Error() string {
	return fmt.Sprintf("deputy cannot %s %s — outside its scope %s; cross-scope allocation/release/integration stays with the steward", e.Act, e.Target, strings.Join(e.Scopes, " "))
}

type ErrDeputyStewardOnly struct{ Act string }

func (e *ErrDeputyStewardOnly) Error() string {
	return fmt.Sprintf("deputy: %q is steward-only — a deputy may activate, fence, judge and gate inside its scope; cross-scope allocation/release/integration and deputy grants stay with the steward", e.Act)
}

type ErrDeputySelfConduct struct {
	ID     string
	Target ActTarget
}

func (e *ErrDeputySelfConduct) Error() string {
	return fmt.Sprintf("deputy %q cannot conduct %s inside its own scope — independence: a deputy never conducts what it supervises", e.ID, e.Target)
}

type ErrDeputySelfJudge struct {
	ID     string
	Target ActTarget
	Who    string
}

func (e *ErrDeputySelfJudge) Error() string {
	return fmt.Sprintf("deputy %q cannot judge %s — it is the judged %s; independence requires a different judge", e.ID, e.Target, e.Who)
}

type ErrDeputyIsDeputy struct{}

func (e *ErrDeputyIsDeputy) Error() string {
	return "deputy: no deputies of deputies — only the steward may grant deputies"
}

// The steward's four acts (§2b). A deputy may perform exactly these, inside its
// scope; conducting is a conductor act it may never take inside its scope.
const (
	ActActivate = "activate"
	ActFence    = "fence"
	ActJudge    = "judge"
	ActGate     = "gate"
)

var fourActs = map[string]bool{ActActivate: true, ActFence: true, ActJudge: true, ActGate: true}

// MaxDeputyTTL bounds a deputy's time box.
const MaxDeputyTTL = 7 * 24 * time.Hour

func deputyID(now time.Time) string {
	return fmt.Sprintf("dep-%s-%09d", now.UTC().Format("20060102T150405"), now.Nanosecond())
}

// ─── grant / list / revoke (steward only) ─────────────────────────────────────

// DeputyAdd grants a scoped deputy occupancy. holder must be a resolved
// instance snapshot (see InstanceResolver); the grant is journaled under the
// steward's verified epoch.
func (s *Store) DeputyAdd(actor principal.Ref, epoch uint64, holder principal.Ref, holderHandle string, scope DeputyScope, ttl time.Duration, now time.Time) (Deputy, error) {
	now = mustUTC(now)
	if err := scope.Valid(); err != nil {
		return Deputy{}, err
	}
	uuid := instanceUUIDOf(holder)
	if uuid == "" {
		return Deputy{}, &ErrDeputyScope{Why: "holder must be a resolved instance UUID snapshot — a handle or label is not an identity"}
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if ttl > MaxDeputyTTL {
		return Deputy{}, &ErrDeputyScope{Why: fmt.Sprintf("time box %s exceeds %s", ttl, MaxDeputyTTL)}
	}
	var out Deputy
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		if rep.Corrupt {
			return &ErrCorruptTail{Line: rep.CorruptLine, Reason: rep.CorruptReason, Kind: rep.CorruptKind, ValidEntries: len(rep.Entries)}
		}
		cur := deriveAuthority(rep).Epoch
		deps := sortedDeputies(deriveDeputies(rep.Entries))
		for _, d := range deps {
			if d.Active(now, cur) && d.isHolder(actor) {
				return &ErrDeputyIsDeputy{}
			}
		}
		auth, err := authorize(rep, actor, epoch)
		if err != nil {
			return err
		}
		if SameHolder(auth.Holder, holder) {
			return &ErrDeputyScope{Why: "the steward cannot deputize itself"}
		}
		var conflicts []Deputy
		for _, d := range deps {
			if !d.Active(now, auth.Epoch) {
				continue
			}
			hit, err := s.overlaps(d.Scope, scope)
			if err != nil {
				return err
			}
			if hit {
				conflicts = append(conflicts, d)
			}
		}
		if len(conflicts) > 0 {
			return &ErrDeputyOverlap{Scope: scope, Conflicts: conflicts}
		}
		scope.Sprints = append([]int(nil), scope.Sprints...)
		sort.Ints(scope.Sprints)
		scope.Epic = strings.TrimSpace(scope.Epic)
		dep := Deputy{
			ID:           deputyID(now),
			Holder:       InstanceRef(uuid),
			HolderHandle: strings.TrimSpace(holderHandle),
			OnBehalfOf:   auth.Holder,
			Scope:        scope,
			ScopeLabel:   scope.Label(),
			GrantedAt:    now,
			ExpiresAt:    now.Add(ttl),
			Epoch:        auth.Epoch,
		}
		b, _ := json.Marshal(deputyRecord{Deputy: dep})
		if _, err := s.appendAuthorized(rep, Entry{
			Actor:   actor,
			Kind:    KindDeputyGranted,
			Summary: fmt.Sprintf("deputy %s granted to instance %s for scope %q until %s (epoch %d)", dep.ID, uuid, dep.ScopeLabel, dep.ExpiresAt.Format(time.RFC3339), auth.Epoch),
			Outcome: OutcomeSuccess,
			Evidence: []Evidence{
				{Kind: "deputy", Ref: string(b), Note: "granted"},
			},
		}, epoch, now); err != nil {
			return err
		}
		out = dep
		return nil
	})
	return out, err
}

// DeputyList returns every grant in the journal, oldest first. Callers judge
// Active at a time.
func (s *Store) DeputyList() ([]Deputy, uint64, error) {
	rep, err := s.Replay()
	if err != nil {
		return nil, 0, err
	}
	return sortedDeputies(deriveDeputies(rep.Entries)), deriveAuthority(rep).Epoch, nil
}

// DeputyRevoke ends a deputy occupancy. Steward only.
func (s *Store) DeputyRevoke(actor principal.Ref, epoch uint64, id string, now time.Time) error {
	now = mustUTC(now)
	id = strings.TrimSpace(id)
	return s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		if _, err := authorize(rep, actor, epoch); err != nil {
			return err
		}
		d, ok := deriveDeputies(rep.Entries)[id]
		if !ok {
			return &ErrDeputyNotFound{ID: id}
		}
		if d.Revoked {
			return &ErrDeputyRevoked{ID: id}
		}
		_, err = s.appendAuthorized(rep, Entry{
			Actor:    actor,
			Kind:     KindDeputyRevoked,
			Summary:  fmt.Sprintf("deputy %s revoked", id),
			Outcome:  OutcomeSuccess,
			Evidence: []Evidence{{Kind: "deputy", Ref: id, Note: "revoke"}},
		}, epoch, now)
		return err
	})
}

// ─── the act guard ────────────────────────────────────────────────────────────

// authorizeAct is the single gate for the four acts. The steward's own gate
// runs FIRST and its epoch checks are absolute: a vacant seat, a zero epoch or
// a stale epoch is refused here for everybody — a deputy never "falls through"
// a failed steward check to a grant that merely matches the current epoch.
// Only a holder mismatch at the CURRENT epoch is considered for the deputy
// path, and then: the act must be one of four, the target non-empty, the
// actor's grants are filtered to the requested target FIRST and only then
// checked for being in force — so a newer grant elsewhere cannot mask an
// older in-scope one, and an in-scope grant that lapsed reports its fence.
func (s *Store) authorizeAct(rep *Replay, actor principal.Ref, epoch uint64, act string, t ActTarget, now time.Time) (Authority, *Deputy, error) {
	if !fourActs[act] {
		return Authority{}, nil, fmt.Errorf("steward: %q is not one of the four acts (activate, fence, judge, gate)", act)
	}
	auth, err := authorize(rep, actor, epoch)
	if err == nil {
		return auth, nil, nil
	}
	if _, notHolder := err.(*ErrNotHolder); !notHolder {
		return Authority{}, nil, err
	}
	if auth = deriveAuthority(rep); auth.Vacant {
		return Authority{}, nil, err
	}
	var own []Deputy
	for _, d := range sortedDeputies(deriveDeputies(rep.Entries)) {
		if d.isHolder(actor) {
			own = append(own, d)
		}
	}
	if len(own) == 0 {
		return Authority{}, nil, err
	}
	if t.empty() {
		return Authority{}, nil, &ErrDeputyStewardOnly{Act: act + " without a sprint or epic"}
	}
	var inScope []Deputy
	var labels []string
	for _, d := range own {
		if d.Active(now, auth.Epoch) {
			labels = append(labels, "deputy:"+d.ScopeLabel)
		}
		hit, cerr := s.contains(d.Scope, t)
		if cerr != nil {
			return Authority{}, nil, cerr
		}
		if hit {
			inScope = append(inScope, d)
		}
	}
	if len(inScope) == 0 {
		return Authority{}, nil, &ErrDeputyOutOfScope{Act: act, Target: t, Scopes: labels}
	}
	for i := len(inScope) - 1; i >= 0; i-- {
		if inScope[i].Active(now, auth.Epoch) {
			d := inScope[i]
			return auth, &d, nil
		}
	}
	return Authority{}, nil, inScope[len(inScope)-1].inactiveReason(now, auth.Epoch)
}

// ownActiveIn reports the actor's active occupancy covering t, if any — every
// own grant is considered, not just the one an act selected.
func (s *Store) ownActiveIn(rep *Replay, who principal.Ref, t ActTarget, now time.Time) (*Deputy, error) {
	cur := deriveAuthority(rep).Epoch
	for _, d := range sortedDeputies(deriveDeputies(rep.Entries)) {
		if !d.isHolder(who) || !d.Active(now, cur) {
			continue
		}
		hit, err := s.contains(d.Scope, t)
		if err != nil {
			return nil, err
		}
		if hit {
			return &d, nil
		}
	}
	return nil, nil
}

// MayConduct is the independence check for a conductor appointment: an
// instance that holds an active deputy occupancy covering the target may not
// conduct it. It is the occupancy lookup the weave/sprint lease takes.
func (s *Store) MayConduct(who principal.Ref, t ActTarget, now time.Time) error {
	rep, err := s.Replay()
	if err != nil {
		return err
	}
	if rep.Corrupt {
		return &ErrCorruptTail{Line: rep.CorruptLine, Reason: rep.CorruptReason, Kind: rep.CorruptKind, ValidEntries: len(rep.Entries)}
	}
	d, err := s.ownActiveIn(rep, who, t, mustUTC(now))
	if err != nil {
		return err
	}
	if d != nil {
		return &ErrDeputySelfConduct{ID: d.ID, Target: t}
	}
	return nil
}

// ownerRef reads a workstream owner string as a principal: an instance UUID,
// a dhnt:agent/<uuid> URN, or (legacy) a bare name that can match nobody's UUID.
func ownerRef(owner string) principal.Ref {
	owner = strings.TrimSpace(owner)
	if id, err := fleet.ParseInstanceUUID(owner); err == nil {
		return InstanceRef(id)
	}
	if strings.HasPrefix(owner, "dhnt:") {
		return principal.Ref{URN: owner}
	}
	return principal.Ref{Name: owner}
}

// ─── the four acts: real journal mutations ────────────────────────────────────

// ActRequest is one of the four acts over a sprint or an epic.
type ActRequest struct {
	Act    string
	Target ActTarget
	// Owner is the conductor the act installs (activate, fence). Empty with
	// fence evicts the holder without a successor.
	Owner     string
	Summary   string
	Rationale string
	Outcome   Outcome // judge: success (accept) or failed/abandoned (close at box)
	Evidence  []Evidence
	// TargetSeq (judge, gate) is the journal entry being judged; its author is
	// checked for independence along with the workstream's conductor.
	TargetSeq uint64
}

// Act performs one of the steward's four acts as a journal mutation, gated by
// authorizeAct. It is the path `steward act` drives for both the steward and
// its deputies:
//
//	activate  workstream update installing Owner (the scope transfer)
//	fence     workstream update replacing/evicting Owner, lane blocked→active
//	judge     workstream close with the judged outcome
//	gate      decision record carrying the merge-gate verdict
func (s *Store) Act(actor principal.Ref, epoch uint64, req ActRequest, now time.Time) (Entry, error) {
	now = mustUTC(now)
	if req.Target.Sprint > 0 && strings.TrimSpace(req.Target.Epic) != "" {
		return Entry{}, &ErrDeputyScope{Why: "an act targets a sprint or an epic, not both"}
	}
	var out Entry
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		auth, dep, err := s.authorizeAct(rep, actor, epoch, req.Act, req.Target, now)
		if err != nil {
			return err
		}
		if req.Target.empty() {
			return &ErrDeputyScope{Why: "an act needs --sprint or --epic"}
		}
		ws := req.Target.Workstream()
		board := ProjectBoard(rep.Entries, nil)
		var cur *Workstream
		for i := range board.Workstreams {
			if board.Workstreams[i].Name == ws {
				cur = &board.Workstreams[i]
			}
		}
		e := Entry{Actor: actor, Workstream: ws, Summary: req.Summary, Rationale: req.Rationale, Evidence: append([]Evidence(nil), req.Evidence...)}
		switch req.Act {
		case ActActivate, ActFence:
			owner := strings.TrimSpace(req.Owner)
			if req.Act == ActActivate && owner == "" {
				return &ErrDeputyScope{Why: "activate needs --owner: the conductor the scope transfers to"}
			}
			if owner != "" {
				// Independence: nobody holding an active occupancy over this
				// target may be installed as its conductor.
				if d, err := s.ownActiveIn(rep, ownerRef(owner), req.Target, now); err != nil {
					return err
				} else if d != nil {
					return &ErrDeputySelfConduct{ID: d.ID, Target: req.Target}
				}
			}
			if cur == nil {
				e.Kind = KindWorkstreamOpen
				e.Update = &WorkstreamUpdate{Lane: LaneInProgress, Owner: owner}
			} else {
				e.Kind = KindWorkstreamUpdate
				u := &WorkstreamUpdate{Owner: owner}
				if owner == "" {
					u.Clear = []string{"owner"}
				}
				if req.Act == ActFence {
					u.Lane = LaneBlocked
					if owner != "" {
						u.Lane = LaneInProgress
					}
				}
				e.Update = u
			}
		case ActJudge:
			if cur == nil {
				return &ErrDeputyScope{Why: fmt.Sprintf("nothing to judge: no workstream %q", ws)}
			}
			if dep != nil {
				if err := judgeIndependent(dep, rep, cur, req); err != nil {
					return err
				}
			}
			e.Kind = KindWorkstreamClose
			e.Outcome = req.Outcome
			if e.Outcome == "" {
				e.Outcome = OutcomeSuccess
			}
		case ActGate:
			if dep != nil && cur != nil {
				if err := judgeIndependent(dep, rep, cur, req); err != nil {
					return err
				}
			}
			e.Kind = KindDecision
			if e.Rationale == "" {
				e.Rationale = "merge gate"
			}
		}
		if e.Summary == "" {
			e.Summary = fmt.Sprintf("%s %s", req.Act, req.Target)
		}
		e.Evidence = append(e.Evidence, Evidence{Kind: "act", Ref: req.Act, Note: req.Target.String()})
		if req.TargetSeq > 0 {
			e.Evidence = append(e.Evidence, Evidence{Kind: "entry", Ref: fmt.Sprintf("seq:%d", req.TargetSeq), Note: "judged"})
		}
		if dep != nil {
			e.Evidence = append(e.Evidence, Evidence{Kind: "deputy", Ref: dep.ID, Note: "on-behalf-of " + holderName(dep.OnBehalfOf) + " scope " + dep.ScopeLabel})
		}
		if e.Kind.SeatEvent() {
			return fmt.Errorf("steward: %s is a seat lifecycle event", e.Kind)
		}
		e.Epoch = auth.Epoch
		stored, err := appendEntry(s.journalPath(), rep, e, now)
		if err != nil {
			return err
		}
		out = stored
		if dep == nil {
			// The steward writing is a heartbeat (as in Record); a deputy's act
			// is not the steward's liveness.
			return committed("act", stored.Seq, stored.Epoch, s.writeSeat(auth, "", now))
		}
		return nil
	})
	return out, err
}

// judgeIndependent refuses a deputy judging what it conducted or authored.
func judgeIndependent(dep *Deputy, rep *Replay, ws *Workstream, req ActRequest) error {
	if ws != nil && ws.Owner != "" && dep.isHolder(ownerRef(ws.Owner)) {
		return &ErrDeputySelfJudge{ID: dep.ID, Target: req.Target, Who: "conductor"}
	}
	if req.TargetSeq > 0 {
		judged, ok := entryBySeq(rep.Entries, req.TargetSeq)
		if !ok {
			return &ErrDeputyScope{Why: fmt.Sprintf("judged entry seq %d does not exist", req.TargetSeq)}
		}
		if judged.Workstream != req.Target.Workstream() {
			return &ErrDeputyScope{Why: fmt.Sprintf("judged entry seq %d belongs to %q, not %q", req.TargetSeq, judged.Workstream, req.Target.Workstream())}
		}
		if dep.isHolder(judged.Actor) {
			return &ErrDeputySelfJudge{ID: dep.ID, Target: req.Target, Who: "author"}
		}
	}
	for _, e := range rep.Entries {
		if e.Workstream == req.Target.Workstream() && (e.Kind == KindWorkstreamOpen || e.Kind == KindWorkstreamUpdate) &&
			e.Update != nil && e.Update.Owner != "" && dep.isHolder(ownerRef(e.Update.Owner)) {
			return &ErrDeputySelfJudge{ID: dep.ID, Target: req.Target, Who: "conductor"}
		}
	}
	return nil
}

// ─── occupancy lookup for role addressing ─────────────────────────────────────

// DeputyOccupancy is the stable lookup the addressing layer consumes:
// deputy:<scope> is a durable address that persists through vacancy and
// handoff; Holder is the CURRENT instance UUID snapshot, or empty when vacant.
// It never carries a label, so a reused label inherits nothing.
type DeputyOccupancy struct {
	Address  string      `json:"address"` // deputy:<scope>
	Topic    string      `json:"topic"`   // deputy.<scope>
	Scope    DeputyScope `json:"scope"`
	DeputyID string      `json:"deputy_id,omitempty"`
	Holder   string      `json:"holder,omitempty"` // instance UUID; "" = vacant
	Vacant   bool        `json:"vacant"`
	State    string      `json:"state"` // active | revoked | expired | fenced
}

// DeputyAddress is the role address for a scope.
func DeputyAddress(scope DeputyScope) string { return "deputy:" + scope.Label() }

// DeputyTopic is the bus topic for a scope.
func DeputyTopic(scope DeputyScope) string { return "deputy." + scope.Label() }

// DeputyOccupancies reports every deputy address ever granted on this seat,
// keyed by scope label, with its current holder (latest grant wins).
func (s *Store) DeputyOccupancies(now time.Time) ([]DeputyOccupancy, error) {
	now = mustUTC(now)
	rep, err := s.Replay()
	if err != nil {
		return nil, err
	}
	if rep.Corrupt {
		return nil, &ErrCorruptTail{Line: rep.CorruptLine, Reason: rep.CorruptReason, Kind: rep.CorruptKind, ValidEntries: len(rep.Entries)}
	}
	cur := deriveAuthority(rep).Epoch
	byLabel := map[string]DeputyOccupancy{}
	var order []string
	for _, d := range sortedDeputies(deriveDeputies(rep.Entries)) {
		o := DeputyOccupancy{Address: DeputyAddress(d.Scope), Topic: DeputyTopic(d.Scope), Scope: d.Scope, DeputyID: d.ID, Vacant: true, State: "active"}
		if d.Active(now, cur) {
			o.Holder, o.Vacant = d.Holder.Episode, false
		} else {
			switch d.inactiveReason(now, cur).(type) {
			case *ErrDeputyRevoked:
				o.State = "revoked"
			case *ErrDeputyExpired:
				o.State = "expired"
			default:
				o.State = "fenced"
			}
		}
		if prev, seen := byLabel[d.ScopeLabel]; !seen {
			order = append(order, d.ScopeLabel)
		} else if !prev.Vacant && o.Vacant {
			continue // an older live holder outranks a newer lapsed grant
		}
		byLabel[d.ScopeLabel] = o
	}
	out := make([]DeputyOccupancy, 0, len(order))
	for _, l := range order {
		out = append(out, byLabel[l])
	}
	return out, nil
}

// DeputyOccupant resolves one deputy:<scope> address (or bare scope label).
func (s *Store) DeputyOccupant(address string, now time.Time) (DeputyOccupancy, bool, error) {
	label := strings.TrimPrefix(strings.TrimSpace(address), "deputy:")
	all, err := s.DeputyOccupancies(now)
	if err != nil {
		return DeputyOccupancy{}, false, err
	}
	for _, o := range all {
		if o.Scope.Label() == label {
			return o, true, nil
		}
	}
	return DeputyOccupancy{}, false, nil
}
