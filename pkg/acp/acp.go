package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"sync/atomic"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/qiangli/yoke/pkg/gate"
)

// This is the ONE file that imports the ACP SDK. Every SDK type stays behind
// this boundary: the public API (types.go, recorder.go) is defined in
// bashy-owned types, and the conversions below translate between the two.
// Replacing the SDK with a hand-written client means rewriting only this file.

// Client launches and manages an ACP agent subprocess.
type Client struct {
	conn            *sdk.ClientSideConnection
	cmd             *exec.Cmd
	protocolVersion int

	// waited is closed when the subprocess has been reaped, and waitErr holds
	// what Wait returned. There is EXACTLY ONE cmd.Wait() for a client — the
	// goroutine NewClient starts — because a second call returns "Wait was
	// already called" and would corrupt the first one's result.
	//
	// This exists so a caller can learn the agent DIED ON ITS OWN. Without it
	// the only signal was Close(), which the caller invokes, so an agent that
	// crashed mid-turn left its session looking permanently live.
	waited  chan struct{}
	waitErr error
}

// NewClient starts the given command as an ACP agent subprocess, connects to
// it, and initializes with minimal capabilities (filesystem and terminal
// declined via an empty ClientCapabilities).
//
// The handler implements the callbacks the agent will invoke. Use BaseHandler
// (or RecordingHandler) if you only need the default behavior.
func NewClient(ctx context.Context, handler Handler, cmd *exec.Cmd) (*Client, error) {
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin pipe: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start: %w", err)
	}

	conn := sdk.NewClientSideConnection(sdkAdapter{h: handler}, stdin, stdout)

	initResp, err := conn.Initialize(ctx, sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersion(ProtocolVersionNumber),
		ClientCapabilities: sdk.ClientCapabilities{
			Fs:       sdk.FileSystemCapabilities{},
			Terminal: false,
		},
	})
	if err != nil {
		// Reap the subprocess we just started so it does not linger.
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf("acp: initialize: %w", err)
	}
	if int(initResp.ProtocolVersion) != ProtocolVersionNumber {
		cmd.Process.Kill()
		cmd.Wait()
		return nil, fmt.Errorf(
			"acp: incompatible protocol version %d (want %d)",
			initResp.ProtocolVersion, ProtocolVersionNumber,
		)
	}

	c := &Client{
		conn:            conn,
		cmd:             cmd,
		protocolVersion: int(initResp.ProtocolVersion),
		waited:          make(chan struct{}),
	}
	// THE ONLY cmd.Wait() for this client. Started here, on the success path
	// only: the failure paths above reap the subprocess themselves before any
	// Client exists to own it.
	go func() {
		c.waitErr = cmd.Wait()
		close(c.waited)
	}()
	return c, nil
}

// Done is closed when the agent subprocess has exited, however it exited —
// crashed, killed by Close, or finished on its own.
//
// Watch it to notice an agent that DIED WITHOUT BEING ASKED TO. An ACP session
// has no other symptom for that: the protocol simply goes quiet, and a caller
// waiting on a turn that will never arrive cannot tell a dead agent from a slow
// one. Callers that own a session lifecycle must select on this.
func (c *Client) Done() <-chan struct{} {
	if c.waited == nil {
		// A client with no subprocess (pipe transport in tests) never dies on
		// its own, so a nil channel — which blocks forever — is the honest
		// answer rather than a channel that closes immediately.
		return nil
	}
	return c.waited
}

// ProtocolVersion reports the strictly negotiated protocol version.
func (c *Client) ProtocolVersion() int { return c.protocolVersion }

// NewSession creates a session with the given absolute working directory.
func (c *Client) NewSession(ctx context.Context, cwd string) (string, error) {
	resp, err := c.conn.NewSession(ctx, sdk.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []sdk.McpServer{},
	})
	if err != nil {
		return "", fmt.Errorf("acp: new session: %w", err)
	}
	return string(resp.SessionId), nil
}

