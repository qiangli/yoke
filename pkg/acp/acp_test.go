package acp

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// fakeAgent is a minimal SDK-side agent driven by per-test callbacks. It lives
// in the internal test package because it necessarily speaks SDK types; the
// public API guard is in api_external_test.go (package acp_test).
type fakeAgent struct {
	asc        *sdk.AgentSideConnection
	initialize func(sdk.InitializeRequest) (sdk.InitializeResponse, error)
	newSession func(sdk.NewSessionRequest) (sdk.NewSessionResponse, error)
	prompt     func(*fakeAgent, sdk.PromptRequest) (sdk.PromptResponse, error)
}

func (a *fakeAgent) Initialize(_ context.Context, r sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return a.initialize(r)
}
func (a *fakeAgent) NewSession(_ context.Context, r sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
	return a.newSession(r)
}
func (a *fakeAgent) Prompt(_ context.Context, r sdk.PromptRequest) (sdk.PromptResponse, error) {
	return a.prompt(a, r)
}
func (a *fakeAgent) Cancel(context.Context, sdk.CancelNotification) error { return nil }
func (a *fakeAgent) Authenticate(context.Context, sdk.AuthenticateRequest) (sdk.AuthenticateResponse, error) {
	return sdk.AuthenticateResponse{}, nil
}
func (a *fakeAgent) Logout(context.Context, sdk.LogoutRequest) (sdk.LogoutResponse, error) {
	return sdk.LogoutResponse{}, nil
}
func (a *fakeAgent) CloseSession(context.Context, sdk.CloseSessionRequest) (sdk.CloseSessionResponse, error) {
	return sdk.CloseSessionResponse{}, nil
}
func (a *fakeAgent) ListSessions(context.Context, sdk.ListSessionsRequest) (sdk.ListSessionsResponse, error) {
	return sdk.ListSessionsResponse{}, nil
}
func (a *fakeAgent) ResumeSession(context.Context, sdk.ResumeSessionRequest) (sdk.ResumeSessionResponse, error) {
	return sdk.ResumeSessionResponse{}, nil
}
func (a *fakeAgent) SetSessionConfigOption(context.Context, sdk.SetSessionConfigOptionRequest) (sdk.SetSessionConfigOptionResponse, error) {
	return sdk.SetSessionConfigOptionResponse{}, nil
}
func (a *fakeAgent) SetSessionMode(context.Context, sdk.SetSessionModeRequest) (sdk.SetSessionModeResponse, error) {
	return sdk.SetSessionModeResponse{}, nil
}

