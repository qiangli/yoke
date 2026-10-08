// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package bus

// ONE recipient resolver for every sender (mb send, ping, Meet DM and
// directed posts).
//
// A human types a NAME; a name is held, released and re-issued, so it is never
// what a queued message is addressed to. Resolution happens exactly once, at
// send time, BEFORE anything is appended, and what is stored is the identity it
// resolved to:
//
//	instance   its UUID address (instance/<uuid>) — a retired Esme-2 never
//	           receives, or hands its mail to, whoever holds the label next
//	role       the role address (conductor:22, steward) — the seat, so mail
//	           survives a holder handoff; the holder is decided at READ time
//	agent/...  the legacy roster, reader and principal ladder, unchanged
//
// A UUID is identity, not authentication and not a route: nothing here proves
// who is reading, and delivery still rides the existing board and relay.

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TargetInstance is a personal recipient resolved to one conversation's UUID.
const TargetInstance = "instance"

// Party is a fixed snapshot of an identity as it was when a post was written:
// the UUID that is the identity plus the display and family binding it ran as.
// It is additive provenance — a later label reuse or catalog edit cannot
// change what an old post says it was sent to or from.
type Party struct {
	UUID     string   `json:"uuid"`
	Label    string   `json:"label,omitempty"`
	FamilyID string   `json:"family_id,omitempty"`
	Family   string   `json:"family,omitempty"`
	Bindings []string `json:"bindings,omitempty"`
}

func partyOf(i fleet.Instance) *Party {
	return &Party{
		UUID: i.UUID, Label: i.Label, FamilyID: i.FamilyID, Family: i.Family,
		Bindings: append([]string(nil), i.Bindings...),
	}
}

// Recipient is a resolved send target.
type Recipient struct {
	// Addr is what Post.To stores.
	Addr string
	// Kind is a Target* constant.
	Kind string
	// Label is the addressee as the sender should see it.
	Label string
	// Party snapshots an instance recipient; nil for every other kind.
	Party *Party
	// Warning is a caveat known at resolve time, e.g. a vacant seat.
	Warning string
}

// Reasons a recipient fails to resolve. All are definitive except
// ReasonUnresolved, which a caller may still try on the cross-host relay.
const (
	ReasonUnresolved = "unresolved"
	ReasonAmbiguous  = "ambiguous"
	ReasonRetired    = "retired"
	ReasonRole       = "role"
)

// RecipientError is a refusal that wrote nothing.
type RecipientError struct {
	Reason string
	Target string
	msg    string
}

func (e *RecipientError) Error() string { return e.msg }

// Refusal reports whether err is a RecipientError that no other route can fix.
func Refusal(err error) bool {
	var re *RecipientError
	return errors.As(err, &re) && re.Reason != ReasonUnresolved
}

// InstanceStoreFn opens the instance store; a var so tests can pin it.
var InstanceStoreFn = func() *fleet.InstanceStore { return fleet.NewInstanceStore("") }

// ExplicitInstanceID extracts a UUID from the spellings that can only mean an
// instance: a bare UUID, instance/<uuid>, instance:<uuid> or dhnt:agent/<uuid>.
func ExplicitInstanceID(s string) (string, bool) {
	s = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(s), "@"))
	for _, p := range []string{fleet.InstanceAddressPrefix, "instance:", "dhnt:agent/"} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			s = rest
			break
		}
	}
	id, err := fleet.ParseInstanceUUID(s)
	return id, err == nil
}

// SenderParty snapshots the sender when it is an instance; nil otherwise.
func SenderParty(from string) *Party {
	id, ok := ExplicitInstanceID(from)
	if !ok {
		return nil
	}
	if inst, err := InstanceStoreFn().Get(id); err == nil {
		return partyOf(inst)
	}
	return nil
}

// ResolveRecipient resolves what a sender typed to the identity a post is
// addressed to. It never writes. Errors are *RecipientError with choices.
func ResolveRecipient(target string) (Recipient, error) {
	t := strings.TrimSpace(target)
	if t == "" {
		return Recipient{}, &RecipientError{Reason: ReasonUnresolved, msg: "failed: no recipient named — nothing was posted"}
	}
	if id, ok := ExplicitInstanceID(t); ok {
		return recipientForUUID(t, id)
	}
	if rr, ok := resolveRoleRecipient(t); ok {
		return rr, nil
	}
	store := InstanceStoreFn()
	all, lerr := store.List()
	if lerr != nil {
		return Recipient{}, fmt.Errorf("failed: instance store unreadable (%v) — nothing was posted", lerr)
	}
	if inst, found, err := resolveInstanceName(t, all); err != nil {
		return Recipient{}, err
	} else if found {
		return instanceRecipient(inst), nil
	}
	if name, isAgent := resolveAgentName(t); isAgent {
		return Recipient{Addr: name, Kind: TargetAgent, Label: name}, nil
	}
	if _, has := CursorSeq(t); has {
		return Recipient{Addr: t, Kind: TargetReader, Label: t}, nil
	}
	if addr, kind, ok := resolvePrincipalTarget(t); ok {
		return Recipient{Addr: addr, Kind: kind, Label: addr}, nil
	}
	if err := roleReferenceError(t); err != nil {
		return Recipient{}, err
	}
	return Recipient{}, unresolvedTargetError(t)
}

func instanceRecipient(i fleet.Instance) Recipient {
	return Recipient{Addr: i.MailAddress(), Kind: TargetInstance, Label: instanceLabel(i), Party: partyOf(i)}
}