// Prompt sends a text prompt to the given session and blocks until the turn
// completes, returning the structured stop reason. A cancelled turn returns
// StopReasonCancelled with a nil error.
func (c *Client) Prompt(ctx context.Context, sessionID, prompt string) (StopReason, error) {
	resp, err := c.conn.Prompt(ctx, sdk.PromptRequest{
		SessionId: sdk.SessionId(sessionID),
		Prompt:    []sdk.ContentBlock{toSDKContent(TextBlock(prompt))},
	})
	if err != nil {
		return "", fmt.Errorf("acp: prompt: %w", err)
	}
	return StopReason(resp.StopReason), nil
}

// Cancel sends a cancellation notification for the given session.
func (c *Client) Cancel(ctx context.Context, sessionID string) error {
	return c.conn.Cancel(ctx, sdk.CancelNotification{SessionId: sdk.SessionId(sessionID)})
}

// Kill terminates the agent subprocess WITHOUT reaping it. Prefer Close, which
// also waits. Kill is a no-op if no subprocess was launched (e.g. tests using
// pipe transport).
func (c *Client) Kill() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	return c.cmd.Process.Kill()
}

// Close terminates the agent subprocess and waits for it to exit, releasing
// its resources so it does not become a zombie. It is a no-op if no
// subprocess was launched. The error from Wait is returned (typically a
// non-nil "signal: killed" for a killed process), except that a nil-process
// client closes cleanly.
func (c *Client) Close() error {
	if c.cmd == nil || c.cmd.Process == nil {
		return nil
	}
	killErr := c.cmd.Process.Kill()
	// Wait on the ONE reaper rather than calling cmd.Wait() again: a second
	// Wait returns "Wait was already called" and races the first.
	if c.waited != nil {
		<-c.waited
	}
	if killErr != nil {
		return killErr
	}
	return c.waitErr
}

// Agent serves a Runner through the ACP v1 agent-side connection.
type Agent struct {
	conn   *sdk.AgentSideConnection
	runner Runner
	opts   AgentOptions

	mu           sync.RWMutex
	sessions     map[string]string
	capabilities ClientCapabilities
	results      map[string]AgentResult
	nextSession  atomic.Uint64
}

// NewAgent exposes runner as an ACP v1 agent over input/output. The SDK owns
// newline-delimited JSON-RPC framing; callers should normally pass os.Stdin
// and os.Stdout from the eventual "bashy acp" command.
func NewAgent(runner Runner, opts AgentOptions, input io.Reader, output io.Writer) *Agent {
	a := &Agent{
		runner:   runner,
		opts:     opts,
		sessions: make(map[string]string),
		results:  make(map[string]AgentResult),
	}
	a.conn = sdk.NewAgentSideConnection(a, output, input)
	return a
}

// Done is closed when the ACP connection stops.
func (a *Agent) Done() <-chan struct{} { return a.conn.Done() }

// LastResult returns the latest verified result for sessionID.
func (a *Agent) LastResult(sessionID string) (AgentResult, bool) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	r, ok := a.results[sessionID]
	return r, ok
}

// Compile-time check that Agent satisfies the SDK's agent interface.
var _ sdk.Agent = (*Agent)(nil)
var _ sdk.AgentLoader = (*Agent)(nil)

func (a *Agent) Initialize(_ context.Context, req sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	if int(req.ProtocolVersion) != ProtocolVersionNumber {
		return sdk.InitializeResponse{}, fmt.Errorf(
			"acp: unsupported protocol version %d (want %d)",
			req.ProtocolVersion, ProtocolVersionNumber,
		)
	}

	// Fail closed: the SDK's zero values preserve the v1 rule that omitted
	// capabilities are unsupported.
	a.mu.Lock()
	a.capabilities = ClientCapabilities{
		FSReadTextFile:  req.ClientCapabilities.Fs.ReadTextFile,
		FSWriteTextFile: req.ClientCapabilities.Fs.WriteTextFile,
		Terminal:        req.ClientCapabilities.Terminal,
	}
	a.mu.Unlock()

	capabilities := sdk.AgentCapabilities{}
	if _, ok := a.runner.(SessionLifecycle); ok {
		capabilities.LoadSession = true
		capabilities.SessionCapabilities = sdk.SessionCapabilities{
			Close: &sdk.SessionCloseCapabilities{}, List: &sdk.SessionListCapabilities{},
			Resume: &sdk.SessionResumeCapabilities{}, Fork: &sdk.SessionForkCapabilities{},
		}
	}
	return sdk.InitializeResponse{
		ProtocolVersion:   sdk.ProtocolVersion(ProtocolVersionNumber),
		AgentCapabilities: capabilities,
		AuthMethods:       []sdk.AuthMethod{},
	}, nil
}

