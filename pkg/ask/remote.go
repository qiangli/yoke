package ask

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// Remote approval — a decision made on ANOTHER host and applied on the host
// that owns the queue.
//
// The approval never leaves its owner. A human on host B answers an approval
// raised on host A by sending a RemoteDirective over whatever transport the
// caller already has (pkg/weave rides the repo session's task feed); A checks
// it here against its own pending record and settles it with Decide, so the
// one-writer CAS, the room audit and every resume rule are exactly the local
// ones. This file is transport-free on purpose: it is the whole policy, and a
// second ingress must not be able to bypass it by re-implementing it.
//
// Everything fails closed. An unknown or prefix-only id, a missing or
// different token, an approver that is not an allowed person, an expired or
// already-decided approval, and a replay of a directive that was already
// applied all come back as a rejected receipt — never as an allow.

// ViaRemote marks a decision that arrived from another host.
const ViaRemote = "remote"

// Receipt statuses.
const (
	RemoteApplied  = "applied"
	RemoteRejected = "rejected"
)

// RemoteDirective is one cross-host answer to an approval.
type RemoteDirective struct {
	// ID names the directive itself (a fresh UUID per answer), so a receipt can
	// be matched to it and a redelivery recognised.
	ID string `json:"id"`
	// Approval is the FULL request id. A remote answer may not use the prefix
	// matching the local CLI allows: a prefix is a convenience for a human at a
	// terminal, and across hosts it would let an answer land on a record its
	// sender never saw.
	Approval string `json:"approval"`
	// Host is the owner host the directive is for; other hosts ignore it.
	Host string `json:"host"`
	// Token is the plan hash the approver saw. It is required and must match.
	Token   string `json:"token"`
	Verdict string `json:"verdict"`
	Always  bool   `json:"always,omitempty"`
	Reason  string `json:"reason,omitempty"`
	// By is the approver's seat as the roster knows it (person:<name>@<host>);
	// ByURN is its canonical principal when the sender has one.
	By      string `json:"by"`
	ByURN   string `json:"by_urn,omitempty"`
	At      string `json:"at,omitempty"`
	TraceID string `json:"trace_id,omitempty"`
}

