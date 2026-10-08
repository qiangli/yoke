// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package steward

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/principal"
)

// Deputy scope kinds.
// A deputy is an occupancy of the steward position, not another role.Kind.
// It carries a non-overlapping scope (listed sprints or an epic), a time box,
// and is fenced by the steward epoch ladder.
type DeputyScope struct {
	Sprints []int  `json:"sprints,omitempty"`
	Epic    string `json:"epic,omitempty"`
}

// Label is the durable address qualifier for deputy:<scope>.
// For listed sprints it is comma-joined ints; for an epic it is "epic:<name>".
func (s DeputyScope) Label() string {
	if epic := strings.TrimSpace(s.Epic); epic != "" {
		return "epic:" + epic
	}
	if len(s.Sprints) == 0 {
		return ""
	}
	parts := make([]string, len(s.Sprints))
	for i, n := range s.Sprints {
		parts[i] = fmt.Sprint(n)
	}
	return strings.Join(parts, ",")
}

// Valid reports whether the scope is well-formed: exactly one of sprints or epic,
// non-empty, sprints positive and deduplicated, epic non-blank.
func (s DeputyScope) Valid() error {
	hasSprints := len(s.Sprints) > 0
	hasEpic := strings.TrimSpace(s.Epic) != ""
	switch {
	case hasSprints && hasEpic:
		return fmt.Errorf("deputy scope: sprints and epic are mutually exclusive")
	case !hasSprints && !hasEpic:
		return fmt.Errorf("deputy scope: either --sprints or --epic is required")
	case hasSprints:
		seen := map[int]bool{}
		for _, n := range s.Sprints {
			if n <= 0 {
				return fmt.Errorf("deputy scope: sprint %d is not positive", n)
			}
			if seen[n] {
				return fmt.Errorf("deputy scope: sprint %d duplicated", n)
			}
			seen[n] = true
		}
		return nil
	default:
		epic := strings.TrimSpace(s.Epic)
		if epic == "" {
			return fmt.Errorf("deputy scope: epic is blank")
		}
		return nil
	}
}

// Contains reports whether the scope covers the given sprint or epic.
// Sprint checks are membership; epic is exact match (case trimmed).
func (s DeputyScope) Contains(sprint int, epic string) bool {
	if epic = strings.TrimSpace(epic); epic != "" && strings.TrimSpace(s.Epic) != "" {
		return strings.EqualFold(strings.TrimSpace(s.Epic), epic)
	}
	if sprint > 0 {
		for _, n := range s.Sprints {
			if n == sprint {
				return true
			}
		}
	}
	return false
}

// Overlaps reports whether two scopes intersect: overlapping sprint sets or same epic.
func (s DeputyScope) Overlaps(other DeputyScope) bool {
	aEpic := strings.TrimSpace(s.Epic)
	bEpic := strings.TrimSpace(other.Epic)
	if aEpic != "" && bEpic != "" {
		return strings.EqualFold(aEpic, bEpic)
	}
	if aEpic != "" || bEpic != "" {
		// epic vs sprints: no deterministic overlap without epic->sprint mapping.
		// Keep disjoint by definition; caller can extend if epic membership is known.
		return false
	}
	// both are sprint lists
	set := map[int]bool{}
	for _, n := range s.Sprints {
		set[n] = true
	}
	for _, n := range other.Sprints {
		if set[n] {
			return true
		}
	}
	return false
}