func (a *Agent) NewSession(ctx context.Context, req sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	if lifecycle, ok := a.runner.(SessionLifecycle); ok {
		session, err := lifecycle.NewSession(ctx, req.Cwd)
		if err != nil {
			return sdk.NewSessionResponse{}, err
		}
		if session.ID == "" || session.Cwd != req.Cwd {
			return sdk.NewSessionResponse{}, fmt.Errorf("acp: runner returned invalid session")
		}
		a.mu.Lock()
		a.sessions[session.ID] = session.Cwd
		a.mu.Unlock()
		return sdk.NewSessionResponse{SessionId: sdk.SessionId(session.ID)}, nil
	}
	id := fmt.Sprintf("bashy-%d", a.nextSession.Add(1))
	a.mu.Lock()
	a.sessions[id] = req.Cwd
	a.mu.Unlock()
	return sdk.NewSessionResponse{SessionId: sdk.SessionId(id)}, nil
}

func (a *Agent) Prompt(ctx context.Context, req sdk.PromptRequest) (sdk.PromptResponse, error) {
	sessionID := string(req.SessionId)
	a.mu.RLock()
	cwd, ok := a.sessions[sessionID]
	caps := a.capabilities
	a.mu.RUnlock()
	if !ok {
		return sdk.PromptResponse{}, fmt.Errorf("acp: unknown session %q", sessionID)
	}
	if a.runner == nil {
		return sdk.PromptResponse{}, fmt.Errorf("acp: no runner configured")
	}

	prompt := make([]ContentBlock, 0, len(req.Prompt))
	for _, block := range req.Prompt {
		prompt = append(prompt, fromSDKContent(block))
	}
	claimed, err := a.runner.Run(ctx, TurnRequest{
		SessionID:    sessionID,
		Cwd:          cwd,
		Prompt:       prompt,
		Capabilities: caps,
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			claimed.StopReason = StopReasonCancelled
		} else {
			return sdk.PromptResponse{}, fmt.Errorf("acp: run prompt: %w", err)
		}
	}
	if claimed.StopReason == "" && errors.Is(ctx.Err(), context.Canceled) {
		claimed.StopReason = StopReasonCancelled
	}
	if !validStopReason(claimed.StopReason) {
		return sdk.PromptResponse{}, fmt.Errorf("acp: invalid stop reason %q", claimed.StopReason)
	}

	// Cancellation is a successful prompt response. Do not attempt output or
	// gate work with a cancelled context.
	if claimed.StopReason != StopReasonCancelled && claimed.Text != "" {
		if err := a.conn.SessionUpdate(ctx, sdk.SessionNotification{
			SessionId: req.SessionId,
			Update: sdk.SessionUpdate{
				AgentMessageChunk: &sdk.SessionUpdateAgentMessageChunk{
					Content:       toSDKContent(TextBlock(claimed.Text)),
					SessionUpdate: "agent_message_chunk",
				},
			},
		}); err != nil {
			return sdk.PromptResponse{}, fmt.Errorf("acp: stream response: %w", err)
		}
	}

	result := a.verifyTurn(ctx, sessionID, cwd, claimed)
	a.mu.Lock()
	a.results[sessionID] = result
	a.mu.Unlock()
	if a.opts.OnResult != nil {
		a.opts.OnResult(result)
	}
	return sdk.PromptResponse{StopReason: sdk.StopReason(result.StopReason)}, nil
}

