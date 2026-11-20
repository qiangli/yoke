package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

// fakeTUISession scripts a live TUI session for `bashy chat command` unit tests
// without spawning a vendor process or PTY.
type fakeTUISession struct {
	mu     sync.Mutex
	events []string
	sent   []string
}

func (f *fakeTUISession) record(ev string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, ev)
}

func (f *fakeTUISession) WaitReadyBefore(context.Context) error {
	f.record("ready-before")
	return nil
}

func (f *fakeTUISession) SendVerbatim(line string) error {
	f.mu.Lock()
	f.events = append(f.events, "send:"+line)
	f.sent = append(f.sent, line)
	f.mu.Unlock()
	return nil
}

func (f *fakeTUISession) WaitReadyAfter(context.Context) error {
	f.record("ready-after")
	return nil
}

func (f *fakeTUISession) log() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.events...)
}

func isolateChatCommandTest(t *testing.T) *fleet.Catalog {
	t.Helper()
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	t.Setenv("BASHY_ROOM_DIR", filepath.Join(home, ".bashy", "room"))
	t.Setenv("BASHY_MB_DIR", filepath.Join(home, ".bashy", "mb"))
	t.Setenv("BASHY_MAILBOX_DIR", filepath.Join(home, ".bashy", "mailbox"))
	t.Setenv("BASHY_FLEET_DIR", filepath.Join(home, ".config", "bashy"))
	t.Setenv("BASHY_PRINCIPAL", "conductor-test")
	for _, k := range []string{"BASHY_AGENT_ID", "BASHY_AGENT", "WEAVE_AGENT", "WEAVE_CONDUCTOR"} {
		t.Setenv(k, "")
	}

	cat := fleet.New(fleet.WithRoot(filepath.Join(home, ".config", "bashy")), fleet.WithoutCloudOverlay())
	prevCat := newCatalog
	newCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { newCatalog = prevCat })
	return cat
}

func withFakeCommandSession(t *testing.T, f *fakeTUISession) {
	t.Helper()
	prev := openCommandSession
	openCommandSession = func(room.Card, commandDeliveryTiming, io.Writer) commandSession { return f }
	t.Cleanup(func() { openCommandSession = prev })
}

