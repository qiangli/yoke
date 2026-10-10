package cligw

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"
	"time"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

func TestCLIHelper(t *testing.T) {
	if os.Getenv("CLIGW_TEST_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(90)
	}
	args = args[1:]
	switch args[0] {
	case "backend-text":
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"hello from cli"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":3}}`)
	case "backend-burst":
		for i := 0; i < 96; i++ {
			fmt.Printf("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":%q}}\n", fmt.Sprintf("chunk-%03d ", i))
		}
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":11,"output_tokens":96}}`)
	case "backend-stream":
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"hello "}}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"world"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":8,"output_tokens":2}}`)
	case "claude-partials":
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}}`)
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`)
		fmt.Println(`{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":1}}`)
	case "system-prompt-claude", "system-prompt-codex", "system-prompt-agy", "system-prompt-muse", "system-prompt-ycode":
		body, _ := io.ReadAll(os.Stdin)
		instructions := ""
		for i, arg := range args {
			if arg == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], "model_instructions_file=") {
				path, _ := strconv.Unquote(strings.TrimPrefix(args[i+1], "model_instructions_file="))
				contents, _ := os.ReadFile(path)
				instructions = string(contents)
			}
		}
		capture, _ := json.Marshal(map[string]any{"args": args, "body": string(body), "instructions": instructions, "genie_effort": os.Getenv("GENIE_EFFORT")})
		_ = os.WriteFile(os.Getenv("CLIGW_CAPTURE_PATH"), capture, 0o600)
		switch strings.TrimPrefix(args[0], "system-prompt-") {
		case "muse":
			fmt.Println(`{"payload_type":"run.terminal.completed","payload":{"terminal":"completed","text":"ok"}}`)
		case "claude":
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`)
			fmt.Println(`{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}`)
		case "agy":
			fmt.Println(`{"event":"delta","delta":{"text":"ok"}}`)
			fmt.Println(`{"event":"result","result":{"status":"SUCCESS"},"usage":{"input_tokens":1,"output_tokens":1}}`)
		case "codex", "ycode":
			fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`)
			fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	case "backend-tool":
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"{\"tool_calls\":[{\"name\":\"weather\",\"arguments\":{\"city\":\"Paris\"}}]}"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":9,"output_tokens":7}}`)
	case "muse-fixture", "response-fixture":
		// A real Muse Code 1.3.0 `exec --json` stream, scrubbed (testdata).
		body, err := os.ReadFile(os.Getenv("CLIGW_MUSE_FIXTURE"))
		if err != nil {
			os.Exit(95)
		}
		os.Stdout.Write(body)
	case "muse-terminal-only":
		fmt.Println(`{"schema_version":1,"payload_type":"run.lifecycle.started","payload":{"kind":"run_started"}}`)
		fmt.Println(`{"schema_version":1,"payload_type":"run.terminal.completed","payload":{"kind":"run_terminal","terminal":"completed","text":"only terminal","reason":null}}`)
	case "muse-tool":
		fmt.Println(`{"schema_version":1,"payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":"Calling a tool.\n{\"tool_calls\":[{\"name\":\"weather\","}}`)
		fmt.Println(`{"schema_version":1,"payload_type":"run.output.delta","payload":{"kind":"run_output_delta","text":"\"arguments\":{\"city\":\"Paris\"}}]}"}}`)
		fmt.Println(`{"schema_version":1,"payload_type":"run.terminal.completed","payload":{"kind":"run_terminal","terminal":"completed","text":"Calling a tool.\n{\"tool_calls\":[{\"name\":\"weather\",\"arguments\":{\"city\":\"Paris\"}}]}","reason":null}}`)
	case "muse-failed":
		fmt.Println(`{"schema_version":1,"payload_type":"run.terminal.failed","payload":{"kind":"run_terminal","terminal":"failed","text":null,"reason":"billing_error"}}`)
		os.Exit(1)
	case "backend-crash":
		fmt.Fprintln(os.Stderr, "fake backend crash")
		os.Exit(7)
	case "backend-cancel":
		if path := os.Getenv("CLIGW_CANCEL_STARTED"); path != "" {
			_ = os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())), 0o600)
		}
		time.Sleep(time.Minute)
	case "warm":
		body, err := io.ReadAll(os.Stdin)
		if err != nil {
			os.Exit(91)
		}
		var message struct {
			Type    string `json:"type"`
			Message struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(body, &message) != nil || message.Type != "user" || message.Message.Role != "user" {
			os.Exit(92)
		}
		fmt.Printf("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", "answer:"+message.Message.Content)
		fmt.Println(`{"type":"result","is_error":false,"usage":{"input_tokens":7,"output_tokens":2}}`)
	case "cold":
		prompt := args[len(args)-1]
		fmt.Printf("{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":%q}}\n", "answer:"+prompt)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":4,"cached_input_tokens":1,"output_tokens":2}}`)
	case "sticky-loop":
		// A stdin-stream-json CLI that stays up across turns: one NDJSON
		// user line in, one assistant event plus a terminal result out.
		// The pid prefix proves two turns reached the same process.
		scan := bufio.NewScanner(os.Stdin)
		scan.Buffer(make([]byte, 64<<10), 4<<20)
		for scan.Scan() {
			line := append([]byte(nil), scan.Bytes()...)
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var message struct {
				Type    string `json:"type"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &message) != nil || message.Message.Role != "user" {
				os.Exit(92)
			}
			fmt.Printf("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", strconv.Itoa(os.Getpid())+":"+message.Message.Content)
			fmt.Println(`{"type":"result","is_error":false,"usage":{"input_tokens":7,"output_tokens":2}}`)
		}
	case "sticky-partials":
		// Claude's wire shape across turns: text deltas as stream_event,
		// then the full assistant snapshot, then the terminal result. A
		// session must not duplicate the snapshot into the turn text.
		scan := bufio.NewScanner(os.Stdin)
		scan.Buffer(make([]byte, 64<<10), 4<<20)
		for scan.Scan() {
			line := append([]byte(nil), scan.Bytes()...)
			if len(bytes.TrimSpace(line)) == 0 {
				continue
			}
			var message struct {
				Type    string `json:"type"`
				Message struct {
					Role    string `json:"role"`
					Content string `json:"content"`
				} `json:"message"`
			}
			if json.Unmarshal(line, &message) != nil || message.Message.Role != "user" {
				os.Exit(92)
			}
			fmt.Printf("{\"type\":\"stream_event\",\"event\":{\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":%q}}}\n", "half:"+message.Message.Content)
			fmt.Printf("{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":%q}]}}\n", "half:"+message.Message.Content)
			fmt.Println(`{"type":"result","is_error":false,"usage":{"input_tokens":7,"output_tokens":2}}`)
		}
	case "crash":
		fmt.Fprintln(os.Stderr, "fake crash")
		os.Exit(7)
	case "group":
		pidFile := args[len(args)-1]
		if _, tail, ok := strings.Cut(pidFile, "\n\n"); ok {
			pidFile = tail
		}
		child := exec.Command(os.Args[0], "-test.run=TestCLIHelper", "--", "child")
		child.Env = os.Environ()
		if child.Start() != nil {
			os.Exit(93)
		}
		if os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600) != nil {
			os.Exit(94)
		}
		time.Sleep(time.Minute)
	case "child":
		time.Sleep(time.Minute)
	}
}

