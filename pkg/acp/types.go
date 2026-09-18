// Package acp is a small, bashy-owned adapter for the Agent Client Protocol
// (ACP). It can both launch and drive an ACP agent subprocess and serve a
// bashy runner as an ACP agent over newline-delimited JSON-RPC.
//
// The public surface of this package is deliberately SDK-free: no type here
// is an alias to, and no exported signature mentions, the underlying
// github.com/coder/acp-go-sdk. That SDK is v0.x with no API-stability
// promise, so it is wrapped and converted at a single boundary (acp.go).
// Swapping it for a hand-written client is a one-file change; callers never
// need to import it. api_external_test.go pins this invariant.
package acp

import (
	"context"
	"time"

	"github.com/qiangli/yoke/pkg/gate"
)

// ProtocolVersionNumber is the ACP protocol version this client speaks.
const ProtocolVersionNumber = 1

// StopReason identifies why an agent stopped processing a turn. It is a
// bashy-owned string type — not an alias to the SDK's — with its own
// constants below.
type StopReason string

const (
	// StopReasonEndTurn: the agent finished the turn normally.
	StopReasonEndTurn StopReason = "end_turn"
	// StopReasonMaxTokens: the agent hit the token budget.
	StopReasonMaxTokens StopReason = "max_tokens"
	// StopReasonMaxTurnRequests: the agent hit the per-turn request cap.
	StopReasonMaxTurnRequests StopReason = "max_turn_requests"
	// StopReasonRefusal: the agent declined to continue.
	StopReasonRefusal StopReason = "refusal"
	// StopReasonCancelled: the turn was cancelled. This arrives as a
	// SUCCESSFUL prompt response carrying this reason, never as an error.
	StopReasonCancelled StopReason = "cancelled"
)

// Exit codes mapped from structured stop reasons.
const (
	ExitEndTurn         = 0
	ExitRefusal         = 3
	ExitCancelled       = 130
	ExitMaxTokens       = 4
	ExitMaxTurnRequests = 4
	ExitUnknown         = 1
)

// ExitFor maps a structured stop reason to a meaningful process exit status.
// A cancelled turn maps to ExitCancelled; it is still a successful response
// carrying StopReasonCancelled, never an error.
func ExitFor(r StopReason) int {
	switch r {
	case StopReasonEndTurn:
		return ExitEndTurn
	case StopReasonRefusal:
		return ExitRefusal
	case StopReasonCancelled:
		return ExitCancelled
	case StopReasonMaxTokens, StopReasonMaxTurnRequests:
		return ExitMaxTokens
	}
	return ExitUnknown
}

// ContentBlock is a piece of message content. Only text is modelled here —
// the one content kind this client sends and records. Non-text blocks
// (image/audio/resource) convert to a ContentBlock with an empty Text.
type ContentBlock struct {
	// Text is the plain or Markdown text of the block.
	Text string
}

// TextBlock creates a text content block for use in prompts.
func TextBlock(s string) ContentBlock { return ContentBlock{Text: s} }

// ClientCapabilities is the stable v1 client surface visible to a served
// agent. Omitted capabilities remain false: absence never implies support.
//
// Only the stable filesystem and terminal capabilities are modelled here.
// Unstable SDK additions deliberately do not leak into bashy's internal API.
type ClientCapabilities struct {
	FSReadTextFile  bool
	FSWriteTextFile bool
	Terminal        bool
}

// TurnRequest is one prompt delivered to a Runner.
type TurnRequest struct {
	SessionID    string
	Cwd          string
	Prompt       []ContentBlock
	Capabilities ClientCapabilities
}

// TurnResponse is the runner's self-reported result before verification.
type TurnResponse struct {
	Text       string
	StopReason StopReason
}

// Runner executes turns for an ACP agent. The context is cancelled when the
// ACP client sends session/cancel.
type Runner interface {
	Run(context.Context, TurnRequest) (TurnResponse, error)
}

// Session describes durable runner-owned session state. SessionLifecycle is
// optional; Agent advertises no lifecycle capabilities unless Runner
// implements it, preserving compatibility with existing runners.
type Session struct {
	ID        string
	Cwd       string
	Title     string
	UpdatedAt time.Time
}

// SessionLifecycle lets a runner own durable identifiers and lineage while
// the ACP adapter remains a policy-free framing boundary.
type SessionLifecycle interface {
	NewSession(context.Context, string) (Session, error)
	ResumeSession(context.Context, string, string) (Session, error)
	ListSessions(context.Context, string) ([]Session, error)
	CloseSession(context.Context, string) error
	ForkSession(context.Context, string, string) (Session, error)
}

// RunnerFunc adapts a function to Runner.
type RunnerFunc func(context.Context, TurnRequest) (TurnResponse, error)

// Run implements Runner.
func (f RunnerFunc) Run(ctx context.Context, req TurnRequest) (TurnResponse, error) {
	return f(ctx, req)
}

// AgentOptions configures the agent-side ACP adapter.
type AgentOptions struct {
	// Gate is the local command that decides whether an end_turn claim passed.
	// An empty gate leaves the turn conformant but unverified.
	Gate string
	// GateDir overrides the gate working directory. When empty, the session
	// working directory is used.
	GateDir string
	// OnResult receives the verified result after every completed prompt.
	OnResult func(AgentResult)
}