// Deputy is one scoped occupancy of the steward position.
// Holder is the resolved instance snapshot at grant time (UUID in Episode).
// OnBehalfOf is the steward principal that remains accountable.
// Epoch is the steward epoch at grant time for fencing; a changed steward
// epoch fences every deputy granted under the old one.
type Deputy struct {
	ID           string       `json:"id"`
	Holder       principal.Ref `json:"holder"`
	HolderHandle string       `json:"holder_handle,omitempty"`
	OnBehalfOf   principal.Ref `json:"on_behalf_of"`
	Scope        DeputyScope  `json:"scope"`
	ScopeLabel   string       `json:"scope_label"`
	GrantedAt    time.Time    `json:"granted_at"`
	ExpiresAt    time.Time    `json:"expires_at"`
	Revoked      bool         `json:"revoked"`
	RevokedAt    *time.Time   `json:"revoked_at,omitempty"`
	Epoch        uint64       `json:"epoch"`
	GrantID      string       `json:"grant_id,omitempty"`
}

// Expired reports whether the deputy has passed its time box.
func (d Deputy) Expired(now time.Time) bool {
	if d.ExpiresAt.IsZero() {
		return false
	}
	return !now.Before(d.ExpiresAt)
}

// Active reports whether the deputy is currently effective.
func (d Deputy) Active(now time.Time, currentEpoch uint64) bool {
	if d.Revoked {
		return false
	}
	if d.Expired(now) {
		return false
	}
	if d.Epoch != currentEpoch {
		return false
	}
	return true
}

// KindDeputyGranted / KindDeputyRevoked are defined in journal.go — reused here.

// DeputyResolver resolves a human handle to a holder instance snapshot at grant time.
// Kept as an interface so authority tests stay independent of fleet wiring.
// Production wiring will use fleet.InstanceStore; tests pass a direct Ref.
type DeputyResolver interface {
	Resolve(handle string) (principal.Ref, error)
}

// deputyPayload is stored in Entry.Evidence for deputy journal entries,
// plus the structured Deputy in the entry's Evidence note.
// We also carry the full Deputy as JSON in the entry's Artifact for
// explicit replay without parsing evidence.
type deputyRecord struct {
	Deputy Deputy `json:"deputy"`
}

// deriveDeputies folds the journal into the current deputy set.
// Last writer wins per deputy ID; revocations mark Revoked.
func deriveDeputies(entries []Entry) map[string]Deputy {
	out := map[string]Deputy{}
	for _, e := range entries {
		switch e.Kind {
		case KindDeputyGranted:
			var rec deputyRecord
			if len(e.Evidence) > 0 {
				for _, ev := range e.Evidence {
					if ev.Kind == "deputy" && ev.Digest == "" {
						_ = json.Unmarshal([]byte(ev.Ref), &rec)
						break
					}
				}
			}
			// Also try artifact if present
			if rec.Deputy.ID == "" && e.Artifact != nil && e.Artifact.Path != "" {
				// artifact path not replayed here; Evidence is the projection
			}
			// Legacy fallback: parse deputy from entry Ref/ID if needed
			if rec.Deputy.ID == "" {
				continue
			}
			dep := rec.Deputy
			// Ensure ScopeLabel derived
			if dep.ScopeLabel == "" {
				dep.ScopeLabel = dep.Scope.Label()
			}
			out[dep.ID] = dep
		case KindDeputyRevoked:
			var id string
			for _, ev := range e.Evidence {
				if ev.Kind == "deputy" && ev.Note == "revoke" {
					id = ev.Ref
					break
				}
			}
			if id == "" {
				continue
			}
			if d, ok := out[id]; ok {
				d.Revoked = true
				now := parseTime(e.Time)
				d.RevokedAt = &now
				out[id] = d
			}
		}
	}
	return out
}

// Deputy authority errors

type ErrDeputyOverlap struct {
	Scope      DeputyScope
	Conflicts  []Deputy
}

func (e *ErrDeputyOverlap) Error() string {
	labels := make([]string, len(e.Conflicts))
	for i, d := range e.Conflicts {
		labels[i] = d.ScopeLabel + "(" + d.ID + ")"
	}
	return fmt.Sprintf("deputy: overlapping scope %q conflicts with existing %s", e.Scope.Label(), strings.Join(labels, ", "))
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
	return fmt.Sprintf("deputy %q is fenced — granted at epoch %d but steward is at epoch %d; a steward takeover fences every deputy of the old epoch", e.ID, e.Presented, e.Current)
}

