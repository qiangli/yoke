package mcp

import (
	"fmt"
	"strings"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/qiangli/yoke/pkg/ask"
)

// NeedsApprovalState is the sync answer when a call's effects need a
// digest-bound approval. It mirrors the pull route's needs-approval state
// and carries the same approval event shape.
const NeedsApprovalState = "needs-approval"

// GatedCall describes one invocation whose effects need an approval.
type GatedCall struct {
	Name    string
	Argv    []string
	Effects []string
}

// GatedGrant is the pending approval the caller must settle through the
// gateway approve tool (ask.ApplyRemote on the owning host, failing closed
// on a stale digest).
type GatedGrant struct {
	ApprovalID string
	Digest     string
	Summary    string
	Effects    []string
}

// NeedsApproval is the structured sync answer for a gated call: the state
// plus the approval event shape {approval_id, digest, summary, effects}.
type NeedsApproval struct {
	State      string   `json:"state"`
	ApprovalID string   `json:"approval_id"`
	Digest     string   `json:"digest"`
	Summary    string   `json:"summary"`
	Effects    []string `json:"effects"`
}

// GatewayApprovalHook builds a Policy.RaiseApproval hook that raises a
// digest-bound ask approval for each gated candidate, the same ask.Raise
// path the pull route takes. host and taskID bind the digest (see
// ask.GatewayDigest): pass the claimed gateway task id when the sync
// handler has one, so the approve directive settles the same candidate on
// either route. A raise failure declines, so the caller fails closed to
// the outright denial.
func GatewayApprovalHook(host, taskID string) func(GatedCall) (GatedGrant, bool) {
	return func(call GatedCall) (GatedGrant, bool) {
		digest := ask.GatewayDigest(call.Argv, host, taskID)
		req, err := ask.Raise(ask.ApprovalSpec{
			Source:  ask.SourceGateway,
			Argv:    append([]string(nil), call.Argv...),
			Effects: append([]string(nil), call.Effects...),
			Session: taskID,
			Token:   digest,
			Prompt:  "gateway run " + strings.Join(call.Argv, " ") + " on " + host,
		})
		if err != nil {
			return GatedGrant{}, false
		}
		return GatedGrant{
			ApprovalID: req.ID,
			Digest:     digest,
			Summary:    req.ApprovalSummary(),
			Effects:    append([]string(nil), call.Effects...),
		}, true
	}
}

// gatedResult answers a gated call with a needs-approval result when the
// policy carries a raise hook. Unknown commands never qualify, and a hook
// that declines or returns an incomplete binding falls through, so the
// caller reports the outright denial.
func (p *Policy) gatedResult(name string, argv, effects []string) (*mcpsdk.CallToolResult, GatedGrant, bool) {
	if p == nil || p.RaiseApproval == nil || !p.knownCommand(name) {
		return nil, GatedGrant{}, false
	}
	grant, ok := p.RaiseApproval(GatedCall{
		Name:    name,
		Argv:    append([]string(nil), argv...),
		Effects: append([]string(nil), effects...),
	})
	if !ok || grant.ApprovalID == "" || grant.Digest == "" {
		return nil, GatedGrant{}, false
	}
	if len(grant.Effects) == 0 {
		grant.Effects = append([]string(nil), effects...)
	}
	text := fmt.Sprintf("needs approval %s (digest %s): effects %s require approval",
		grant.ApprovalID, grant.Digest, strings.Join(grant.Effects, ","))
	return &mcpsdk.CallToolResult{
		IsError: true,
		Content: []mcpsdk.Content{&mcpsdk.TextContent{Text: text}},
		StructuredContent: NeedsApproval{
			State:      NeedsApprovalState,
			ApprovalID: grant.ApprovalID,
			Digest:     grant.Digest,
			Summary:    grant.Summary,
			Effects:    grant.Effects,
		},
	}, grant, true
}
