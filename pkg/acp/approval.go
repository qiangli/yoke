package acp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
)

// Permission modes, spelled as the fleet agent record spells them (fleet
// PermissionAuto and friends). They are repeated rather than imported so this
// package keeps its small dependency surface.
const (
	PermissionModeAuto   = "auto"
	PermissionModeAsk    = "ask"
	PermissionModeReview = "review"
	PermissionModeDeny   = "deny"
)

// DefaultApprovalWait bounds how long an ACP agent is left blocked on a
// permission request. A turn cannot wait forever for a human who is not there;
// on expiry the request is DENIED, recorded as a timeout.
const DefaultApprovalWait = 10 * time.Minute

// ApprovalHandler answers ACP permission requests according to the agent's
// permission mode, through the one approval queue (pkg/ask).
//
//   - auto (or empty): BaseHandler's behaviour, unchanged — select "allow once".
//     This is the default and raises nothing, so existing agents are not slowed
//     or prompted.
//   - deny:   reject, without asking anyone.
//   - ask:    raise a persisted approval and block for a human's decision.
//   - review: let Reviewer (an LLM through the model door) decide within
//     ReviewTimeout; anything it cannot decide falls back to ask.
//
// An unanswered ask is a denial, never an allow.
type ApprovalHandler struct {
	// Inner receives session updates. Nil ignores them.
	Inner Handler
	// Mode is the agent's permission mode; "" means auto.
	Mode string
	// Session names the live session in the room, so `chat grant` settles the
	// same record. It may be a function because the room card is created after
	// the client; nil or an empty result leaves the approval unattached.
	Session func() string
	// Sprint is recorded on each approval.
	Sprint string
	// Reviewer decides in review mode; nil makes review behave as ask.
	Reviewer ask.ReviewFunc
	// ReviewTimeout bounds Reviewer (ask.DefaultReviewTimeout when zero).
	ReviewTimeout time.Duration
	// Wait bounds the human wait (DefaultApprovalWait when zero).
	Wait time.Duration
}

var _ Handler = (*ApprovalHandler)(nil)

// SessionUpdate delegates to Inner.
func (h *ApprovalHandler) SessionUpdate(ctx context.Context, n SessionNotification) error {
	if h.Inner == nil {
		return nil
	}
	return h.Inner.SessionUpdate(ctx, n)
}

// RequestPermission applies the permission mode.
func (h *ApprovalHandler) RequestPermission(ctx context.Context, req PermissionRequest) (PermissionResponse, error) {
	switch strings.ToLower(strings.TrimSpace(h.Mode)) {
	case "", PermissionModeAuto:
		return BaseHandler{}.RequestPermission(ctx, req)
	case PermissionModeDeny:
		return rejection(req, false), nil
	case PermissionModeAsk, PermissionModeReview:
	default:
		// An unrecognised mode is not a licence to allow.
		return rejection(req, false), nil
	}

	r, err := ask.Raise(h.spec(req))
	if err != nil {
		// No queue means no human can be reached: deny rather than guess.
		return rejection(req, false), nil
	}

	if strings.EqualFold(strings.TrimSpace(h.Mode), PermissionModeReview) {
		if d, ok := ask.Review(ctx, r, h.Reviewer, h.ReviewTimeout); ok {
			return respond(req, d), nil
		}
	}

	wait := h.Wait
	if wait <= 0 {
		wait = DefaultApprovalWait
	}
	wctx, cancel := context.WithTimeout(ctx, wait)
	defer cancel()
	d, err := ask.WaitDecision(wctx, r.ID)
	switch {
	case err == nil:
		return respond(req, d), nil
	case errors.Is(err, context.DeadlineExceeded):
		return respond(req, ask.DenyUnanswered(r.ID, "no human answered the permission request")), nil
	case errors.Is(err, context.Canceled) || ctx.Err() != nil:
		return PermissionResponse{Cancelled: true}, nil
	default:
		// The record vanished (cancelled with `ask cancel`, or reaped): a withdrawn
		// question is not consent.
		return rejection(req, false), nil
	}
}

func (h *ApprovalHandler) spec(req PermissionRequest) ask.ApprovalSpec {
	argv := []string{"acp:" + orDefault(req.ToolCall.Kind, "tool"), req.ToolCall.Title}
	for _, l := range req.ToolCall.Locations {
		argv = append(argv, l.Path)
	}
	opts := make([]ask.ApprovalOption, 0, len(req.Options))
	for _, o := range req.Options {
		opts = append(opts, ask.ApprovalOption{ID: o.ID, Name: o.Name, Kind: string(o.Kind)})
	}
	session := req.SessionID
	if h.Session != nil {
		if s := h.Session(); s != "" {
			session = s
		}
	}
	return ask.ApprovalSpec{
		Source:  ask.SourceACP,
		Argv:    argv,
		Sprint:  h.Sprint,
		Session: session,
		Options: opts,
		Mode:    strings.ToLower(strings.TrimSpace(h.Mode)),
		Prompt:  "ACP permission request: " + req.ToolCall.Title,
	}
}

// respond maps a queue decision onto the option the agent offered.
func respond(req PermissionRequest, d ask.Decision) PermissionResponse {
	if !d.Allowed() {
		return rejection(req, false)
	}
	if d.OptionID != "" {
		for _, o := range req.Options {
			if o.ID == d.OptionID && isAllow(o.Kind) {
				return PermissionResponse{OptionID: o.ID}
			}
		}
	}
	want, other := PermissionAllowOnce, PermissionAllowAlways
	if d.Always {
		want, other = other, want
	}
	for _, kind := range []PermissionOptionKind{want, other} {
		for _, o := range req.Options {
			if o.Kind == kind {
				return PermissionResponse{OptionID: o.ID}
			}
		}
	}
	// The agent offered no allow option to select: the turn cannot proceed.
	return PermissionResponse{Cancelled: true}
}

// rejection selects a reject option, or cancels when the agent offered none.
func rejection(req PermissionRequest, always bool) PermissionResponse {
	want, other := PermissionRejectOnce, PermissionRejectAlways
	if always {
		want, other = other, want
	}
	for _, kind := range []PermissionOptionKind{want, other} {
		for _, o := range req.Options {
			if o.Kind == kind {
				return PermissionResponse{OptionID: o.ID}
			}
		}
	}
	return PermissionResponse{Cancelled: true}
}

func isAllow(k PermissionOptionKind) bool {
	return k == PermissionAllowOnce || k == PermissionAllowAlways
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}