// AgentResult is the verified outcome of a served prompt.
type AgentResult struct {
	SessionID         string
	Text              string
	ClaimedStopReason StopReason
	StopReason        StopReason
	Gate              gate.Outcome
}

// Succeeded is the only success predicate callers should use. As with
// herald.Result, an absent gate is unverified and therefore not success.
func (r AgentResult) Succeeded() bool { return r.Gate.Trusted() }

// ToolCallStatus is the execution state of a tool call.
type ToolCallStatus string

const (
	ToolCallPending    ToolCallStatus = "pending"
	ToolCallInProgress ToolCallStatus = "in_progress"
	ToolCallCompleted  ToolCallStatus = "completed"
	ToolCallFailed     ToolCallStatus = "failed"
)

// ToolCallLocation is a file the agent reported a tool call touching.
type ToolCallLocation struct {
	// Path is the file path the tool call accessed or modified.
	Path string
	// Line is an optional 1-based line number; 0 when unspecified.
	Line int
}

// ToolCall describes a tool invocation reported by the agent. When it arrives
// as an update to an existing call, unchanged fields are zero-valued (an
// empty Status means "no status change", nil Locations means "no change").
type ToolCall struct {
	// ID is the unique identifier of this tool call within the session.
	ID string
	// Title is a human-readable description of what the tool is doing.
	Title string
	// Status is the current execution status, or "" if unspecified.
	Status ToolCallStatus
	// Kind is the category of tool being invoked (e.g. "edit", "read").
	Kind string
	// Locations are the files this tool call touched — the working set the
	// agent reports after the fact.
	Locations []ToolCallLocation
}

// SessionUpdate is one streamed update during a prompt turn. Exactly one
// field is non-nil per update.
type SessionUpdate struct {
	// AgentMessageChunk is a chunk of the agent's streamed reply.
	AgentMessageChunk *ContentBlock
	// AgentThoughtChunk is a chunk of the agent's streamed reasoning.
	AgentThoughtChunk *ContentBlock
	// UserMessageChunk is a chunk of the user's message being echoed back.
	UserMessageChunk *ContentBlock
	// ToolCall announces a newly initiated tool call.
	ToolCall *ToolCall
	// ToolCallUpdate carries a status/locations change to an existing call.
	ToolCallUpdate *ToolCall
}

// SessionNotification wraps a SessionUpdate with the session it pertains to.
type SessionNotification struct {
	// SessionID is the session this update pertains to.
	SessionID string
	// Update is the actual update content.
	Update SessionUpdate
}

// PermissionOptionKind hints at the nature of a permission option.
type PermissionOptionKind string

const (
	PermissionAllowOnce    PermissionOptionKind = "allow_once"
	PermissionAllowAlways  PermissionOptionKind = "allow_always"
	PermissionRejectOnce   PermissionOptionKind = "reject_once"
	PermissionRejectAlways PermissionOptionKind = "reject_always"
)

// PermissionOption is one choice offered when the agent requests permission.
type PermissionOption struct {
	// ID is the unique identifier of this option.
	ID string
	// Name is the human-readable label.
	Name string
	// Kind hints at the nature of the option.
	Kind PermissionOptionKind
}

// PermissionRequest is sent when the agent needs authorization for a tool
// call.
type PermissionRequest struct {
	// SessionID is the session making the request.
	SessionID string
	// ToolCall describes the tool call requiring permission.
	ToolCall ToolCall
	// Options are the choices available to answer the request.
	Options []PermissionOption
}

// PermissionResponse answers a PermissionRequest.
type PermissionResponse struct {
	// OptionID is the selected option's ID. Ignored when Cancelled is true.
	OptionID string
	// Cancelled reports the turn was cancelled before a choice was made.
	Cancelled bool
}

// Handler is the set of callbacks an ACP client must implement. It is a real
// interface over bashy-owned types — not an alias to the SDK's Client. Embed
// BaseHandler and override only the callbacks you care about. The SDK's
// filesystem and terminal callbacks are handled internally (declined) since
// this client advertises no such capabilities.
type Handler interface {
	// SessionUpdate receives one streamed update during a prompt turn.
	SessionUpdate(ctx context.Context, n SessionNotification) error
	// RequestPermission answers a request for authorization to run a tool.
	RequestPermission(ctx context.Context, req PermissionRequest) (PermissionResponse, error)
}

// BaseHandler provides default callback behavior: it ignores session updates
// and answers permission requests by selecting an "allow once" option when
// offered, otherwise cancelling. Embed it and override SessionUpdate (and/or
// RequestPermission) as needed.
type BaseHandler struct{}

// Compile-time check that BaseHandler satisfies Handler.
var _ Handler = (*BaseHandler)(nil)

// SessionUpdate ignores the update.
func (BaseHandler) SessionUpdate(context.Context, SessionNotification) error { return nil }

// RequestPermission selects the first "allow once" option if present,
// otherwise cancels.
func (BaseHandler) RequestPermission(_ context.Context, req PermissionRequest) (PermissionResponse, error) {
	for _, o := range req.Options {
		if o.Kind == PermissionAllowOnce {
			return PermissionResponse{OptionID: o.ID}, nil
		}
	}
	return PermissionResponse{Cancelled: true}, nil
}