func newPipeClient(t *testing.T, handler Handler, agent *fakeAgent) *Client {
	t.Helper()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()

	agent.asc = sdk.NewAgentSideConnection(agent, a2cW, c2aR)
	conn := sdk.NewClientSideConnection(sdkAdapter{h: handler}, c2aW, a2cR)

	initResp, err := conn.Initialize(context.Background(), sdk.InitializeRequest{
		ProtocolVersion:    sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{},
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	return &Client{conn: conn, protocolVersion: int(initResp.ProtocolVersion)}
}

func newPipeAgentClient(t *testing.T, runner Runner, opts AgentOptions, caps sdk.ClientCapabilities) (*Agent, *sdk.ClientSideConnection) {
	t.Helper()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	agent := NewAgent(runner, opts, c2aR, a2cW)
	client := sdk.NewClientSideConnection(sdkAdapter{h: &BaseHandler{}}, c2aW, a2cR)

	initResp, err := client.Initialize(context.Background(), sdk.InitializeRequest{
		ProtocolVersion:    sdk.ProtocolVersion(ProtocolVersionNumber),
		ClientCapabilities: caps,
	})
	if err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if got := int(initResp.ProtocolVersion); got != ProtocolVersionNumber {
		t.Fatalf("protocol version = %d, want %d", got, ProtocolVersionNumber)
	}
	return agent, client
}

func defaultInit(r sdk.InitializeRequest) (sdk.InitializeResponse, error) {
	return sdk.InitializeResponse{ProtocolVersion: r.ProtocolVersion}, nil
}

func TestNewClient_InitializeAndNewSession(t *testing.T) {
	agent := &fakeAgent{
		initialize: func(r sdk.InitializeRequest) (sdk.InitializeResponse, error) {
			if r.ProtocolVersion != sdk.ProtocolVersionNumber {
				t.Errorf("protocol version = %d, want %d", r.ProtocolVersion, sdk.ProtocolVersionNumber)
			}
			return sdk.InitializeResponse{ProtocolVersion: r.ProtocolVersion}, nil
		},
		newSession: func(r sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			if r.Cwd != "/tmp/test" {
				t.Errorf("cwd = %q, want /tmp/test", r.Cwd)
			}
			if r.McpServers == nil {
				t.Error("McpServers is nil")
			}
			return sdk.NewSessionResponse{SessionId: "sess-001"}, nil
		},
		prompt: func(_ *fakeAgent, _ sdk.PromptRequest) (sdk.PromptResponse, error) {
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}

	client := newPipeClient(t, &RecordingHandler{}, agent)
	defer client.Close()

	if client.ProtocolVersion() != ProtocolVersionNumber {
		t.Errorf("ProtocolVersion() = %d, want %d", client.ProtocolVersion(), ProtocolVersionNumber)
	}

	sessionID, err := client.NewSession(context.Background(), "/tmp/test")
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	if sessionID != "sess-001" {
		t.Errorf("sessionID = %q, want sess-001", sessionID)
	}
}

func TestPrompt_StopReasons(t *testing.T) {
	// A cancelled turn must be a SUCCESSFUL response carrying the stop reason,
	// never an error.
	for _, tc := range []struct {
		name string
		sr   sdk.StopReason
		want StopReason
	}{
		{"end_turn", sdk.StopReasonEndTurn, StopReasonEndTurn},
		{"cancelled", sdk.StopReasonCancelled, StopReasonCancelled},
		{"refusal", sdk.StopReasonRefusal, StopReasonRefusal},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &fakeAgent{
				initialize: defaultInit,
				newSession: func(sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
					return sdk.NewSessionResponse{SessionId: "s"}, nil
				},
				prompt: func(_ *fakeAgent, r sdk.PromptRequest) (sdk.PromptResponse, error) {
					if len(r.Prompt) != 1 {
						t.Errorf("prompt blocks = %d, want 1", len(r.Prompt))
					}
					return sdk.PromptResponse{StopReason: tc.sr}, nil
				},
			}
			client := newPipeClient(t, &RecordingHandler{}, agent)
			defer client.Close()

			sid, err := client.NewSession(context.Background(), "/tmp/test")
			if err != nil {
				t.Fatalf("NewSession: %v", err)
			}
			got, err := client.Prompt(context.Background(), sid, "hello")
			if err != nil {
				t.Fatalf("Prompt: %v", err)
			}
			if got != tc.want {
				t.Errorf("stopReason = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestPrompt_StreamsThroughRecorder exercises the SDK->bashy boundary: the
// agent streams a text chunk and a tool call with locations, and the
// RecordingHandler captures them as bashy-owned values.
func TestPrompt_StreamsThroughRecorder(t *testing.T) {
	agent := &fakeAgent{
		initialize: defaultInit,
		newSession: func(sdk.NewSessionRequest) (sdk.NewSessionResponse, error) {
			return sdk.NewSessionResponse{SessionId: "s-stream"}, nil
		},
		prompt: func(a *fakeAgent, r sdk.PromptRequest) (sdk.PromptResponse, error) {
			ctx := context.Background()
			_ = a.asc.SessionUpdate(ctx, sdk.SessionNotification{
				SessionId: r.SessionId,
				Update: sdk.SessionUpdate{AgentMessageChunk: &sdk.SessionUpdateAgentMessageChunk{
					Content: sdk.ContentBlock{Text: &sdk.ContentBlockText{Text: "working ", Type: "text"}},
				}},
			})
			status := sdk.ToolCallStatusInProgress
			_ = a.asc.SessionUpdate(ctx, sdk.SessionNotification{
				SessionId: r.SessionId,
				Update: sdk.SessionUpdate{ToolCall: &sdk.SessionUpdateToolCall{
					ToolCallId: "tc-1",
					Title:      "edit",
					Status:     status,
					Locations: []sdk.ToolCallLocation{
						{Path: "src/main.go"},
						{Path: "src/utils.go"},
					},
				}},
			})
			return sdk.PromptResponse{StopReason: sdk.StopReasonEndTurn}, nil
		},
	}

	rec := &RecordingHandler{}
	client := newPipeClient(t, rec, agent)
	defer client.Close()

	sid, _ := client.NewSession(context.Background(), "/tmp/test")
	if _, err := client.Prompt(context.Background(), sid, "go"); err != nil {
		t.Fatalf("Prompt: %v", err)
	}

	if got := rec.AgentText(); got != "working " {
		t.Errorf("AgentText() = %q, want %q", got, "working ")
	}
	paths := rec.TouchedPaths()
	if len(paths) != 2 || paths[0] != "src/main.go" || paths[1] != "src/utils.go" {
		t.Errorf("TouchedPaths() = %v, want [src/main.go src/utils.go]", paths)
	}
	events := rec.ToolCallEvents()
	if len(events) != 1 || events[0].Status != ToolCallInProgress || events[0].ID != "tc-1" {
		t.Errorf("ToolCallEvents() = %+v, want one in_progress tc-1", events)
	}
}

func TestAgentInitializeV1CapabilitiesFailClosed(t *testing.T) {
	var got TurnRequest
	runner := RunnerFunc(func(_ context.Context, req TurnRequest) (TurnResponse, error) {
		got = req
		return TurnResponse{StopReason: StopReasonEndTurn}, nil
	})

	// fs:{} and terminal:false are the minimal v1 client surface. The SDK
	// encodes false-valued capabilities according to ACP's omission-is-false
	// rule; the agent must never promote an omitted field to supported.
	agent, client := newPipeAgentClient(t, runner, AgentOptions{}, sdk.ClientCapabilities{
		Fs:       sdk.FileSystemCapabilities{},
		Terminal: false,
	})

	cwd := t.TempDir()
	session, err := client.NewSession(context.Background(), sdk.NewSessionRequest{
		Cwd:        cwd,
		McpServers: []sdk.McpServer{},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	response, err := client.Prompt(context.Background(), sdk.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []sdk.ContentBlock{sdk.TextBlock("hello")},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if response.StopReason != sdk.StopReasonEndTurn {
		t.Fatalf("stop reason = %q, want end_turn", response.StopReason)
	}
	if got.Capabilities.FSReadTextFile || got.Capabilities.FSWriteTextFile || got.Capabilities.Terminal {
		t.Errorf("omitted capabilities became supported: %+v", got.Capabilities)
	}
	if got.Cwd != cwd {
		t.Errorf("cwd = %q, want %q", got.Cwd, cwd)
	}
	if result, ok := agent.LastResult(string(session.SessionId)); !ok || result.Succeeded() {
		t.Errorf("ungated result = (%+v, %v), want present and unverified", result, ok)
	}
}

func TestAgentInitializeRejectsNonV1(t *testing.T) {
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	NewAgent(RunnerFunc(func(context.Context, TurnRequest) (TurnResponse, error) {
		return TurnResponse{StopReason: StopReasonEndTurn}, nil
	}), AgentOptions{}, c2aR, a2cW)
	client := sdk.NewClientSideConnection(sdkAdapter{h: &BaseHandler{}}, c2aW, a2cR)

	_, err := client.Initialize(context.Background(), sdk.InitializeRequest{
		ProtocolVersion: sdk.ProtocolVersion(2),
	})
	if err == nil {
		t.Fatal("initialize protocolVersion 2 succeeded, want strict v1 rejection")
	}
	if !strings.Contains(err.Error(), "unsupported protocol version 2") {
		t.Fatalf("initialize error = %q, want agent's strict v1 rejection", err)
	}
}

func TestAgentCancelledTurnIsSuccessfulResponse(t *testing.T) {
	started := make(chan struct{})
	runner := RunnerFunc(func(ctx context.Context, _ TurnRequest) (TurnResponse, error) {
		close(started)
		<-ctx.Done()
		return TurnResponse{}, ctx.Err()
	})
	_, client := newPipeAgentClient(t, runner, AgentOptions{}, sdk.ClientCapabilities{})
	session, err := client.NewSession(context.Background(), sdk.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []sdk.McpServer{},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	type promptResult struct {
		response sdk.PromptResponse
		err      error
	}
	done := make(chan promptResult, 1)
	go func() {
		response, err := client.Prompt(context.Background(), sdk.PromptRequest{
			SessionId: session.SessionId,
			Prompt:    []sdk.ContentBlock{sdk.TextBlock("stop")},
		})
		done <- promptResult{response: response, err: err}
	}()

	<-started
	if err := client.Cancel(context.Background(), sdk.CancelNotification{SessionId: session.SessionId}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	select {
	case got := <-done:
		if got.err != nil {
			t.Fatalf("cancelled prompt returned JSON-RPC error: %v", got.err)
		}
		if got.response.StopReason != sdk.StopReasonCancelled {
			t.Errorf("stop reason = %q, want cancelled", got.response.StopReason)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancelled prompt did not return")
	}
}

type lifecycleRunner struct{ sessions map[string]Session }

func (r *lifecycleRunner) Run(context.Context, TurnRequest) (TurnResponse, error) {
	return TurnResponse{StopReason: StopReasonEndTurn}, nil
}
func (r *lifecycleRunner) NewSession(_ context.Context, cwd string) (Session, error) {
	value := Session{ID: "durable-1", Cwd: cwd}
	r.sessions[value.ID] = value
	return value, nil
}
func (r *lifecycleRunner) ResumeSession(_ context.Context, id, cwd string) (Session, error) {
	value, ok := r.sessions[id]
	if !ok || value.Cwd != cwd {
		return Session{}, fmt.Errorf("missing")
	}
	return value, nil
}
func (r *lifecycleRunner) ListSessions(context.Context, string) ([]Session, error) {
	out := make([]Session, 0, len(r.sessions))
	for _, value := range r.sessions {
		out = append(out, value)
	}
	return out, nil
}
func (r *lifecycleRunner) CloseSession(_ context.Context, id string) error {
	delete(r.sessions, id)
	return nil
}
func (r *lifecycleRunner) ForkSession(_ context.Context, id, cwd string) (Session, error) {
	if _, ok := r.sessions[id]; !ok {
		return Session{}, fmt.Errorf("missing")
	}
	value := Session{ID: "fork-1", Cwd: cwd}
	r.sessions[value.ID] = value
	return value, nil
}

func TestAgentAdvertisesAndDelegatesDurableSessionLifecycle(t *testing.T) {
	runner := &lifecycleRunner{sessions: map[string]Session{}}
	agent := NewAgent(runner, AgentOptions{}, strings.NewReader(""), io.Discard)
	initialized, err := agent.Initialize(context.Background(), sdk.InitializeRequest{ProtocolVersion: ProtocolVersionNumber})
	if err != nil {
		t.Fatal(err)
	}
	capabilities := initialized.AgentCapabilities
	if !capabilities.LoadSession || capabilities.SessionCapabilities.Resume == nil || capabilities.SessionCapabilities.List == nil || capabilities.SessionCapabilities.Close == nil || capabilities.SessionCapabilities.Fork == nil {
		t.Fatalf("capabilities=%#v", capabilities)
	}
	created, err := agent.NewSession(context.Background(), sdk.NewSessionRequest{Cwd: "/workspace", McpServers: []sdk.McpServer{}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := agent.ResumeSession(context.Background(), sdk.ResumeSessionRequest{SessionId: created.SessionId, Cwd: "/workspace"}); err != nil {
		t.Fatal(err)
	}
	if _, err := agent.LoadSession(context.Background(), sdk.LoadSessionRequest{SessionId: created.SessionId, Cwd: "/workspace", McpServers: []sdk.McpServer{}}); err != nil {
		t.Fatal(err)
	}
	listed, err := agent.ListSessions(context.Background(), sdk.ListSessionsRequest{})
	if err != nil || len(listed.Sessions) != 1 {
		t.Fatalf("listed=%#v err=%v", listed, err)
	}
	forked, err := agent.UnstableForkSession(context.Background(), sdk.UnstableForkSessionRequest{SessionId: created.SessionId, Cwd: "/workspace"})
	if err != nil || forked.SessionId == created.SessionId {
		t.Fatalf("fork=%#v err=%v", forked, err)
	}
	if _, err := agent.CloseSession(context.Background(), sdk.CloseSessionRequest{SessionId: created.SessionId}); err != nil {
		t.Fatal(err)
	}
}

func TestAgentFailedGateOverridesEndTurnClaim(t *testing.T) {
	runner := RunnerFunc(func(context.Context, TurnRequest) (TurnResponse, error) {
		return TurnResponse{Text: "claimed done", StopReason: StopReasonEndTurn}, nil
	})
	agent, client := newPipeAgentClient(t, runner, AgentOptions{Gate: "exit 7"}, sdk.ClientCapabilities{})
	session, err := client.NewSession(context.Background(), sdk.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []sdk.McpServer{},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	response, err := client.Prompt(context.Background(), sdk.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []sdk.ContentBlock{sdk.TextBlock("finish")},
	})
	if err != nil {
		t.Fatalf("prompt: %v", err)
	}
	if response.StopReason != sdk.StopReasonRefusal {
		t.Errorf("wire stop reason = %q, want legal fail-closed refusal", response.StopReason)
	}
	result, ok := agent.LastResult(string(session.SessionId))
	if !ok {
		t.Fatal("verified result was not recorded")
	}
	if result.ClaimedStopReason != StopReasonEndTurn {
		t.Errorf("claimed stop reason = %q, want end_turn", result.ClaimedStopReason)
	}
	if !result.Gate.Ran || result.Gate.Passed || result.Gate.ExitCode != 7 {
		t.Errorf("gate = %+v, want ran, failed, exit 7", result.Gate)
	}
	if result.Succeeded() {
		t.Error("failed gate reported success")
	}
}

func TestAgentUngatedClientDegradesToConformantACP(t *testing.T) {
	runner := RunnerFunc(func(context.Context, TurnRequest) (TurnResponse, error) {
		return TurnResponse{StopReason: StopReasonEndTurn}, nil
	})
	agent, client := newPipeAgentClient(t, runner, AgentOptions{}, sdk.ClientCapabilities{})
	session, err := client.NewSession(context.Background(), sdk.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []sdk.McpServer{},
	})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}

	// This stock SDK client never negotiates or mentions a gate.
	response, err := client.Prompt(context.Background(), sdk.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []sdk.ContentBlock{sdk.TextBlock("ordinary ACP")},
	})
	if err != nil {
		t.Fatalf("ordinary ACP prompt: %v", err)
	}
	if response.StopReason != sdk.StopReasonEndTurn {
		t.Errorf("stop reason = %q, want conformant end_turn", response.StopReason)
	}
	result, ok := agent.LastResult(string(session.SessionId))
	if !ok || result.Gate.Ran || result.Succeeded() {
		t.Errorf("degraded result = (%+v, %v), want present and unverified", result, ok)
	}
}