func (a *Agent) verifyTurn(ctx context.Context, sessionID, cwd string, claimed TurnResponse) AgentResult {
	result := AgentResult{
		SessionID:         sessionID,
		Text:              claimed.Text,
		ClaimedStopReason: claimed.StopReason,
		StopReason:        claimed.StopReason,
	}
	if claimed.StopReason == StopReasonCancelled {
		return result
	}

	gateDir := a.opts.GateDir
	if gateDir == "" {
		gateDir = cwd
	}
	result.Gate = gate.RunLocal(ctx, gateDir, a.opts.Gate, string(claimed.StopReason))
	if claimed.StopReason == StopReasonEndTurn && result.Gate.Ran && !result.Gate.Passed {
		// ACP v1 has no generic "verification failed" stop reason. Refusal is
		// the fail-closed legal response: never preserve an unverified
		// end_turn claim as success.
		result.StopReason = StopReasonRefusal
	}
	return result
}

func validStopReason(reason StopReason) bool {
	switch reason {
	case StopReasonEndTurn, StopReasonMaxTokens, StopReasonMaxTurnRequests,
		StopReasonRefusal, StopReasonCancelled:
		return true
	default:
		return false
	}
}

func (*Agent) Cancel(context.Context, sdk.CancelNotification) error { return nil }

func (*Agent) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, fmt.Errorf("acp: authentication not offered")
}

func (*Agent) Logout(context.Context, sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, fmt.Errorf("acp: logout not offered")
}

func (a *Agent) CloseSession(ctx context.Context, req sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	lifecycle, ok := a.runner.(SessionLifecycle)
	if !ok {
		return sdk.CloseSessionResponse{}, fmt.Errorf("acp: session close not offered")
	}
	if err := lifecycle.CloseSession(ctx, string(req.SessionId)); err != nil {
		return sdk.CloseSessionResponse{}, err
	}
	a.mu.Lock()
	delete(a.sessions, string(req.SessionId))
	a.mu.Unlock()
	return sdk.CloseSessionResponse{}, nil
}

func (a *Agent) ListSessions(ctx context.Context, req sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	lifecycle, ok := a.runner.(SessionLifecycle)
	if !ok {
		return sdk.ListSessionsResponse{}, fmt.Errorf("acp: session list not offered")
	}
	if req.Cursor != nil {
		return sdk.ListSessionsResponse{}, fmt.Errorf("acp: session list cursor is not supported")
	}
	cwd := ""
	if req.Cwd != nil {
		cwd = *req.Cwd
	}
	sessions, err := lifecycle.ListSessions(ctx, cwd)
	if err != nil {
		return sdk.ListSessionsResponse{}, err
	}
	result := sdk.ListSessionsResponse{Sessions: make([]sdk.SessionInfo, 0, len(sessions))}
	for _, session := range sessions {
		if session.ID == "" || session.Cwd == "" {
			return sdk.ListSessionsResponse{}, fmt.Errorf("acp: runner returned invalid session")
		}
		info := sdk.SessionInfo{SessionId: sdk.SessionId(session.ID), Cwd: session.Cwd}
		if session.Title != "" {
			title := session.Title
			info.Title = &title
		}
		if !session.UpdatedAt.IsZero() {
			updated := session.UpdatedAt.UTC().Format(time.RFC3339Nano)
			info.UpdatedAt = &updated
		}
		result.Sessions = append(result.Sessions, info)
	}
	return result, nil
}

func (a *Agent) ResumeSession(ctx context.Context, req sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	lifecycle, ok := a.runner.(SessionLifecycle)
	if !ok {
		return sdk.ResumeSessionResponse{}, fmt.Errorf("acp: session resume not offered")
	}
	session, err := lifecycle.ResumeSession(ctx, string(req.SessionId), req.Cwd)
	if err != nil {
		return sdk.ResumeSessionResponse{}, err
	}
	if session.ID != string(req.SessionId) || session.Cwd != req.Cwd {
		return sdk.ResumeSessionResponse{}, fmt.Errorf("acp: runner returned invalid resumed session")
	}
	a.mu.Lock()
	a.sessions[session.ID] = session.Cwd
	a.mu.Unlock()
	return sdk.ResumeSessionResponse{}, nil
}