func TestChatCommandLookupAndRenderingOnFakeTUISession(t *testing.T) {
	isolateChatCommandTest(t)
	cases := []struct {
		id      string
		tool    string
		cmd     string
		args    []string
		want    string
		wantOrd []string
	}{
		{
			id: "claude-seat", tool: "claude", cmd: "rename", args: []string{"sprint-681"},
			want:    "/rename sprint-681",
			wantOrd: []string{"ready-before", "send:/rename sprint-681", "ready-after"},
		},
		{
			id: "claude-seat-clear", tool: "claude", cmd: "clear", args: nil,
			want:    "/clear",
			wantOrd: []string{"ready-before", "send:/clear", "ready-after"},
		},
		{
			id: "agy-seat", tool: "agy", cmd: "compact", args: []string{"keep", "auth", "decisions"},
			want:    "/compact keep auth decisions",
			wantOrd: []string{"ready-before", "send:/compact keep auth decisions", "ready-after"},
		},
		{
			id: "muse-seat", tool: "muse", cmd: "rename", args: []string{"worker-lane"},
			want:    "/name worker-lane",
			wantOrd: []string{"ready-before", "send:/name worker-lane", "ready-after"},
		},
		{
			id: "codex-seat", tool: "codex", cmd: "status", args: nil,
			want:    "/status",
			wantOrd: []string{"ready-before", "send:/status", "ready-after"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.tool+":"+tc.cmd, func(t *testing.T) {
			if err := room.Join(room.Card{
				ID: tc.id, Tool: tc.tool, Binding: tc.tool + ":model",
				Mode: "interactive", PID: os.Getpid(), CtlSock: "/tmp/fake.sock",
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { room.Leave(tc.id) })

			fake := &fakeTUISession{}
			withFakeCommandSession(t, fake)

			cmd := NewChatCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs(append([]string{"command", "--json", tc.id, tc.cmd}, tc.args...))
			if err := cmd.Execute(); err != nil {
				t.Fatalf("chat command %s %s: %v\nstderr=%s", tc.id, tc.cmd, err, stderr.String())
			}

			if got := fake.log(); !slices.Equal(got, tc.wantOrd) {
				t.Fatalf("session acts = %q, want %q", got, tc.wantOrd)
			}

			var env CommandEnvelope
			if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
				t.Fatalf("decode envelope: %v\n%s", err, stdout.String())
			}
			if env.SchemaVersion != commandSchemaVersion || env.Outcome != "ok" || env.ExitCode != 0 ||
				env.Session != tc.id || env.Tool != tc.tool || env.Command != tc.cmd || env.Slash != tc.want {
				t.Fatalf("envelope = %+v", env)
			}
		})
	}
}

func TestChatCommandSendsVerbatimWithoutPrependingQueuedInboxOrSteer(t *testing.T) {
	isolateChatCommandTest(t)
	srv := newCtlSockServer(t)
	logPath := filepath.Join(t.TempDir(), "session.log")
	if err := os.WriteFile(logPath, []byte("❯ "), 0o600); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-2 * time.Second)
	_ = os.Chtimes(logPath, past, past)

	const id = "claude-live-worker"
	const steerAddr = "steer-claude-live-worker"
	card := room.Card{
		ID:      id,
		Nick:    id,
		Tool:    "claude",
		Binding: "claude:opus5.5",
		Mode:    "interactive",
		PID:     os.Getpid(),
		CtlSock: srv.path,
		LogPath: logPath,
		Caps:    []string{room.CapInboxDelivery, SteerInboxPrefix + steerAddr},
	}
	if err := room.Join(card); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { room.Leave(card.ID) })

	// Queue both a unified-inbox message AND a steer-inbox item before running
	// `chat command`. Neither may be prepended or drained by `chat command`.
	if err := bus.PostMessage(bus.Post{From: "manager", To: id, Body: "UNREAD_INBOX_BODY"}); err != nil {
		t.Fatal(err)
	}
	if err := bus.Publish(bus.Notification{Principal: "manager", To: srv.path, Body: "UNREAD_SOCKET_PREAMBLE"}); err != nil {
		t.Fatal(err)
	}
	if rec, err := Steer(id, "manager", "QUEUED_STEER_LINE"); err != nil || !rec.Queued {
		t.Fatalf("seed queued steer: rec=%+v err=%v", rec, err)
	}

	fastTiming := commandDeliveryTiming{
		poll:             5 * time.Millisecond,
		socketTimeout:    200 * time.Millisecond,
		settle:           15 * time.Millisecond,
		readyTimeout:     200 * time.Millisecond,
		responseGrace:    20 * time.Millisecond,
		postReadyTimeout: 200 * time.Millisecond,
	}
	env, err := runSessionCommandWith(context.Background(), newCatalog(), id, "clear", nil, fastTiming, 0, false, io.Discard)
	if err != nil {
		t.Fatalf("runSessionCommandWith: %v", err)
	}
	if env.Slash != "/clear" || env.Outcome != "ok" {
		t.Fatalf("envelope = %+v", env)
	}

	wire := string(settle(srv))
	wantWire := agentpty.TextFrame("/clear")
	if wire != wantWire {
		t.Fatalf("wire frame = %q, want verbatim %q (no inbox prepend or queued steer merge)", wire, wantWire)
	}
	for _, forbidden := range []string{"UNREAD_INBOX_BODY", "UNREAD_SOCKET_PREAMBLE", "QUEUED_STEER_LINE"} {
		if strings.Contains(wire, forbidden) {
			t.Fatalf("command wire payload leaked %q: %q", forbidden, wire)
		}
	}

	// Both queued items remain unread in their stores.
	if directed, _, _, err := bus.Unseen(id, 10); err != nil || len(directed) != 1 || directed[0].Body != "UNREAD_INBOX_BODY" {
		t.Fatalf("agent board mail altered: %+v, %v", directed, err)
	}
	if steers, _, _, err := bus.Unseen(steerAddr, 10); err != nil || len(steers) != 1 || steers[0].Body != "QUEUED_STEER_LINE" {
		t.Fatalf("steer inbox altered: %+v, %v", steers, err)
	}
}