type ErrDeputyExpired struct{ ID string }

func (e *ErrDeputyExpired) Error() string { return fmt.Sprintf("deputy %q has expired", e.ID) }

type ErrDeputyRevoked struct{ ID string }

func (e *ErrDeputyRevoked) Error() string { return fmt.Sprintf("deputy %q has been revoked", e.ID) }

type ErrDeputyOutOfScope struct {
	ID     string
	Action string
	Scope  DeputyScope
}

func (e *ErrDeputyOutOfScope) Error() string {
	return fmt.Sprintf("deputy %q cannot %q outside its scope %q — cross-scope allocation/release/integration stays with the steward", e.ID, e.Action, e.Scope.Label())
}

type ErrDeputySelfConduct struct {
	ID     string
	Sprint int
}

func (e *ErrDeputySelfConduct) Error() string {
	return fmt.Sprintf("deputy %q cannot conduct sprint %d inside its own scope — independence: a deputy cannot conduct a sprint it is meant to supervise", e.ID, e.Sprint)
}

type ErrDeputyIsDeputy struct{}

func (e *ErrDeputyIsDeputy) Error() string {
	return "deputy: no deputies of deputies — only the steward may grant deputies"
}

// Deputy actions that a deputy may perform within scope.
// Cross-scope allocation/release/integration are steward-only.
const (
	DeputyActionActivate = "activate"
	DeputyActionFence    = "fence"
	DeputyActionJudge    = "judge"
	DeputyActionGate     = "gate"
	DeputyActionConduct  = "conduct"
)

var allowedDeputyActions = map[string]bool{
	DeputyActionActivate: true,
	DeputyActionFence:    true,
	DeputyActionJudge:    true,
	DeputyActionGate:     true,
}

// deputyID generates an ID for a deputy grant.
func deputyID(now time.Time) string {
	return fmt.Sprintf("dep-%s-%04d", now.UTC().Format("20060102T150405"), now.Nanosecond()%10000)
}