// LoadSession restores durable runner state through ACP's baseline load
// operation. ResumeSession exposes the newer no-replay lifecycle operation;
// both attach the same runner-owned session to this connection exactly once.
func (a *Agent) LoadSession(ctx context.Context, req sdk.LoadSessionRequest) (sdk.LoadSessionResponse, error) {
	lifecycle, ok := a.runner.(SessionLifecycle)
	if !ok {
		return sdk.LoadSessionResponse{}, fmt.Errorf("acp: session load not offered")
	}
	session, err := lifecycle.ResumeSession(ctx, string(req.SessionId), req.Cwd)
	if err != nil {
		return sdk.LoadSessionResponse{}, err
	}
	if session.ID != string(req.SessionId) || session.Cwd != req.Cwd {
		return sdk.LoadSessionResponse{}, fmt.Errorf("acp: runner returned invalid loaded session")
	}
	a.mu.Lock()
	a.sessions[session.ID] = session.Cwd
	a.mu.Unlock()
	return sdk.LoadSessionResponse{}, nil
}

// UnstableForkSession is implemented only because ACP v1 currently exposes
// fork under its unstable extension. Capability advertisement remains exact.
func (a *Agent) UnstableForkSession(ctx context.Context, req sdk.UnstableForkSessionRequest) (sdk.UnstableForkSessionResponse, error) {
	lifecycle, ok := a.runner.(SessionLifecycle)
	if !ok {
		return sdk.UnstableForkSessionResponse{}, fmt.Errorf("acp: session fork not offered")
	}
	session, err := lifecycle.ForkSession(ctx, string(req.SessionId), req.Cwd)
	if err != nil {
		return sdk.UnstableForkSessionResponse{}, err
	}
	if session.ID == "" || session.ID == string(req.SessionId) || session.Cwd != req.Cwd {
		return sdk.UnstableForkSessionResponse{}, fmt.Errorf("acp: runner returned invalid forked session")
	}
	a.mu.Lock()
	a.sessions[session.ID] = session.Cwd
	a.mu.Unlock()
	return sdk.UnstableForkSessionResponse{SessionId: sdk.SessionId(session.ID)}, nil
}

func (*Agent) SetSessionConfigOption(context.Context, sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	return sdk.SetSessionConfigOptionResponse{}, fmt.Errorf("acp: session configuration not offered")
}

func (*Agent) SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, fmt.Errorf("acp: session modes not offered")
}

// sdkAdapter bridges a bashy-owned Handler to the SDK's acp.Client interface.
// It handles the two callbacks a bashy client cares about (session updates and
// permission requests) and declines every filesystem and terminal callback —
// this client advertises no such capabilities, so a well-behaved agent never
// invokes them. These declining stubs, and the empty ClientCapabilities that
// justifies them, are the nine required acp.Client methods.
type sdkAdapter struct{ h Handler }

// Compile-time check that the adapter satisfies the SDK's Client interface.
var _ sdk.Client = sdkAdapter{}

func (a sdkAdapter) SessionUpdate(ctx context.Context, n sdk.SessionNotification) error {
	return a.h.SessionUpdate(ctx, fromSDKNotification(n))
}

func (a sdkAdapter) RequestPermission(ctx context.Context, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	resp, err := a.h.RequestPermission(ctx, fromSDKPermissionRequest(p))
	if err != nil {
		return sdk.RequestPermissionResponse{}, err
	}
	return toSDKPermissionResponse(resp), nil
}

func (sdkAdapter) ReadTextFile(context.Context, sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, fmt.Errorf("fs.readTextFile not offered")
}

func (sdkAdapter) WriteTextFile(context.Context, sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, fmt.Errorf("fs.writeTextFile not offered")
}

func (sdkAdapter) CreateTerminal(context.Context, sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, fmt.Errorf("terminal not offered")
}

func (sdkAdapter) KillTerminal(context.Context, sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, fmt.Errorf("terminal not offered")
}

func (sdkAdapter) TerminalOutput(context.Context, sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, fmt.Errorf("terminal not offered")
}

func (sdkAdapter) ReleaseTerminal(context.Context, sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, fmt.Errorf("terminal not offered")
}

func (sdkAdapter) WaitForTerminalExit(context.Context, sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, fmt.Errorf("terminal not offered")
}

// --- boundary conversions: SDK types <-> bashy-owned types ---

