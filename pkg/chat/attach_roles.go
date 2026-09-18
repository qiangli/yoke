package chat

import (
	"fmt"
	"os"
	"strings"
	"sync"

	"github.com/qiangli/yoke/pkg/room"
)

// ATTACH ROLES — joining an in-flight session as a PARTICIPANT, where the role IS
// an enforced effect cap.
//
// `coach attach` already answered "can I watch a session that is already
// running". The question left open is "with how much power". An operator who
// only wants to see what an agent is doing should not be one keystroke away from
// breaking its turn; an operator who wants to suggest a direction should not have
// to accept the authority to stop it. Roles make that a property of the
// ATTACHMENT rather than of the operator's restraint.
//
//	role         observe   say   interrupt (ESC)
//	observer     yes       no    no
//	advisor      yes       yes   no
//	supervisor   yes       yes   yes
//
// WHY ESC IS THE LINE BETWEEN advisor AND supervisor: ESC is the raw escape byte
// (0x1b) sent as an agentpty VerbatimFrame, and it is the ONLY thing that breaks
// into a turn already running. A Say is queued and read at a turn boundary, which
// an agent stuck in a tool loop never reaches. So "may suggest" and "may stop"
// are genuinely different powers, not a cosmetic distinction — an advisor can
// change where an agent goes next, only a supervisor can change what it is doing
// now.
//
// ENFORCEMENT IS BY CAPABILITY, NOT BY CONVENTION. The role is not a label the
// coach is asked to respect: it selects the Steerer, and the Steerer is the only
// surface an attachment can emit through. An advisor that could reach Interrupt
// would BE a supervisor, so it is not given one. (P1 scope: these three
// non-authoring roles only. `judge` and `pair` author files and need an isolation
// story that does not exist yet — they are deliberately not built here.)
//
// DEMOTE, NEVER DROP. When the LLM-free reflex policy trips under a capped role,
// the trip is still detected, still recorded in the report, and still logged —
// only the effect is reduced to what the role permits. That mirrors how the
// notification bus demotes an unauthorized interrupt to a queued notification
// instead of honouring or discarding it: a dropped signal leaves an operator on
// stale assumptions, and a silently-honoured one defeats the cap.

// AttachRole is the participation role an attachment joins a running session
// with. Its zero value is deliberately NOT a valid role — a role must be chosen
// (or explicitly defaulted by a command), never inferred.
type AttachRole string

const (
	// RoleObserver watches and reports. It cannot emit anything at all.
	RoleObserver AttachRole = "observer"
	// RoleAdvisor watches and may say a sentence, read at the coachee's next turn
	// boundary. It cannot break into a turn already running.
	RoleAdvisor AttachRole = "advisor"
	// RoleSupervisor watches, may say, and may press ESC — today's `coach attach`
	// behavior, unchanged.
	RoleSupervisor AttachRole = "supervisor"
)

// AttachRoles lists the supported roles, LEAST powerful first. The order is the
// point: it is what a help text and an error message should show, so an operator
// reads the ladder rather than a set.
func AttachRoles() []AttachRole { return []AttachRole{RoleObserver, RoleAdvisor, RoleSupervisor} }

// AttachRoleNames is AttachRoles as plain strings, for flag help and errors.
func AttachRoleNames() []string {
	out := make([]string, 0, len(AttachRoles()))
	for _, r := range AttachRoles() {
		out = append(out, string(r))
	}
	return out
}

// ParseAttachRole resolves an operator-supplied --as value. An unknown value is
// REFUSED, never defaulted: silently falling back to a powerful role because a
// word was misspelled is exactly the failure roles exist to prevent, and silently
// falling back to a weak one would hide a typo behind an attachment that quietly
// does nothing.
func ParseAttachRole(s string) (AttachRole, error) {
	switch AttachRole(strings.ToLower(strings.TrimSpace(s))) {
	case RoleObserver:
		return RoleObserver, nil
	case RoleAdvisor:
		return RoleAdvisor, nil
	case RoleSupervisor:
		return RoleSupervisor, nil
	case "judge", "pair":
		// Named in the design, deliberately not built: both author, and authoring
		// from an attachment needs an isolation story that does not exist yet. Say
		// so rather than letting them read as typos.
		return "", fmt.Errorf("attach: role %q is not implemented — it authors, and attaching an author needs an isolation story that does not exist yet; valid roles are %s (least to most power)",
			strings.TrimSpace(s), strings.Join(AttachRoleNames(), ", "))
	}
	return "", fmt.Errorf("attach: unknown role %q — valid roles are %s (least to most power)",
		strings.TrimSpace(s), strings.Join(AttachRoleNames(), ", "))
}

