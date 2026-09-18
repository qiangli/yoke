package chat

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/acp"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

// TestFakeACPAgentHelper is the FAKE ACP AGENT — a real subprocess speaking
// real JSON-RPC over real stdio, not a stub and not a live CLI.
//
// It is this test binary re-exec'd with CHAT_FAKE_ACP_AGENT=1, which is the
// standard way to get a hermetic child process out of a Go test: no toolchain
// at test time, no network, no third-party agent installed. It blocks until the
// parent closes the connection, so the testing package never gets a chance to
// print its own verdict onto the protocol stream.
func TestFakeACPAgentHelper(t *testing.T) {
	if os.Getenv("CHAT_FAKE_ACP_AGENT") != "1" {
		t.Skip("helper process; run via TestACPSessionIsDrivenOverTheProtocol")
	}
	agent := acp.NewAgent(
		acp.RunnerFunc(func(_ context.Context, req acp.TurnRequest) (acp.TurnResponse, error) {
			var b strings.Builder
			for _, p := range req.Prompt {
				b.WriteString(p.Text)
			}
			if strings.Contains(b.String(), "refuse") {
				return acp.TurnResponse{Text: "no", StopReason: acp.StopReasonRefusal}, nil
			}
			if os.Getenv("CHAT_FAKE_ACP_EXIT_DURING_PROMPT") == "1" {
				os.Exit(42)
			}
			if os.Getenv("CHAT_FAKE_ACP_EXIT_AFTER_PROMPT") == "1" {
				time.AfterFunc(100*time.Millisecond, func() { os.Exit(0) })
			}
			return acp.TurnResponse{Text: "ack:" + b.String(), StopReason: acp.StopReasonEndTurn}, nil
		}),
		acp.AgentOptions{},
		os.Stdin, os.Stdout,
	)
	<-agent.Done()
}

// pinACPCatalog installs a tool whose ACP launch is the helper above, and
// points the launcher at a store containing it.
func pinACPCatalog(t *testing.T) {
	t.Helper()
	// Keep the launcher off the developer's own home: room cards, shims and the
	// event dir all resolve from it.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
	t.Setenv("CHAT_FAKE_ACP_AGENT", "1")

	root := t.TempDir()
	prev := newCatalog
	newCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { newCatalog = prev })

	self, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	tool := fleet.Tool{
		Name: "fakeacp",
		Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{
			Binary: self,
			Launch: fleet.ToolLaunch{
				// A headless one-shot it will never use here, so the resolution
				// path is the ordinary one.
				Exec: "fakeacp -run {prompt}",
				// Deliberately NO steer_exec: a tool can speak ACP and have no
				// interactive pty launch at all, and that must still be drivable.
				ACPExec: "fakeacp -test.run=^TestFakeACPAgentHelper$",
			},
		},
	}
	if err := newCatalog().SaveTool(tool); err != nil {
		t.Fatalf("SaveTool: %v", err)
	}
}