// DeputyAdd grants a scoped deputy occupancy. The holder is a resolved
// instance snapshot (UUID in Episode). Scope is checked for non-overlap,
// the time box is applied, and the grant is journaled under the steward's
// fencing epoch. Only the current steward holder may grant.
func (s *Store) DeputyAdd(actor principal.Ref, epoch uint64, holder principal.Ref, holderHandle string, scope DeputyScope, ttl time.Duration, now time.Time) (Deputy, error) {
	now = mustUTC(now)
	if err := scope.Valid(); err != nil {
		return Deputy{}, err
	}
	if strings.TrimSpace(holder.Name) == "" && strings.TrimSpace(holder.Episode) == "" {
		return Deputy{}, &ErrDeputyScope{Why: "deputy holder is empty — pass an instance UUID or handle resolved at grant time"}
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	if ttl > 7*24*time.Hour {
		return Deputy{}, &ErrDeputyScope{Why: fmt.Sprintf("deputy TTL %s exceeds 7 days", ttl)}
	}
	var out Deputy
	err := s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		// No deputies of deputies: reject before authorize for a clear signal.
		// Check against current active deputies (fenced/expired/revoked excluded).
		curEpoch := deriveAuthority(rep).Epoch
		depsPre := deriveDeputies(rep.Entries)
		for _, d := range depsPre {
			if !d.Active(now, curEpoch) {
				continue
			}
			matched := false
			if d.Holder.Episode != "" && actor.Episode != "" {
				matched = d.Holder.Episode == actor.Episode
			} else {
				matched = SameHolder(d.Holder, actor)
			}
			if matched {
				return &ErrDeputyIsDeputy{}
			}
		}
		auth, err := authorize(rep, actor, epoch)
		if err != nil {
			return err
		}
		deps := depsPre
		// Non-overlapping scope check against active deputies at current epoch.
		var conflicts []Deputy
		for _, d := range deps {
			if !d.Active(now, auth.Epoch) {
				continue
			}
			if d.Scope.Overlaps(scope) {
				conflicts = append(conflicts, d)
			}
		}
		if len(conflicts) > 0 {
			return &ErrDeputyOverlap{Scope: scope, Conflicts: conflicts}
		}
		id := deputyID(now)
		dep := Deputy{
			ID:           id,
			Holder:       holder,
			HolderHandle: holderHandle,
			OnBehalfOf:   auth.Holder,
			Scope:        scope,
			ScopeLabel:   scope.Label(),
			GrantedAt:    now,
			ExpiresAt:    now.Add(ttl),
			Epoch:        auth.Epoch,
		}
		// Sort sprints for stable label
		if len(dep.Scope.Sprints) > 0 {
			sorted := append([]int(nil), dep.Scope.Sprints...)
			// simple insertion sort
			for i := 1; i < len(sorted); i++ {
				for j := i; j > 0 && sorted[j] < sorted[j-1]; j-- {
					sorted[j], sorted[j-1] = sorted[j-1], sorted[j]
				}
			}
			dep.Scope.Sprints = sorted
			dep.ScopeLabel = dep.Scope.Label()
		}
		rec := deputyRecord{Deputy: dep}
		b, _ := json.Marshal(rec)
		e := Entry{
			Actor:   actor,
			Kind:    KindDeputyGranted,
			Summary: fmt.Sprintf("deputy %s granted to %s for scope %q until %s (epoch %d)", dep.ID, holderName(holder), dep.ScopeLabel, dep.ExpiresAt.Format(time.RFC3339), auth.Epoch),
			Outcome: OutcomeSuccess,
			Evidence: []Evidence{
				{Kind: "deputy", Ref: string(b), Note: "granted"},
				{Kind: "grant", Ref: dep.ID, Note: "deputy-id"},
			},
		}
		stored, err := s.appendAuthorized(rep, e, epoch, now)
		if err != nil {
			return err
		}
		_ = stored
		out = dep
		return nil
	})
	return out, err
}

// DeputyList returns all deputies known from the journal, with Revoked/Expired
// and epoch fencing reflected but not filtered — callers can check Active().
func (s *Store) DeputyList(now time.Time) ([]Deputy, error) {
	now = mustUTC(now)
	rep, err := s.Replay()
	if err != nil {
		return nil, err
	}
	m := deriveDeputies(rep.Entries)
	out := make([]Deputy, 0, len(m))
	for _, d := range m {
		out = append(out, d)
	}
	// sort by granted time
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].GrantedAt.Before(out[j-1].GrantedAt); j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out, nil
}

// DeputyRevoke marks a deputy as revoked. Only the steward (current holder) may revoke.
func (s *Store) DeputyRevoke(actor principal.Ref, epoch uint64, id string, now time.Time) error {
	now = mustUTC(now)
	if strings.TrimSpace(id) == "" {
		return &ErrDeputyNotFound{ID: id}
	}
	return s.withLock(func() error {
		rep, err := s.Replay()
		if err != nil {
			return err
		}
		if _, err := authorize(rep, actor, epoch); err != nil {
			return err
		}
		m := deriveDeputies(rep.Entries)
		if _, ok := m[id]; !ok {
			return &ErrDeputyNotFound{ID: id}
		}
		if m[id].Revoked {
			return &ErrDeputyRevoked{ID: id}
		}
		e := Entry{
			Actor:   actor,
			Kind:    KindDeputyRevoked,
			Summary: fmt.Sprintf("deputy %q revoked", id),
			Outcome: OutcomeSuccess,
			Evidence: []Evidence{
				{Kind: "deputy", Ref: id, Note: "revoke"},
			},
		}
		_, err = s.appendAuthorized(rep, e, epoch, now)
		return err
	})
}