// RemoteReceipt is the owner's explicit answer to a directive.
type RemoteReceipt struct {
	Directive string    `json:"directive"`
	Approval  string    `json:"approval"`
	Host      string    `json:"host"`
	Token     string    `json:"token,omitempty"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Decision  *Decision `json:"decision,omitempty"`
	TraceID   string    `json:"trace_id,omitempty"`
	At        time.Time `json:"at"`
}

// Applied reports whether the directive settled the approval.
func (r RemoteReceipt) Applied() bool { return r.Status == RemoteApplied }

// RemotePolicy says who may answer this host's approvals from elsewhere.
type RemotePolicy struct {
	// Approvers are person names (`qiangli`) or full seats
	// (`person:qiangli@laptop`). Empty admits nobody: remote approval is
	// opt-in by naming the human, never by default.
	Approvers []string
}

// ErrRemoteRejected wraps every refusal, so a caller can tell a policy
// rejection (answered with a receipt) from an I/O failure (retry).
var ErrRemoteRejected = errors.New("ask: remote approval rejected")

var requestIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// ApplyRemote validates a directive against this host's pending approval and,
// when every check passes, settles it once with Decide. The receipt is always
// returned; err is non-nil exactly when the receipt is a rejection.
func ApplyRemote(d RemoteDirective, pol RemotePolicy) (RemoteReceipt, error) {
	rc := RemoteReceipt{
		Directive: d.ID, Approval: d.Approval, Host: d.Host, Token: d.Token,
		TraceID: d.TraceID, At: time.Now().UTC(),
	}
	r, dec, err := applyRemote(d, pol)
	if err != nil {
		rc.Status, rc.Reason = RemoteRejected, err.Error()
		room.Record(room.EventApproval, firstNonEmpty(d.By, "unknown"), "approval:"+shortID(d.Approval), RemoteRejected,
			fmt.Sprintf("remote directive %s rejected: %s%s", d.ID, err, traceSuffix(d.TraceID)))
		return rc, fmt.Errorf("%w: %v", ErrRemoteRejected, err)
	}
	rc.Status, rc.Decision = RemoteApplied, &dec
	room.Record(room.EventApproval, dec.By, approvalTarget(r), "remote",
		fmt.Sprintf("remote directive %s applied: %s (token %s)%s", d.ID, dec.Verdict, r.Approval.Token, traceSuffix(d.TraceID)))
	return rc, nil
}

func applyRemote(d RemoteDirective, pol RemotePolicy) (Request, Decision, error) {
	if strings.TrimSpace(d.ID) == "" {
		return Request{}, Decision{}, errors.New("directive has no id")
	}
	if !requestIDPattern.MatchString(d.Approval) {
		return Request{}, Decision{}, fmt.Errorf("approval id %q is not a full request id", d.Approval)
	}
	if strings.TrimSpace(d.Token) == "" {
		return Request{}, Decision{}, errors.New("directive carries no plan token")
	}
	if d.Verdict != VerdictAllow && d.Verdict != VerdictDeny {
		return Request{}, Decision{}, fmt.Errorf("verdict must be allow or deny, got %q", d.Verdict)
	}
	if !pol.admits(d.By) {
		return Request{}, Decision{}, fmt.Errorf("approver %q is not an allowed person for remote approval", d.By)
	}
	r, err := Load(d.Approval)
	if err != nil {
		return Request{}, Decision{}, fmt.Errorf("no approval %s on this host", shortID(d.Approval))
	}
	if !r.IsApproval() || r.Approval == nil {
		return Request{}, Decision{}, fmt.Errorf("%s is not an approval", shortID(r.ID))
	}
	if !strings.EqualFold(d.Token, r.Approval.Token) {
		return Request{}, Decision{}, fmt.Errorf("token %s does not match approval %s — the plan changed", d.Token, shortID(r.ID))
	}
	if _, decided := LoadDecision(r.ID); decided {
		return Request{}, Decision{}, ErrDecided
	}
	if approvalExpired(r, time.Now()) {
		return Request{}, Decision{}, fmt.Errorf("approval %s has expired", shortID(r.ID))
	}
	by := strings.TrimSpace(d.By)
	if d.ByURN != "" {
		by = d.ByURN + " (" + by + ")"
	}
	if _, err := Decide(r.ID, Decision{Verdict: d.Verdict, Always: d.Always, By: by, Reason: d.Reason},
		DecideOpts{Token: d.Token, Via: ViaRemote}); err != nil {
		return Request{}, Decision{}, err
	}
	dec, ok := LoadDecision(r.ID)
	if !ok {
		return Request{}, Decision{}, errors.New("decision vanished after it was recorded")
	}
	return r, dec, nil
}

// admits reports whether seat names an allowed PERSON. An agent seat never
// qualifies: an approval is a human's yes, and an agent answering one from
// another host is exactly the self-approval the queue exists to prevent.
func (p RemotePolicy) admits(seat string) bool {
	seat = strings.TrimSpace(seat)
	rest, ok := strings.CutPrefix(seat, "person:")
	if !ok {
		return false
	}
	name, host, _ := strings.Cut(rest, "@")
	if name == "" || host == "" {
		return false
	}
	for _, a := range p.Approvers {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if strings.Contains(a, "@") || strings.HasPrefix(a, "person:") {
			if strings.EqualFold(strings.TrimPrefix(a, "person:"), rest) {
				return true
			}
			continue
		}
		if strings.EqualFold(a, name) {
			return true
		}
	}
	return false
}

func traceSuffix(id string) string {
	if id == "" {
		return ""
	}
	return " trace " + id
}