func TestWorkerWarmReceivesOneJSONPrompt(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "warm")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if w.StartedAt().IsZero() {
		t.Fatal("warm worker was not started before Do")
	}
	var events []Event
	got, err := w.Do(context.Background(), "hello", func(ev Event) { events = append(events, ev) })
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "answer:hello" || got.Outcome != OutcomeOK {
		t.Fatalf("result = %+v", got)
	}
	if got.Usage.InputTokens != 7 || got.Usage.OutputTokens != 2 || got.Usage.Estimated {
		t.Fatalf("usage = %+v", got.Usage)
	}
	if len(events) != 2 || !events[1].Done {
		t.Fatalf("events = %+v", events)
	}
	if _, err := w.Do(context.Background(), "again", nil); err == nil {
		t.Fatal("one-shot worker accepted a second Do")
	}
}

func TestWorkerColdStartsAtDo(t *testing.T) {
	installFakeCatalog(t, "fakecold", WarmCold, "cold")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if !w.StartedAt().IsZero() {
		t.Fatal("cold worker started before Do")
	}
	got, err := w.Do(context.Background(), "hello cold", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "answer:System:\n"+neutralSystemPrompt+"\n\nhello cold" || got.Usage.CachedInputTokens != 1 {
		t.Fatalf("result = %+v", got)
	}
}

func TestWorkerCrashReturnsErrorOutcome(t *testing.T) {
	installFakeCatalog(t, "fakecrash", WarmCold, "crash")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Do(context.Background(), "boom", nil)
	if err == nil || got.Outcome != OutcomeError || !strings.Contains(err.Error(), "exit status 7") {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestMeasuredWarmArgv(t *testing.T) {
	tests := []struct {
		name string
		w    *Worker
		want []string
	}{
		{
			name: "claude",
			w:    &Worker{mode: WarmStdinStreamJSON, launch: agentlaunch.Launch{Tool: "claude", Args: []string{"--model", "M", "-p"}}, tool: fleet.Tool{Name: "claude", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--output-format stream-json --verbose"}}}},
			want: []string{"claude", "-p", "--model", "M", "--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages", "--system-prompt", neutralSystemPrompt, "--tools", "", "--no-session-persistence", "--strict-mcp-config", "--setting-sources", ""},
		},
		{
			name: "agy",
			w:    &Worker{mode: WarmStdinStreamJSON, launch: agentlaunch.Launch{Tool: "agy", Args: []string{"--print-timeout", "40m", "--model", "M", "-p"}}, tool: fleet.Tool{Name: "agy", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--output-format stream-json"}}}},
			want: []string{"agy", "--model", "M", "--new-project", "--add-dir", "", "--log-file", os.DevNull, "--input-format", "stream-json", "--output-format", "stream-json", "-p="},
		},
		{
			name: "codex",
			w:    &Worker{cwd: "/tmp/cligw-test", mode: WarmStdin, launch: agentlaunch.Launch{Tool: "codex", Args: []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only"}}, tool: fleet.Tool{Name: "codex", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--json"}}}},
			// the bare model: codex's own tools off (Sprint 290 W1), every one
			// of them (Sprint 317): a tool call is another sampling request,
			// and turn.completed sums them all.
			want: append([]string{"codex", "exec", "-c", `model_instructions_file="/tmp/cligw-test/instructions.md"`,
				"--ephemeral", "--ignore-user-config", "--disable", "shell_tool", "--disable", "apps", "--disable", "browser_use", "--disable", "computer_use",
				"-c", `web_search="disabled"`}, append(codexToolsOffConfig(),
				"--json", "--skip-git-repo-check", "-s", "read-only")...),
		},
		{
			name: "codex with a catalog",
			w:    &Worker{cwd: "/tmp/cligw-test", mode: WarmStdin, codexCatalog: "/tmp/cligw-test/models.json", launch: agentlaunch.Launch{Tool: "codex", Args: []string{"exec", "--sandbox", "read-only"}}, tool: fleet.Tool{Name: "codex", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--json"}}}},
			want: append([]string{"codex", "exec", "-c", `model_catalog_json="/tmp/cligw-test/models.json"`, "-c", `model_instructions_file="/tmp/cligw-test/instructions.md"`,
				"--ephemeral", "--ignore-user-config", "--disable", "shell_tool", "--disable", "apps", "--disable", "browser_use", "--disable", "computer_use",
				"-c", `web_search="disabled"`}, append(codexToolsOffConfig(),
				"--json", "-s", "read-only")...),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.w.argv("", ""); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("argv = %#v\nwant = %#v", got, tt.want)
			}
		})
	}
}

func TestWorkerClaudePartialMessagesAreDeltasWithoutSnapshotDuplication(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "claude-partials")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	got, err := w.DoCompletion(context.Background(), CompletionPrompt{System: "Be concise.", Prompt: "hi"}, func(ev Event) {
		if ev.Text != "" {
			deltas = append(deltas, ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "hello" || !reflect.DeepEqual(deltas, []string{"hel", "lo"}) {
		t.Fatalf("result=%+v deltas=%q", got, deltas)
	}
}

func TestWorkerSystemPromptMechanisms(t *testing.T) {
	for _, tt := range []struct {
		tool string
		warm WarmMode
	}{
		{tool: "claude", warm: WarmStdinStreamJSON},
		{tool: "codex", warm: WarmStdin},
		{tool: "agy", warm: WarmStdinStreamJSON},
	} {
		t.Run(tt.tool, func(t *testing.T) {
			capturePath := filepath.Join(t.TempDir(), "capture.json")
			t.Setenv("CLIGW_CAPTURE_PATH", capturePath)
			installFakeCatalog(t, tt.tool, tt.warm, "system-prompt-"+tt.tool)
			w, err := NewWorker(context.Background(), "test-agent")
			if err != nil {
				t.Fatal(err)
			}
			got, err := w.DoCompletion(context.Background(), CompletionPrompt{System: "Be concise.", Prompt: "hello"}, nil)
			if err != nil || got.Text != "ok" {
				t.Fatalf("result=%+v err=%v", got, err)
			}
			var capture struct {
				Args         []string `json:"args"`
				Body         string   `json:"body"`
				Instructions string   `json:"instructions"`
			}
			data, err := os.ReadFile(capturePath)
			if err != nil || json.Unmarshal(data, &capture) != nil {
				t.Fatalf("capture: %s: %v", data, err)
			}
			wantSystem := neutralSystemPrompt + "\n\nBe concise."
			switch tt.tool {
			case "claude":
				if !containsSequence(capture.Args, []string{"--system-prompt", wantSystem}) || strings.Contains(capture.Body, "Be concise.") {
					t.Fatalf("claude capture = %+v", capture)
				}
			case "codex":
				if strings.TrimSpace(capture.Instructions) != wantSystem || strings.Contains(capture.Body, "Be concise.") {
					t.Fatalf("codex capture = %+v", capture)
				}
			case "agy":
				inline := strings.ReplaceAll("System:\n"+wantSystem+"\n\nhello", "\n", `\n`)
				if !strings.Contains(capture.Body, inline) {
					t.Fatalf("agy capture = %+v", capture)
				}
			}
		})
	}
}

func installFakeCatalog(t *testing.T, toolName string, warm WarmMode, mode string) {
	t.Helper()
	installFakeCatalogEffort(t, toolName, warm, mode, "")
}

func installFakeCatalogEffort(t *testing.T, toolName string, warm WarmMode, mode, effort string) {
	t.Helper()
	t.Setenv("CLIGW_TEST_HELPER", "1")
	cat := fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithBaselineFS(fstest.MapFS{}))
	launch := fleet.ToolLaunch{
		Exec:         fmt.Sprintf("%s -test.run=TestCLIHelper -- %s {prompt}", os.Args[0], mode),
		Warm:         string(warm),
		EventsStdout: "--events-json",
		EventsDone:   fleet.EventsDone{Field: "type", Values: []string{"result", "turn.completed"}},
	}
	if toolName == "claude" {
		launch.EventsOutcome = fleet.EventsOutcome{Path: "is_error", OK: []string{"false"}}
	} else if toolName == "muse" || strings.HasPrefix(mode, "muse-") {
		// As the baseline muse.yaml declares them (Muse Code 1.3.0).
		launch.EventsDone = fleet.EventsDone{Field: "payload_type", Values: []string{"run.terminal.completed", "run.terminal.failed", "run.terminal.cancelled"}}
		launch.EventsOutcome = fleet.EventsOutcome{Path: "payload.terminal", OK: []string{"completed"}}
	} else if toolName == "agy" {
		launch.EventsDone = fleet.EventsDone{Field: "event", Values: []string{"result"}}
		launch.EventsOutcome = fleet.EventsOutcome{Path: "result.status", OK: []string{"SUCCESS"}}
	}
	if err := cat.SaveTool(fleet.Tool{Name: toolName, Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: os.Args[0], Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "test-agent", Tool: toolName, Effort: effort}); err != nil {
		t.Fatal(err)
	}
	old := agentlaunch.NewCatalog
	agentlaunch.NewCatalog = func() *fleet.Catalog { return cat }
	t.Cleanup(func() { agentlaunch.NewCatalog = old })
}

func waitFor(t *testing.T, timeout time.Duration, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if check() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("condition did not become true")
}

func TestCodexToolsOffConfigTurnsOffEveryRemainingTool(t *testing.T) {
	// Measured on codex-cli 0.157.1 (Sprint 317): with only shell/apps/browser/
	// computer off, gpt-5.5 still listed request_user_input, view_image,
	// get/create/update_goal, apply_patch, image_gen, tool_search and
	// multi_tool_use.parallel.
	got := strings.Join(codexToolsOffConfig(), " ")
	for _, want := range []string{"features.goals=false", "features.image_generation=false", "features.view_image=false",
		"features.tool_suggest=false", "features.skill_search=false", "features.multi_agent=false", "features.sleep_tool=false",
		"features.unified_exec=false", "features.plugins=false", "tools.experimental_request_user_input.enabled=false",
		"include_environment_context=false", "include_permissions_instructions=false"} {
		if !strings.Contains(got, want) {
			t.Errorf("codexToolsOffConfig lacks %s: %s", want, got)
		}
	}
	if strings.Contains(got, "--disable") {
		t.Errorf("an unknown --disable name is fatal on older codex; use -c features.X=false: %s", got)
	}
}

func TestWriteCodexCatalogDropsApplyPatch(t *testing.T) {
	home := t.TempDir()
	cache := `{"fetched_at":"2026-09-27T00:00:00Z","etag":"x","models":[` +
		`{"slug":"gpt-5.5","apply_patch_tool_type":"freeform","context_window":272000},` +
		`{"slug":"gpt-5.6-sol","apply_patch_tool_type":"function"}]}`
	if err := os.WriteFile(filepath.Join(home, "models_cache.json"), []byte(cache), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEX_HOME", home)
	dir := t.TempDir()
	path, err := writeCodexCatalog(dir)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(path) != dir {
		t.Fatalf("catalog %s is not in the worker dir %s", path, dir)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Models) != 2 {
		t.Fatalf("models = %v", got.Models)
	}
	for _, m := range got.Models {
		if v, ok := m["apply_patch_tool_type"]; !ok || v != nil {
			t.Fatalf("%v: apply_patch_tool_type must be present and null", m["slug"])
		}
	}
	if got.Models[0]["context_window"] != float64(272000) {
		t.Fatalf("other metadata must be kept: %v", got.Models[0])
	}

	t.Setenv("CODEX_HOME", t.TempDir())
	if path, err := writeCodexCatalog(t.TempDir()); err != nil || path != "" {
		t.Fatalf("no cache: path=%q err=%v, want no catalog and no error", path, err)
	}
}
