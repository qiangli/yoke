package acp

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/ask"
)

// The REVERSE direction of approval.go. There, bashy is the ACP client and an
// agent's session/request_permission is raised INTO the approval queue. Here,
// bashy is the ACP agent (`bashy acp`) and an approval its local session raised
// in the queue is relayed OUT to the host as session/request_permission, so the
// human at Zed or a Codync bot answers it where they already are.
//
// The queue stays the one record. The host's answer is written with
// ask.Decide, so `bashy approve list|show` and the room timeline see it, and a
// `bashy approve allow|deny` that lands first simply wins (Decide is first
// writer wins) and withdraws the host's question.

// RelayPoll is how often RelayApprovals looks for newly raised approvals.
const RelayPoll = 200 * time.Millisecond

// Synthesized permission options for an approval that carries none (a yielded
// @confirm, `bashy approve request`). Their ids are what the host selects.
const (
	RelayOptionAllowOnce   = "allow-once"
	RelayOptionAllowAlways = "allow-always"
	RelayOptionRejectOnce  = "reject-once"
)

// PermissionAsker sends one permission request to the host. TurnRequest's
// RequestPermission is the production value.
type PermissionAsker func(context.Context, PermissionRequest) (PermissionResponse, error)

// RelayApprovals relays every pending approval raised for queue session to the
// host until ctx ends. Each approval is put to the host ONCE: an answered one
// is decided, and one the host cancelled stays pending for `bashy approve`
// rather than being asked again in a loop.
func RelayApprovals(ctx context.Context, session string, host PermissionAsker) {
	if host == nil || strings.TrimSpace(session) == "" {
		return
	}
	asked := map[string]bool{}
	t := time.NewTicker(RelayPoll)
	defer t.Stop()
	for {
		pending, _ := ask.PendingForSession(session)
		for _, r := range pending {
			if asked[r.ID] {
				continue
			}
			asked[r.ID] = true
			// One at a time: a host shows one permission dialog per turn, and
			// a second request racing the first would be answered blind.
			_, _ = RelayApproval(ctx, r, host)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// RelayApproval puts one pending approval to the host and records its answer
// on the queue. It returns the decision that stands — the host's, or one a
// faster decider recorded first — and ok=false when nothing was decided (the
// host cancelled or could not be reached; the approval stays pending).
func RelayApproval(ctx context.Context, r ask.Request, host PermissionAsker) (ask.Decision, bool) {
	if r.Approval == nil {
		return ask.Decision{}, false
	}
	req := relayRequest(r)

	// Withdraw the host's question when the approval is settled elsewhere
	// (`bashy approve`, `chat grant`, the requester's own timeout) or vanishes.
	hctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go func() {
		_, _ = ask.WaitDecision(hctx, r.ID)
		cancel()
	}()

	resp, err := host(hctx, req)
	if err != nil || resp.Cancelled || resp.OptionID == "" {
		d, ok := ask.LoadDecision(r.ID)
		return d, ok
	}
	d := relayDecision(req, resp)
	if _, err := ask.Decide(r.ID, d, ask.DecideOpts{Via: ask.ViaACP}); err != nil && !errors.Is(err, ask.ErrDecided) {
		return ask.Decision{}, false
	}
	return ask.LoadDecision(r.ID)
}

// relayRequest renders a queue approval as an ACP permission request.
func relayRequest(r ask.Request) PermissionRequest {
	a := r.Approval
	title := strings.TrimSpace(r.Prompt)
	if title == "" {
		title = strings.Join(a.Argv, " ")
	}
	kind := "other"
	if len(a.Argv) > 0 && strings.HasPrefix(a.Argv[0], "acp:") {
		kind = strings.TrimPrefix(a.Argv[0], "acp:")
	}
	req := PermissionRequest{
		ToolCall: ToolCall{ID: "approval:" + r.ID, Title: title, Kind: kind, Status: ToolCallPending},
	}
	for _, o := range a.Options {
		req.Options = append(req.Options, PermissionOption{ID: o.ID, Name: o.Name, Kind: PermissionOptionKind(o.Kind)})
	}
	if len(req.Options) == 0 {
		req.Options = []PermissionOption{
			{ID: RelayOptionAllowOnce, Name: "Allow once", Kind: PermissionAllowOnce},
			{ID: RelayOptionAllowAlways, Name: "Always allow", Kind: PermissionAllowAlways},
			{ID: RelayOptionRejectOnce, Name: "Reject", Kind: PermissionRejectOnce},
		}
	}
	return req
}

// relayDecision maps the host's selected option onto a queue decision. An
// option the request did not offer is not consent.
func relayDecision(req PermissionRequest, resp PermissionResponse) ask.Decision {
	d := ask.Decision{Verdict: ask.VerdictDeny, By: "acp-host", Reason: "host rejected the permission request"}
	for _, o := range req.Options {
		if o.ID != resp.OptionID {
			continue
		}
		if isAllow(o.Kind) {
			d = ask.Decision{Verdict: ask.VerdictAllow, Always: o.Kind == PermissionAllowAlways, By: "acp-host", OptionID: o.ID}
		}
		return d
	}
	d.Reason = "host selected an option that was not offered"
	return d
}