func TestACPSessionIsDrivenOverTheProtocol(t *testing.T) {
	pinACPCatalog(t)
	t.Setenv(agentlaunch.ACPEnv, "1")

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	var streamed strings.Builder
	s, err := Start(ctx, "fakeacp", SessionOptions{
		Prompt: "hello",
		Cwd:    t.TempDir(),
		Stream: &streamed,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	if got := s.Rung(); got != agentlaunch.RungACPNative {
		t.Errorf("Rung = %s, want %s", got, agentlaunch.RungACPNative)
	}
	if s.CtlSock != "" {
		t.Errorf("an ACP session advertised a pty control socket: %q", s.CtlSock)
	}
	if s.EventsPath() != "" {
		t.Errorf("an ACP session advertised an events file: %q", s.EventsPath())
	}

	// THE POINT OF THE WHOLE EXERCISE: this returns when the agent SAYS the turn
	// ended, not after 25 seconds of silence. A quiet period is passed and
	// ignored; if it were being honoured this would take 30 seconds.
	started := time.Now()
	if err := s.WaitIdle(ctx, 30*time.Second); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 20*time.Second {
		t.Errorf("WaitIdle took %s — it waited for silence, not for the report", elapsed)
	}
	if got := s.ACPStopReason(); got != string(acp.StopReasonEndTurn) {
		t.Errorf("ACPStopReason = %q, want %q", got, acp.StopReasonEndTurn)
	}
	if got := s.Turn(); got != "ack:hello" {
		t.Errorf("Turn = %q, want %q", got, "ack:hello")
	}

	// A steer is the next prompt in the SAME session, and it is answered.
	if err := s.Say("again"); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if err := s.WaitIdle(ctx, 30*time.Second); err != nil {
		t.Fatalf("WaitIdle after Say: %v", err)
	}
	if got := s.Turn(); got != "ack:again" {
		t.Errorf("Turn after Say = %q, want %q", got, "ack:again")
	}

	// A stop reason that is NOT end_turn arrives as a fact, where rung 4 would
	// have reported an ordinary quiet turn and called it success.
	if err := s.Say("please refuse"); err != nil {
		t.Fatalf("Say: %v", err)
	}
	if err := s.WaitIdle(ctx, 30*time.Second); err != nil {
		t.Fatalf("WaitIdle after refusal: %v", err)
	}
	if got := s.ACPStopReason(); got != string(acp.StopReasonRefusal) {
		t.Errorf("ACPStopReason = %q, want %q", got, acp.StopReasonRefusal)
	}

	// The transcript is the same tee every observer of a session already reads.
	if got := s.Output(); !strings.Contains(got, "ack:hello") || !strings.Contains(got, "ack:again") {
		t.Errorf("Output = %q, want both turns", got)
	}
	if got := streamed.String(); !strings.Contains(got, "ack:hello") {
		t.Errorf("Stream = %q, want the agent's text", got)
	}

	s.Close()
	if s.Live() {
		t.Error("session still live after Close")
	}
}

func TestACPSessionNoticesAgentProcessExit(t *testing.T) {
	pinACPCatalog(t)
	t.Setenv(agentlaunch.ACPEnv, "1")
	t.Setenv("CHAT_FAKE_ACP_EXIT_AFTER_PROMPT", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	s, err := Start(ctx, "fakeacp", SessionOptions{
		Prompt: "exit after this turn",
		Cwd:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	if err := s.WaitIdle(ctx, 30*time.Second); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if got := s.Turn(); got != "ack:exit after this turn" {
		t.Fatalf("Turn = %q, want helper response before it exits", got)
	}

	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("ACP agent process exited, but Session.done never closed")
	}
	if s.Live() {
		t.Fatal("Live() = true after the ACP agent process exited")
	}
}

func TestACPSessionNoticesAgentDeathMidTurn(t *testing.T) {
	pinACPCatalog(t)
	t.Setenv(agentlaunch.ACPEnv, "1")
	t.Setenv("CHAT_FAKE_ACP_EXIT_DURING_PROMPT", "1")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s, err := Start(ctx, "fakeacp", SessionOptions{
		Prompt: "die during this turn",
		Cwd:    t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()

	if err := s.WaitIdle(ctx, 30*time.Second); err == nil {
		t.Fatal("WaitIdle succeeded after the ACP agent died mid-turn")
	}

	select {
	case <-s.done:
	case <-time.After(2 * time.Second):
		t.Fatal("ACP agent died mid-turn, but Session.done never closed")
	}
	if s.Live() {
		t.Fatal("Live() = true after the ACP agent died mid-turn")
	}
}

func TestNonACPLaunchesTakeExactlyTheOldPath(t *testing.T) {
	t.Run("gate off: even an ACP-declaring tool is untouched", func(t *testing.T) {
		pinACPCatalog(t)
		t.Setenv(agentlaunch.ACPEnv, "") // the default: opt-in, not opt-out

		s, mine, err := startACPSession(context.Background(), "fakeacp", SessionOptions{Prompt: "hi"})
		if mine || s != nil || err != nil {
			t.Fatalf("startACPSession = (%v, %v, %v), want (nil, false, nil)", s, mine, err)
		}
		if got := agentlaunch.EffectiveRung(acpToolLaunch(t)); got != agentlaunch.RungPTY {
			t.Errorf("EffectiveRung = %s, want %s", got, agentlaunch.RungPTY)
		}

		// And Start therefore reports what it reported before ACP existed: this
		// tool declares no steer_exec, so it cannot be steered.
		_, err = Start(context.Background(), "fakeacp", SessionOptions{Prompt: "hi"})
		if err == nil {
			t.Fatal("Start succeeded for a tool with no steerable launch")
		}
		if !strings.Contains(err.Error(), "cannot be steered") && !strings.Contains(err.Error(), "pty") {
			t.Errorf("Start error = %v, want the pre-ACP refusal", err)
		}
	})

	t.Run("gate on: a tool with no acp_exec is untouched", func(t *testing.T) {
		pinACPCatalog(t)
		t.Setenv(agentlaunch.ACPEnv, "1")

		// agy is the acceptance criterion by name: `agy -p` prints nothing and
		// exits 0 when stdout is not a TTY, so it MUST stay on rung 4.
		for _, name := range []string{"agy", "claude", "codex"} {
			s, mine, err := startACPSession(context.Background(), name, SessionOptions{Prompt: "hi"})
			if mine || s != nil || err != nil {
				t.Errorf("%s: startACPSession = (%v, %v, %v), want (nil, false, nil)", name, s, mine, err)
			}
		}
	})

	t.Run("gate on: an unknown tool is untouched", func(t *testing.T) {
		pinACPCatalog(t)
		t.Setenv(agentlaunch.ACPEnv, "1")

		s, mine, err := startACPSession(context.Background(), "no-such-tool-anywhere", SessionOptions{Prompt: "hi"})
		if mine || s != nil || err != nil {
			t.Fatalf("startACPSession = (%v, %v, %v), want (nil, false, nil)", s, mine, err)
		}
	})
}

func acpToolLaunch(t *testing.T) fleet.ToolLaunch {
	t.Helper()
	tool, ok := newCatalog().Tool("fakeacp")
	if !ok {
		t.Fatal("fakeacp is not in the pinned catalog")
	}
	return tool.CLI.Launch
}

// A BOUND MODEL REFUSES ACP — the routing-correctness guard.
//
// ACP carries no model-selection call, and the ACP-native tools take no model
// flag in ACP mode: `opencode acp --model moonshot/kimi-k3` prints its help
// instead of speaking the protocol. If a tool:model binding were driven over
// ACP anyway, every agent sharing that tool would exec the identical process,
// and their DIFFERENT BANDS would collapse into whatever model the install is
// configured for. `--min-band 3` would then be satisfied by the absence of any
// check, which is the fleet-evidence invariant inverted.
//
// So a bound model must fall to the rung below, which delivers it. This test
// exists because that refusal is invisible when it works: the session simply
// starts on the pty path and looks completely normal.
func TestBoundModelRefusesACPAndFallsToTheRungBelow(t *testing.T) {
	pinACPCatalog(t)
	t.Setenv(agentlaunch.ACPEnv, "1")

	if err := newCatalog().SaveModel(fleet.Model{
		Name:       "somemodel",
		Kind:       fleet.ModelKindSubscription,
		Provider:   "test",
		UpstreamID: "provider/somemodel",
	}); err != nil {
		t.Fatalf("SaveModel: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// The tool itself IS on rung 1 — this is not a tool that lacks acp_exec.
	tool, ok := newCatalog().Tool("fakeacp")
	if !ok {
		t.Fatal("fakeacp missing from the pinned catalog")
	}
	if got := agentlaunch.RungFor(tool.CLI.Launch); got != agentlaunch.RungACPNative {
		t.Fatalf("fakeacp is on rung %s, want acp-native — the test would prove nothing", got)
	}

	// Tool-only binding: ACP claims it.
	if _, mine, _ := startACPSession(ctx, "fakeacp", SessionOptions{Cwd: t.TempDir()}); !mine {
		t.Error("a tool-level binding was NOT driven over ACP; the guard is too broad")
	}

	// tool:model binding: ACP declines, so the caller falls to the rung below.
	if _, mine, err := startACPSession(ctx, "fakeacp:somemodel", SessionOptions{Cwd: t.TempDir()}); mine {
		t.Errorf("a bound model was driven over ACP, collapsing the binding (err=%v)", err)
	}
}