// CanObserve is true for every role: watching is what attaching IS. It exists so
// the capability table reads as a table in code, not as two methods and an
// implicit third.
func (r AttachRole) CanObserve() bool {
	switch r {
	case RoleObserver, RoleAdvisor, RoleSupervisor:
		return true
	}
	return false
}

// CanSay reports whether the role may inject a sentence (read at a turn boundary).
func (r AttachRole) CanSay() bool { return r == RoleAdvisor || r == RoleSupervisor }

// CanInterrupt reports whether the role may press ESC (break into a running turn).
func (r AttachRole) CanInterrupt() bool { return r == RoleSupervisor }

// String makes AttachRole print as its wire spelling in messages and audit bodies.
func (r AttachRole) String() string { return string(r) }

// roleSteerer is the enforcement point: every effect an attachment can have on a
// coachee passes through here, and what the role does not permit does not reach
// the socket. It wraps the real ctlSteerer rather than replacing it, so the
// permitted effects are byte-identical to a launched coach's.
//
// Demotions are COUNTED, not discarded — an attachment that was prevented from
// intervening is a fact about the session, and a caller (the CLI summary, a test)
// can report it.
type roleSteerer struct {
	role  AttachRole
	inner Steerer

	mu        sync.Mutex
	demotions int
}

// newRoleSteerer builds the capped steerer for a role. Note the belt AND braces
// for observer: it is capped by role AND handed the existing no-op steerer
// (NewCtlSteerer("") sends nothing), so an observer has no reachable socket at
// all even if the cap above it were bypassed.
func newRoleSteerer(role AttachRole, ctlSock string) *roleSteerer {
	sock := ctlSock
	if !role.CanSay() && !role.CanInterrupt() {
		sock = ""
	}
	return &roleSteerer{role: role, inner: NewCtlSteerer(sock)}
}

// Interrupt sends ESC only for a role that may stop a turn. For any other role it
// is a recorded no-op: the trip that produced it is already in the coach's report,
// so the intervention is demoted to "detected and reported", never dropped.
func (s *roleSteerer) Interrupt() error {
	if !s.role.CanInterrupt() {
		s.demote()
		return nil
	}
	return s.inner.Interrupt()
}

// Say injects a sentence only for a role that may speak. An observer's steer is
// demoted the same way an advisor's ESC is.
func (s *roleSteerer) Say(text string) error {
	if !s.role.CanSay() {
		s.demote()
		return nil
	}
	return s.inner.Say(text)
}

func (s *roleSteerer) demote() {
	s.mu.Lock()
	s.demotions++
	s.mu.Unlock()
}

// Demotions is how many effects the role cap turned into report-only records.
func (s *roleSteerer) Demotions() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.demotions
}

var _ Steerer = (*roleSteerer)(nil)

// attachActor attributes the attachment. A participant nobody can attribute is
// not a participant — the same rule bus.Publish enforces with its required
// Principal — but an unattributable attach still gets recorded as "unknown"
// rather than not recorded at all: demote, never drop. (USERNAME is the Windows
// spelling principalName does not cover.)
func attachActor() string {
	if p := principalName(); p != "" {
		return p
	}
	if v := strings.TrimSpace(os.Getenv("USERNAME")); v != "" {
		return v
	}
	return "unknown"
}

// Audit event kinds, carried in the note body (the room's Event shape is fixed
// and not widened by this feature — these ride room.EventNote).
const (
	attachAuditAttach = "attach"
	attachAuditDetach = "detach"
)

// emitAttachAudit records a participation boundary on the room timeline: WHO
// joined or left, in WHAT role, over WHICH session. Both ends are emitted because
// only the pair bounds the window in which an attachment could have had an
// effect — an attach with no detach reads as "still watching", which is exactly
// what an operator scanning the timeline needs to know.
//
// Best-effort by design: a timeline that cannot be written must not prevent an
// operator from supervising a session that is going wrong.
func emitAttachAudit(kind string, role AttachRole, card room.Card, actor string) {
	_ = room.Emit(room.Event{
		Type:      room.EventNote,
		Actor:     actor,
		Target:    card.ID,
		Principal: actor,
		Body: fmt.Sprintf("%s role=%s actor=%s target=%s binding=%s",
			kind, role, actor, card.ID, card.Binding),
	})
}