func TestChatCommandRefusalExitsTwoAndListsDeclaredNames(t *testing.T) {
	cat := isolateChatCommandTest(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "baretool",
		Kind: fleet.ToolKindCLI,
		CLI:  fleet.ToolCLI{Launch: fleet.ToolLaunch{Exec: "baretool {prompt}", SteerExec: "baretool"}},
	}); err != nil {
		t.Fatal(err)
	}

	if err := room.Join(room.Card{
		ID: "agy-live", Tool: "agy", Binding: "agy:gemini4-carbon",
		Mode: "interactive", PID: os.Getpid(), CtlSock: "/tmp/agy.sock",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { room.Leave("agy-live") })

	if err := room.Join(room.Card{
		ID: "bare-live", Tool: "baretool", Binding: "baretool:m",
		Mode: "interactive", PID: os.Getpid(), CtlSock: "/tmp/bare.sock",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { room.Leave("bare-live") })

	fake := &fakeTUISession{}
	withFakeCommandSession(t, fake)

	t.Run("undeclared command on agy lists declared tui commands", func(t *testing.T) {
		cmd := NewChatCmd()
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"command", "--json", "agy-live", "status"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("expected refusal for undeclared agy:status")
		}
		if got := ExitCode(err); got != 2 {
			t.Fatalf("ExitCode(err) = %d, want 2 (err=%v)", got, err)
		}
		for _, want := range []string{"clear", "compact", "exit", "rename", "resume"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("refusal %q does not list declared command %q", err.Error(), want)
			}
		}
		if strings.Contains(err.Error(), "video") {
			t.Errorf("refusal %q listed print-only command video", err.Error())
		}
		var env CommandEnvelope
		if jerr := json.Unmarshal(stdout.Bytes(), &env); jerr != nil {
			t.Fatalf("decode refusal envelope: %v\n%s", jerr, stdout.String())
		}
		if env.ExitCode != 2 || env.Outcome != "refused" || !slices.Equal(env.Declared, []string{"clear", "compact", "exit", "rename", "resume"}) {
			t.Fatalf("refusal envelope = %+v", env)
		}
	})

	t.Run("tool declaring no tui commands exits 2", func(t *testing.T) {
		cmd := NewChatCmd()
		var stdout, stderr bytes.Buffer
		cmd.SetOut(&stdout)
		cmd.SetErr(&stderr)
		cmd.SetArgs([]string{"command", "bare-live", "clear"})
		err := cmd.Execute()
		if err == nil {
			t.Fatal("expected refusal for tool declaring nothing")
		}
		if got := ExitCode(err); got != 2 {
			t.Fatalf("ExitCode(err) = %d, want 2 (err=%v)", got, err)
		}
		if !strings.Contains(err.Error(), "declares no tui session commands") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	if len(fake.log()) != 0 {
		t.Fatalf("refused command touched live session: %q", fake.log())
	}
}

func TestChatCommandRefusesClearAndExitOnCallersOwnSessionWithoutForce(t *testing.T) {
	isolateChatCommandTest(t)

	if err := room.Join(room.Card{
		ID: "manager-seat", Nick: "manager-seat", Tool: "claude", Binding: "claude:opus5.5",
		Mode: "interactive", PID: os.Getpid(), CtlSock: "/tmp/manager.sock",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { room.Leave("manager-seat") })

	t.Run("env identity refuses clear and exit until --force", func(t *testing.T) {
		t.Setenv("BASHY_PRINCIPAL", "dhnt:agent/manager-seat")
		t.Setenv("BASHY_AGENT_ID", "manager-seat")

		fake := &fakeTUISession{}
		withFakeCommandSession(t, fake)

		for _, destructive := range []string{"clear", "exit"} {
			cmd := NewChatCmd()
			var stdout, stderr bytes.Buffer
			cmd.SetOut(&stdout)
			cmd.SetErr(&stderr)
			cmd.SetArgs([]string{"command", "--json", "manager-seat", destructive})
			err := cmd.Execute()
			if err == nil {
				t.Fatalf("expected refusal for self-targeted %s", destructive)
			}
			if got := ExitCode(err); got != 2 {
				t.Fatalf("%s ExitCode = %d, want 2 (err=%v)", destructive, got, err)
			}
			if !strings.Contains(err.Error(), "--force") {
				t.Fatalf("%s refusal %q does not mention --force", destructive, err.Error())
			}
		}
		if len(fake.log()) != 0 {
			t.Fatalf("refused self commands touched live session: %q", fake.log())
		}

		// Non-destructive commands on the caller's own session remain allowed without --force.
		statusCmd := NewChatCmd()
		statusCmd.SetOut(&bytes.Buffer{})
		statusCmd.SetErr(&bytes.Buffer{})
		statusCmd.SetArgs([]string{"command", "manager-seat", "status"})
		if err := statusCmd.Execute(); err != nil {
			t.Fatalf("self status without --force: %v", err)
		}

		// Explicit --force overrides the self-session guard.
		forceCmd := NewChatCmd()
		forceCmd.SetOut(&bytes.Buffer{})
		forceCmd.SetErr(&bytes.Buffer{})
		forceCmd.SetArgs([]string{"command", "--force", "manager-seat", "clear"})
		if err := forceCmd.Execute(); err != nil {
			t.Fatalf("self clear with --force: %v", err)
		}

		wantActs := []string{
			"ready-before", "send:/status", "ready-after",
			"ready-before", "send:/clear", "ready-after",
		}
		if got := fake.log(); !slices.Equal(got, wantActs) {
			t.Fatalf("session acts = %q, want %q", got, wantActs)
		}
	})

	if ppid := os.Getppid(); ppid > 1 && ppid != os.Getpid() {
		t.Run("room card process ancestry refuses clear without --force", func(t *testing.T) {
			if err := room.Join(room.Card{
				ID: "ancestry-seat", Tool: "codex", Binding: "codex:gpt6-sol",
				Mode: "interactive", PID: os.Getpid(), OwnerPID: ppid, CtlSock: "/tmp/ancestry.sock",
			}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { room.Leave("ancestry-seat") })

			fake := &fakeTUISession{}
			withFakeCommandSession(t, fake)

			cmd := NewChatCmd()
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs([]string{"command", "ancestry-seat", "clear"})
			err := cmd.Execute()
			if err == nil || ExitCode(err) != 2 || !strings.Contains(err.Error(), "--force") {
				t.Fatalf("ancestry self clear = %v (code=%d), want exit 2 mentioning --force", err, ExitCode(err))
			}
			if len(fake.log()) != 0 {
				t.Fatalf("ancestry refusal touched session: %q", fake.log())
			}
		})
	}
}

func TestChatCommandRecordsTimelineEvent(t *testing.T) {
	isolateChatCommandTest(t)
	if err := room.Join(room.Card{
		ID: "codex-worker", Tool: "codex", Binding: "codex:gpt6-sol",
		Mode: "interactive", PID: os.Getpid(), CtlSock: "/tmp/codex.sock",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { room.Leave("codex-worker") })

	fake := &fakeTUISession{}
	withFakeCommandSession(t, fake)

	cmd := NewChatCmd()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"command", "codex-worker", "rename", "s681-thread"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}

	events, err := room.Timeline(20)
	if err != nil {
		t.Fatal(err)
	}
	var got *room.Event
	for i := range events {
		if events[i].Type == room.EventCommand {
			got = &events[i]
		}
	}
	if got == nil {
		t.Fatalf("no command event recorded in timeline: %+v", events)
	}
	if got.Actor != "conductor-test" || got.Target != "codex-worker" || got.Topic != "rename" || got.Body != "/rename s681-thread" {
		t.Fatalf("command event = %+v", *got)
	}
}

func TestChatSessionsJSONIncludesDeclaredCommands(t *testing.T) {
	cat := isolateChatCommandTest(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "plain",
		Kind: fleet.ToolKindCLI,
		CLI:  fleet.ToolCLI{Launch: fleet.ToolLaunch{Exec: "plain {prompt}", SteerExec: "plain"}},
	}); err != nil {
		t.Fatal(err)
	}

	sock := filepath.Join(t.TempDir(), "live.sock")
	if err := os.WriteFile(sock, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct{ id, tool string }{
		{"s-claude", "claude"},
		{"s-agy", "agy"},
		{"s-muse", "muse"},
		{"s-codex", "codex"},
		{"s-plain", "plain"},
	} {
		if err := room.Join(room.Card{
			ID: tc.id, Tool: tc.tool, Binding: tc.tool + ":m",
			Mode: "interactive", PID: os.Getpid(), CtlSock: sock,
			Joined: time.Now().UTC().Add(time.Duration(i) * time.Second).Format(time.RFC3339),
		}); err != nil {
			t.Fatal(err)
		}
		id := tc.id
		t.Cleanup(func() { room.Leave(id) })
	}

	cmd := NewChatCmd()
	var stdout bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{"sessions", "--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("sessions --json: %v", err)
	}

	var env SessionsEnvelope
	if err := json.Unmarshal(stdout.Bytes(), &env); err != nil {
		t.Fatalf("decode sessions json: %v\n%s", err, stdout.String())
	}
	if env.SchemaVersion != sessionsSchemaVersion || len(env.Sessions) != 5 {
		t.Fatalf("envelope = %+v", env)
	}
	byID := map[string][]string{}
	for _, s := range env.Sessions {
		if s.Commands == nil {
			t.Fatalf("session %s commands marshaled as null instead of []", s.ID)
		}
		byID[s.ID] = s.Commands
	}
	want := map[string][]string{
		"s-claude": {"clear", "compact", "exit", "rename", "resume", "status"},
		"s-agy":    {"clear", "compact", "exit", "rename", "resume"},
		"s-muse":   {"clear", "compact", "exit", "rename", "resume", "status"},
		"s-codex":  {"clear", "compact", "exit", "rename", "resume", "status"},
		"s-plain":  {},
	}
	for id, wantCmds := range want {
		if !slices.Equal(byID[id], wantCmds) {
			t.Errorf("%s commands = %q, want %q", id, byID[id], wantCmds)
		}
	}
}

func TestSessionQuitUsesDeclaredExitAndFallsBackToQuit(t *testing.T) {
	cat := isolateChatCommandTest(t)
	if err := cat.SaveTool(fleet.Tool{
		Name: "customexit",
		Kind: fleet.ToolKindCLI,
		CLI:  fleet.ToolCLI{Launch: fleet.ToolLaunch{Exec: "customexit {prompt}", SteerExec: "customexit"}},
		Commands: []fleet.ToolCommand{
			{Name: "exit", Slash: "/bye-now", Mode: fleet.ToolCommandTUI},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveTool(fleet.Tool{
		Name: "undeclaredexit",
		Kind: fleet.ToolKindCLI,
		CLI:  fleet.ToolCLI{Launch: fleet.ToolLaunch{Exec: "undeclaredexit {prompt}", SteerExec: "undeclaredexit"}},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		tool string
		want string
	}{
		{"claude", agentpty.TextFrame("/exit")},
		{"customexit", agentpty.TextFrame("/bye-now")},
		{"undeclaredexit", agentpty.TextFrame("/quit")},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			srv := newCtlSockServer(t)
			// Pending notification on the control socket must NOT be prepended to Quit.
			if err := bus.Publish(bus.Notification{Principal: "op", To: srv.path, Body: "DO_NOT_PREPEND"}); err != nil {
				t.Fatal(err)
			}
			s := &Session{
				Agent:   tc.tool + ":m",
				Nick:    tc.tool,
				CtlSock: srv.path,
				catalog: cat,
				launch:  Launch{ToolName: tc.tool},
				done:    make(chan struct{}),
			}
			if err := s.Quit(); err != nil {
				t.Fatalf("Quit(%s): %v", tc.tool, err)
			}
			if got := string(settle(srv)); got != tc.want {
				t.Fatalf("Quit(%s) wire = %q, want %q", tc.tool, got, tc.want)
			}
		})
	}
}