func toSDKContent(cb ContentBlock) sdk.ContentBlock {
	return sdk.ContentBlock{Text: &sdk.ContentBlockText{Text: cb.Text, Type: "text"}}
}

func fromSDKContent(c sdk.ContentBlock) ContentBlock {
	if c.Text != nil {
		return ContentBlock{Text: c.Text.Text}
	}
	return ContentBlock{}
}

func fromSDKNotification(n sdk.SessionNotification) SessionNotification {
	u := n.Update
	var out SessionUpdate
	switch {
	case u.AgentMessageChunk != nil:
		cb := fromSDKContent(u.AgentMessageChunk.Content)
		out.AgentMessageChunk = &cb
	case u.AgentThoughtChunk != nil:
		cb := fromSDKContent(u.AgentThoughtChunk.Content)
		out.AgentThoughtChunk = &cb
	case u.UserMessageChunk != nil:
		cb := fromSDKContent(u.UserMessageChunk.Content)
		out.UserMessageChunk = &cb
	case u.ToolCall != nil:
		tc := fromSDKToolCall(u.ToolCall)
		out.ToolCall = &tc
	case u.ToolCallUpdate != nil:
		tc := fromSDKSessionToolCallUpdate(u.ToolCallUpdate)
		out.ToolCallUpdate = &tc
	}
	return SessionNotification{SessionID: string(n.SessionId), Update: out}
}

func fromSDKToolCall(t *sdk.SessionUpdateToolCall) ToolCall {
	return ToolCall{
		ID:        string(t.ToolCallId),
		Title:     t.Title,
		Status:    ToolCallStatus(t.Status),
		Kind:      string(t.Kind),
		Locations: fromSDKLocations(t.Locations),
	}
}

func fromSDKSessionToolCallUpdate(t *sdk.SessionToolCallUpdate) ToolCall {
	tc := ToolCall{
		ID:        string(t.ToolCallId),
		Locations: fromSDKLocations(t.Locations),
	}
	if t.Status != nil {
		tc.Status = ToolCallStatus(*t.Status)
	}
	if t.Title != nil {
		tc.Title = *t.Title
	}
	if t.Kind != nil {
		tc.Kind = string(*t.Kind)
	}
	return tc
}

func fromSDKToolCallUpdate(t sdk.ToolCallUpdate) ToolCall {
	tc := ToolCall{
		ID:        string(t.ToolCallId),
		Locations: fromSDKLocations(t.Locations),
	}
	if t.Status != nil {
		tc.Status = ToolCallStatus(*t.Status)
	}
	if t.Title != nil {
		tc.Title = *t.Title
	}
	if t.Kind != nil {
		tc.Kind = string(*t.Kind)
	}
	return tc
}

func fromSDKLocations(in []sdk.ToolCallLocation) []ToolCallLocation {
	if len(in) == 0 {
		return nil
	}
	out := make([]ToolCallLocation, len(in))
	for i, l := range in {
		out[i] = ToolCallLocation{Path: l.Path}
		if l.Line != nil {
			out[i].Line = *l.Line
		}
	}
	return out
}

func fromSDKPermissionRequest(p sdk.RequestPermissionRequest) PermissionRequest {
	req := PermissionRequest{
		SessionID: string(p.SessionId),
		ToolCall:  fromSDKToolCallUpdate(p.ToolCall),
	}
	for _, o := range p.Options {
		req.Options = append(req.Options, PermissionOption{
			ID:   string(o.OptionId),
			Name: o.Name,
			Kind: PermissionOptionKind(o.Kind),
		})
	}
	return req
}

func toSDKPermissionResponse(r PermissionResponse) sdk.RequestPermissionResponse {
	if r.Cancelled || r.OptionID == "" {
		return sdk.RequestPermissionResponse{
			Outcome: sdk.RequestPermissionOutcome{Cancelled: &sdk.RequestPermissionOutcomeCancelled{}},
		}
	}
	return sdk.RequestPermissionResponse{
		Outcome: sdk.RequestPermissionOutcome{
			Selected: &sdk.RequestPermissionOutcomeSelected{OptionId: sdk.PermissionOptionId(r.OptionID)},
		},
	}
}
