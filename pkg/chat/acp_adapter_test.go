package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/acp"
	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/ask"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/room"
)

var updateACPGolden = flag.Bool("update-acp-golden", false, "rewrite testdata/acp-events.golden.json")

// captureACPEvents replaces the host-log append with a recorder.
func captureACPEvents(t *testing.T) func() []room.Event {
	t.Helper()
	var (
		mu  sync.Mutex
		got []room.Event
	)
	prev := acpEmit
	acpEmit = func(e room.Event) error {
		mu.Lock()
		defer mu.Unlock()
		got = append(got, e)
		return nil
	}
	t.Cleanup(func() { acpEmit = prev })
	return func() []room.Event {
		mu.Lock()
		defer mu.Unlock()
		return append([]room.Event(nil), got...)
	}
}

func testACPDriver() (*acpDriver, *strings.Builder) {
	var sink strings.Builder
	return &acpDriver{sink: &sink, end: make(chan acpTurn, 1), card: "claude-opus", principal: "alice", session: "sess-1"}, &sink
}

// TestACPUpdatesMapToEntryEvents is the adapter golden for the S3 entry event
// contract: every update kind the adapters send — tool_call, its updates, plan,
// thought, usage and permission — lands on the host log as one typed entry
// carrying the session's identity, and only the answer reaches the transcript.
func TestACPUpdatesMapToEntryEvents(t *testing.T) {
	events := captureACPEvents(t)
	d, sink := testACPDriver()
	h := newACPHandler(d, "")
	ctx := context.Background()
	send := func(u acp.SessionUpdate) {
		if err := h.SessionUpdate(ctx, acp.SessionNotification{SessionID: "sess-1", Update: u}); err != nil {
			t.Fatal(err)
		}
	}

	send(acp.SessionUpdate{AgentThoughtChunk: &acp.ContentBlock{Text: "Look at the "}})
	send(acp.SessionUpdate{AgentThoughtChunk: &acp.ContentBlock{Text: "failing test first."}})
	send(acp.SessionUpdate{Plan: &acp.Plan{Entries: []acp.PlanEntry{
		{Content: "reproduce", Priority: "high", Status: "completed"},
		{Content: "fix", Priority: "medium", Status: "in_progress"},
	}}})
	send(acp.SessionUpdate{ToolCall: &acp.ToolCall{ID: "t1", Title: "go test ./pkg/x", Kind: "execute", Status: acp.ToolCallPending,
		Locations: []acp.ToolCallLocation{{Path: "pkg/x/x_test.go", Line: 12}}}})
	if _, err := h.RequestPermission(ctx, acp.PermissionRequest{SessionID: "sess-1",
		ToolCall: acp.ToolCall{ID: "t1", Title: "go test ./pkg/x", Kind: "execute"},
		Options: []acp.PermissionOption{{ID: "ao", Kind: acp.PermissionAllowOnce}, {ID: "ro", Kind: acp.PermissionRejectOnce}},
	}); err != nil {
		t.Fatal(err)
	}
	send(acp.SessionUpdate{ToolCallUpdate: &acp.ToolCall{ID: "t1", Status: acp.ToolCallCompleted}})
	send(acp.SessionUpdate{AgentMessageChunk: &acp.ContentBlock{Text: "fixed"}})
	send(acp.SessionUpdate{Usage: &acp.Usage{Used: 1200, Size: 200000, CostAmount: 0.25, CostCurrency: "USD"}})
	send(acp.SessionUpdate{AgentThoughtChunk: &acp.ContentBlock{Text: "done"}})
	d.flushThought() // the turn ending flushes trailing reasoning

	if sink.String() != "fixed" {
		t.Errorf("transcript = %q, want only the answer", sink.String())
	}
	got, err := json.MarshalIndent(events(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	golden := filepath.Join("testdata", "acp-events.golden.json")
	if *updateACPGolden {
		if err := os.WriteFile(golden, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden (run with -update-acp-golden): %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("ACP entry events changed.\n--- want\n%s\n--- got\n%s", want, got)
	}
	for _, e := range events() {
		if e.Actor != "claude-opus" || e.Principal != "alice" || e.To != "sess-1" {
			t.Errorf("entry lost session identity: %+v", e)
		}
	}
}

// A permission request in ask mode reaches the ONE approval queue typed (kind,
// title, offered options) and keyed by the session's room card — the key
// `chat grant` and `bashy approve` settle — and the decision is logged too.
func TestACPPermissionKeepsIdentityForApprovalQueue(t *testing.T) {
	t.Setenv(ask.DirEnv, t.TempDir())
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	events := captureACPEvents(t)
	d, _ := testACPDriver()
	h := newACPHandler(d, "ask")

	done := make(chan acp.PermissionResponse, 1)
	go func() {
		got, _ := h.RequestPermission(context.Background(), chatPermReq())
		done <- got
	}()
	var pending []ask.Request
	deadline := time.Now().Add(5 * time.Second)
	for len(pending) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no approval raised")
		}
		time.Sleep(10 * time.Millisecond)
		pending, _ = ask.PendingForSession("claude-opus")
	}
	a := pending[0].Approval
	if a.Source != ask.SourceACP || a.Session != "claude-opus" || a.Mode != "ask" {
		t.Errorf("approval identity = source %q session %q mode %q", a.Source, a.Session, a.Mode)
	}
	if strings.Join(a.Argv, " ") != "acp:execute run tests" {
		t.Errorf("approval argv = %q, want the typed tool call", a.Argv)
	}
	kinds := []string{}
	for _, o := range a.Options {
		kinds = append(kinds, o.Kind)
	}
	if strings.Join(kinds, ",") != "allow_once,allow_always,reject_once" {
		t.Errorf("approval options = %v, want the typed options offered", kinds)
	}
	if _, err := ask.Grant("claude-opus", "human", false, nil); err != nil {
		t.Fatal(err)
	}
	if got := <-done; got.OptionID != "ao" {
		t.Fatalf("grant once -> %+v", got)
	}
	ev := events()
	if len(ev) != 1 || ev[0].Topic != acpTopicPermission || !strings.Contains(ev[0].Body, "-> allow_once") || ev[0].Actor != "claude-opus" {
		t.Errorf("permission entry = %+v", ev)
	}
}

func adapterTool(bin string, args ...string) fleet.Tool {
	return fleet.Tool{
		Name: "fakeacp", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{
			Binary: "fakeacp-tool-not-installed",
			Launch: fleet.ToolLaunch{
				Exec:    "fakeacp-tool-not-installed -run {prompt}",
				ACPExec: strings.Join(append([]string{bin}, args...), " "),
				ACPAdapter: &fleet.ToolACPAdapter{
					ID: "fakeacp", Source: "npm", Package: "@test/fakeacp", Version: "1.2.3",
					License: "MIT", LicenseURL: "https://example.com/LICENSE",
					Bin: bin, Args: args,
					Tarball: fleet.ToolACPAdapterTarball{
						URL:    "https://registry.npmjs.org/@test/fakeacp/-/fakeacp-1.2.3.tgz",
						SHA256: strings.Repeat("0", 64),
					},
				},
			},
		},
	}
}

// fakeNPMInstall lays out a global npm install: PATH/bin -> node_modules/pkg.
func fakeNPMInstall(t *testing.T, version string) string {
	t.Helper()
	root := t.TempDir()
	pkg := filepath.Join(root, "lib", "node_modules", "@test", "fakeacp")
	if err := os.MkdirAll(filepath.Join(pkg, "dist"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(pkg, "package.json"), []byte(`{"name":"@test/fakeacp","version":"`+version+`"}`), 0o644)
	script := filepath.Join(pkg, "dist", "index.js")
	_ = os.WriteFile(script, []byte("#!/bin/sh\n"), 0o755)
	bin := filepath.Join(root, "bin")
	_ = os.MkdirAll(bin, 0o755)
	if err := os.Symlink(script, filepath.Join(bin, "fakeacp-adapter")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	return bin
}

func TestCheckACPAdapterAvailability(t *testing.T) {
	t.Run("pinned release installed", func(t *testing.T) {
		t.Setenv("PATH", fakeNPMInstall(t, "1.2.3"))
		st := CheckACPAdapter(adapterTool("fakeacp-adapter"))
		if !st.Available || !st.Verified || st.Installed != "1.2.3" || st.Native {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("other release installed", func(t *testing.T) {
		t.Setenv("PATH", fakeNPMInstall(t, "1.2.4"))
		st := CheckACPAdapter(adapterTool("fakeacp-adapter"))
		if st.Available || !strings.Contains(st.Reason, "pinned 1.2.3") {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("not installed", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		t.Setenv("HOME", t.TempDir())
		st := CheckACPAdapter(adapterTool("fakeacp-adapter-missing"))
		if st.Available || !strings.Contains(st.Reason, "npm install -g @test/fakeacp@1.2.3") {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("runtime missing", func(t *testing.T) {
		t.Setenv("PATH", fakeNPMInstall(t, "1.2.3"))
		tool := adapterTool("fakeacp-adapter")
		tool.CLI.Launch.ACPAdapter.Requires = []string{"node-not-on-this-path"}
		if st := CheckACPAdapter(tool); st.Available || !strings.Contains(st.Reason, "needs node-not-on-this-path") {
			t.Errorf("status = %+v", st)
		}
	})
	t.Run("invalid pin", func(t *testing.T) {
		tool := adapterTool("fakeacp-adapter")
		tool.CLI.Launch.ACPAdapter.Version = "latest"
		if st := CheckACPAdapter(tool); st.Available {
			t.Errorf("an unpinned adapter was available: %+v", st)
		}
	})
	t.Run("seeds", func(t *testing.T) {
		// The three seeds are well-formed pins; whether this host has them
		// installed is the manager's live gate, not this one.
		cat := fleet.New(fleet.WithoutLocalStore(), fleet.WithoutCloudOverlay())
		for _, name := range []string{"claude", "codex", "gemini"} {
			tool, ok := cat.Tool(name)
			if !ok {
				t.Fatalf("seed %s missing", name)
			}
			st := CheckACPAdapter(tool)
			if st.Spec == "" || (!st.Available && strings.Contains(st.Reason, "acp_adapter:")) {
				t.Errorf("%s: %+v", name, st)
			}
			if st.Native != (name == "gemini") {
				t.Errorf("%s: native = %v", name, st.Native)
			}
		}
	})
}

// pinACPAdapterCatalog installs a tool whose OWN binary does not exist and
// whose pinned adapter is the fake ACP agent. A session that answers can only
// have been driven through the adapter.
func pinACPAdapterCatalog(t *testing.T, adapterBin string) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("BASHY_FORCE_AGENT_SHELL", "0")
	t.Setenv("CHAT_FAKE_ACP_AGENT", "1")
	root := t.TempDir()
	prev := newCatalog
	newCatalog = func() *fleet.Catalog { return fleet.New(fleet.WithRoot(root)) }
	t.Cleanup(func() { newCatalog = prev })
	if err := newCatalog().SaveTool(adapterTool(adapterBin, "-test.run=^TestFakeACPAgentHelper$")); err != nil {
		t.Fatalf("SaveTool: %v", err)
	}
}

func TestACPSessionRunsThePinnedAdapter(t *testing.T) {
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(self, " \t") {
		t.Skip("test binary path has whitespace")
	}
	pinACPAdapterCatalog(t, self)
	t.Setenv(agentlaunch.ACPEnv, "1")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Start(ctx, "fakeacp", SessionOptions{Prompt: "via adapter", Cwd: t.TempDir()})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer s.Close()
	if err := s.WaitIdle(ctx, 30*time.Second); err != nil {
		t.Fatalf("WaitIdle: %v", err)
	}
	if got := s.Turn(); got != "ack:via adapter" {
		t.Errorf("Turn = %q", got)
	}
}

func TestACPSessionWithUnavailableAdapterFallsThrough(t *testing.T) {
	pinACPAdapterCatalog(t, "fakeacp-adapter-not-installed")
	t.Setenv(agentlaunch.ACPEnv, "1")
	t.Setenv("PATH", t.TempDir())
	s, ok, err := startACPSession(context.Background(), "fakeacp", SessionOptions{Cwd: t.TempDir()})
	if ok || err != nil || s != nil {
		t.Fatalf("startACPSession = %v, %v, %v; want not-mine so the next rung runs", s, ok, err)
	}
}