// DeputyActive returns the active deputy set (not revoked, not expired, epoch matches current).
func (s *Store) DeputyActive(now time.Time) ([]Deputy, error) {
	now = mustUTC(now)
	rep, err := s.Replay()
	if err != nil {
		return nil, err
	}
	auth := deriveAuthority(rep)
	m := deriveDeputies(rep.Entries)
	var out []Deputy
	for _, d := range m {
		if d.Active(now, auth.Epoch) {
			out = append(out, d)
		}
	}
	return out, nil
}

// CheckDeputyAuthority validates that holder (instance UUID) may perform action
// within the given sprint/epic scope. It enforces: active, epoch fenced,
// not revoked/expired, holder snapshot matches resolved UUID (reuse never transfers),
// scope containment, allowed actions, and independence (no self-conduct).
func (s *Store) CheckDeputyAuthority(holder principal.Ref, action string, sprint int, epic string, now time.Time) error {
	now = mustUTC(now)
	rep, err := s.Replay()
	if err != nil {
		return err
	}
	auth := deriveAuthority(rep)
	m := deriveDeputies(rep.Entries)
	// Find deputy that claims this holder instance
	var dep *Deputy
	for _, d := range m {
		// match by instance UUID strictly: holder.Episode must equal deputy.Holder.Episode
		// If holder has no Episode, fall back to SameHolder name+host
		matched := false
		if d.Holder.Episode != "" && holder.Episode != "" {
			matched = d.Holder.Episode == holder.Episode
		} else {
			matched = SameHolder(d.Holder, holder)
		}
		if matched {
			if !d.Active(now, auth.Epoch) {
				// fencing/expiry/revoke takes precedence over scope
				if d.Revoked {
					return &ErrDeputyRevoked{ID: d.ID}
				}
				if d.Expired(now) {
					return &ErrDeputyExpired{ID: d.ID}
				}
				if d.Epoch != auth.Epoch {
					return &ErrDeputyFenced{ID: d.ID, Presented: d.Epoch, Current: auth.Epoch}
				}
			}
			cp := d
			dep = &cp
			break
		}
	}
	if dep == nil {
		return &ErrDeputyScope{Why: fmt.Sprintf("no active deputy for holder %q", holderName(holder))}
	}
	// Independence: deputy cannot conduct a sprint in its own scope
	// Conduct is a conductor act, not a deputy act — it is forbidden inside supervised scope and allowed outside.
	if action == DeputyActionConduct {
		if dep.Scope.Contains(sprint, epic) {
			return &ErrDeputySelfConduct{ID: dep.ID, Sprint: sprint}
		}
		return nil
	}
	// Allowed deputy actions: only activate, fence, judge, gate within scope
	if !allowedDeputyActions[action] {
		return &ErrDeputyOutOfScope{ID: dep.ID, Action: action, Scope: dep.Scope}
	}
	// Scope containment for the four allowed acts
	if sprint > 0 || strings.TrimSpace(epic) != "" {
		if !dep.Scope.Contains(sprint, epic) {
			return &ErrDeputyOutOfScope{ID: dep.ID, Action: action, Scope: dep.Scope}
		}
	} else {
		// actions without explicit sprint/epic are cross-scope -> steward only
		return &ErrDeputyOutOfScope{ID: dep.ID, Action: action, Scope: dep.Scope}
	}
	return nil
}

// DeputyLabelForScope returns the durable topic/label for deputy:<scope>.
func DeputyLabelForScope(scope DeputyScope) string {
	lbl := scope.Label()
	if lbl == "" {
		return "deputy"
	}
	return "deputy:" + lbl
}

// DeputyTopicForScope returns the bus topic for deputy:<scope>.
func DeputyTopicForScope(scope DeputyScope) string {
	lbl := scope.Label()
	if lbl == "" {
		return "deputy"
	}
	return "deputy." + lbl
}
