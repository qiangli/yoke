// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package bus

import (
	"errors"
	"fmt"
	"strings"
)

// SendRequest is one authored post, with its target already chosen.
//
// It exists so the CLI and any other host (the web console's board panel) reach
// the board through ONE implementation. The alternative — a second code path
// that also appends and also steers — is how the two drift, and the drift is
// invisible: both surfaces report success while addressing different names or
// skipping the live push. See dhnt/docs/agent-interaction-surfaces-design.md §5,
// "a feature reaching one by a private path is a parity bug by construction".
type SendRequest struct {
	// From is the resolved sender. Callers resolve it with BoardIdentity;
	// PostMessageSeq refuses an empty one, because the board's one guarantee is
	// that a post names who sent it.
	From string

	// To is a single agent, role, reader or principal, resolved HERE at send
	// time. Empty, with an empty Audience, is a broadcast.
	To string

	// Audience is the group selector. One post carries it; it is resolved at
	// READ time, so the board does not grow with the size of the audience.
	Audience *Audience
	// Mode is ModeAll (every member sees it, views counted) or ModeAny (the
	// first reader claims it). Empty means ModeAll. Meaningful only with an
	// Audience.
	Mode string

	Topic string
	Body  string
}

// BodyError marks a body-admission refusal, so a caller can prefix it with its
// own verb ("mb send: ...") while every OTHER failure — an unresolvable
// addressee, say — keeps the wording that already tells the sender what to do.
type BodyError struct{ Err error }

func (e *BodyError) Error() string { return e.Err.Error() }
func (e *BodyError) Unwrap() error { return e.Err }

// verbError re-applies the command prefix the CLI has always printed. It exists
// because the send order is resolve-THEN-validate: pre-validating at the call
// site to get the prefix would either change which error a doubly-wrong send
// reports, or resolve the target twice — and resolution is the expensive half.
func verbError(verb string, err error) error {
	var be *BodyError
	if errors.As(err, &be) {
		return fmt.Errorf("%s: %w", verb, be.Err)
	}
	return err
}

// Send kinds, so a caller can render the confirmation its own way.
const (
	SendDirect    = "direct"
	SendAudience  = "audience"
	SendBroadcast = "broadcast"
)

// SendResult is what a caller needs to report a send truthfully: the assigned
// sequence, the name to echo back, and one Delivery per recipient carrying the
// provable Yoke state (accepted/queued/delivered/read/failed/unverified).
type SendResult struct {
	Seq  int64  `json:"seq"`
	Kind string `json:"kind"`

	// Label is the addressee as the SENDER should see it — the name they typed,
	// not the routing address. A confirmation reading
	// "steward.dragon-u501-b683b300b1" makes a successful send look like it went
	// somewhere unintended.
	Label string `json:"label"`

	Deliveries []Delivery `json:"deliveries,omitempty"`
}

// Send appends one post to the board and then pushes it to whatever live
// sessions it can reach.
//
// BOARD FIRST, STEER SECOND, always. The durable copy is the one that must not
// be optional: steering first would lose the message entirely if the append
// failed. An unresolvable target writes NOTHING and fails with choices — a post
// to a name nobody answers was a receipt indistinguishable from a real delivery.
//
// A body-admission refusal comes back as a *BodyError so each caller can prefix
// it with its own verb; use verbError.
func Send(req SendRequest) (SendResult, error) {
	body := req.Body

	if req.Audience != nil && !req.Audience.Empty() {
		if err := req.Audience.Validate(); err != nil {
			return SendResult{}, err
		}
		if err := ValidateCoordinationBody(body); err != nil {
			return SendResult{}, &BodyError{Err: err}
		}
		aud := *req.Audience
		mode := req.Mode
		if mode == "" {
			mode = ModeAll
		}
		// RESOLVE BEFORE THE APPEND, exactly as the req.To path below does and
		// as this function's own contract requires: "an unresolvable target
		// writes NOTHING and fails". The selector used to be resolved AFTER
		// the post was durably appended, with its error DISCARDED — so a
		// selector naming nothing resolvable (`--role reviewer`) posted to the
		// board anyway and reported success, which is the receipt
		// indistinguishable from a real delivery that this comment forbids.
		if FleetSelect == nil {
			return SendResult{}, fmt.Errorf("audience: selector resolver is unavailable for %s", aud.describe())
		}
		names, err := FleetSelect(aud)
		if err != nil {
			return SendResult{}, err
		}
		seq, err := PostMessageSeq(Post{
			From: req.From, Audience: &aud, Mode: mode, Topic: req.Topic, Body: body,
		})
		if err != nil {
			return SendResult{}, err
		}
		res := SendResult{Seq: seq, Kind: SendAudience, Label: aud.describe()}
		for _, n := range names {
			d := SteerLive(n, steerNotice(req.From, body))
			d.State = deliveryState(n, seq, d.Steered, true)
			res.Deliveries = append(res.Deliveries, d)
		}
		return res, nil
	}

	if target := strings.TrimSpace(req.To); target != "" {
		// Resolve BEFORE validating the body, so a typo'd addressee is reported
		// as a typo'd addressee rather than as whatever the body was.
		addr, kind, ok := ResolveSendTarget(target)
		if !ok {
			return SendResult{}, unresolvedTargetError(target)
		}
		if err := ValidateCoordinationBody(body); err != nil {
			return SendResult{}, &BodyError{Err: err}
		}
		seq, err := PostMessageSeq(Post{From: req.From, To: addr, Topic: req.Topic, Body: body})
		if err != nil {
			return SendResult{}, err
		}
		d := SteerLive(addr, steerNotice(req.From, body))
		// A ROLE is a seat, not a reader: its cursor is not one agent's, so the
		// per-reader states do not apply.
		d.State = deliveryState(addr, seq, d.Steered, kind != TargetRole)
		d.To = RoleLabelFor(d.To)
		return SendResult{Seq: seq, Kind: SendDirect, Label: d.To, Deliveries: []Delivery{d}}, nil
	}

	if err := ValidateCoordinationBody(body); err != nil {
		return SendResult{}, &BodyError{Err: err}
	}
	seq, err := PostMessageSeq(Post{From: req.From, Topic: req.Topic, Body: body})
	if err != nil {
		return SendResult{}, err
	}
	res := SendResult{Seq: seq, Kind: SendBroadcast, Label: "the board"}
	if FleetNames != nil {
		for _, n := range FleetNames() {
			d := SteerLive(n, steerNotice(req.From, body))
			d.State = deliveryState(n, seq, d.Steered, true)
			res.Deliveries = append(res.Deliveries, d)
		}
	}
	return res, nil
}