func instanceLabel(i fleet.Instance) string {
	if l := strings.TrimSpace(i.Label); l != "" {
		return l
	}
	return i.UUID
}

func recipientForUUID(typed, id string) (Recipient, error) {
	inst, err := InstanceStoreFn().Get(id)
	if err != nil {
		if errors.Is(err, fleet.ErrInstanceUnknown) {
			return Recipient{}, &RecipientError{Reason: ReasonUnresolved, Target: typed, msg: fmt.Sprintf(
				"failed: %q names no instance record on this host — nothing was posted\n  list live instances: bashy instance list", typed)}
		}
		return Recipient{}, err
	}
	if !inst.Active() {
		return Recipient{}, retiredError(typed, inst, nil)
	}
	return instanceRecipient(inst), nil
}

// resolveInstanceName places a human name among the instance records.
//
// Order is the safety argument: a LIVE label/handle wins; otherwise a name that
// only RETIRED instances ever held fails as retired (never falling through to
// the legacy roster or a stale reader cursor, which would resurrect the label);
// otherwise a family name selects its instance only when exactly one is live.
func resolveInstanceName(name string, all []fleet.Instance) (fleet.Instance, bool, error) {
	want := strings.ToLower(strings.TrimSpace(name))
	var live, gone []fleet.Instance
	for _, i := range all {
		if strings.ToLower(strings.TrimSpace(i.Label)) != want && strings.ToLower(strings.TrimSpace(i.Handle)) != want {
			continue
		}
		if i.Active() {
			live = append(live, i)
		} else {
			gone = append(gone, i)
		}
	}
	if len(live) == 1 {
		return live[0], true, nil
	}
	if len(live) > 1 {
		return fleet.Instance{}, false, ambiguousError(name, live)
	}
	if len(gone) > 0 {
		sort.SliceStable(gone, func(a, b int) bool { return gone[a].Retired > gone[b].Retired })
		return fleet.Instance{}, false, retiredError(name, gone[0], all)
	}
	canon := strings.ToLower(resolveBoardName(name))
	var fam []fleet.Instance
	for _, i := range all {
		if !i.Active() {
			continue
		}
		f := strings.ToLower(strings.TrimSpace(i.Family))
		if (f != "" && (f == want || f == canon)) || strings.EqualFold(i.FamilyID, name) {
			fam = append(fam, i)
		}
	}
	switch len(fam) {
	case 0:
		return fleet.Instance{}, false, nil
	case 1:
		return fam[0], true, nil
	}
	return fleet.Instance{}, false, ambiguousError(name, fam)
}

func listInstances(is []fleet.Instance) string {
	var b strings.Builder
	for _, i := range is {
		fmt.Fprintf(&b, "\n    %s  %s  (%s)", i.UUID, instanceLabel(i), i.Family)
	}
	return b.String()
}

func ambiguousError(name string, is []fleet.Instance) error {
	return &RecipientError{Reason: ReasonAmbiguous, Target: name, msg: fmt.Sprintf(
		"failed: %q matches %d live instances — a family or shared name is not a personal mailbox, and no instance was guessed; nothing was posted%s\n"+
			"  address one by label or UUID: bashy mb send <uuid> \"...\"\n"+
			"  or reach the whole family as a selector: bashy mb send --family <name> \"...\"",
		strings.TrimSpace(name), len(is), listInstances(is))}
}

func retiredError(typed string, gone fleet.Instance, all []fleet.Instance) error {
	msg := fmt.Sprintf("failed: %q is the retired instance %s (%s, retired %s) — retired recipients receive no new mail; nothing was posted",
		strings.TrimSpace(typed), gone.UUID, instanceLabel(gone), gone.Retired)
	if gone.MailArchive != "" {
		msg += "\n  its archived mail: " + gone.MailArchive
	}
	var live []fleet.Instance
	for _, i := range all {
		if i.Active() && i.FamilyID == gone.FamilyID {
			live = append(live, i)
		}
	}
	if len(live) > 0 {
		msg += "\n  live instances of the same family (address one explicitly; the label was not redirected):" + listInstances(live)
	}
	return &RecipientError{Reason: ReasonRetired, Target: typed, msg: msg}
}

var roleLike = regexp.MustCompile(`^([a-z][a-z-]*):[A-Za-z0-9._-]+$`)

// roleReferenceError rejects an invalid role address by name. A role address
// (conductor:316) is a seat reference, never an agent name or a tool:model
// binding, so one that names no existing seat fails rather than posting.
func roleReferenceError(t string) error {
	m := roleLike.FindStringSubmatch(strings.ToLower(t))
	if m == nil {
		return nil
	}
	var seats []string
	known := map[string]bool{"conductor": true, "deputy": true, "steward": true}
	if HostRoles != nil {
		for _, r := range HostRoles() {
			if p, _, ok := strings.Cut(strings.ToLower(r.Label), ":"); ok {
				known[p] = true
				if p == m[1] {
					seats = append(seats, r.Label)
				}
			}
		}
	}
	if !known[m[1]] {
		return nil
	}
	msg := fmt.Sprintf("failed: %q is not an existing %s role address on this host — nothing was posted", t, m[1])
	if len(seats) > 0 {
		msg += "\n  existing: " + strings.Join(seats, ", ")
	}
	return &RecipientError{Reason: ReasonRole, Target: t, msg: msg}
}
