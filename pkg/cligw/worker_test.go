package cligw

import (
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
	case "backend-stream":
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"hello "}}`)
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"world"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":8,"output_tokens":2}}`)
	case "claude-partials":
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"hel"}}}`)
		fmt.Println(`{"type":"stream_event","event":{"type":"content_block_delta","delta":{"type":"text_delta","text":"lo"}}}`)
		fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`)
		fmt.Println(`{"type":"result","is_error":false,"usage":{"input_tokens":5,"output_tokens":1}}`)
	case "system-prompt-claude", "system-prompt-codex", "system-prompt-agy":
		body, _ := io.ReadAll(os.Stdin)
		instructions := ""
		for i, arg := range args {
			if arg == "-c" && i+1 < len(args) && strings.HasPrefix(args[i+1], "model_instructions_file=") {
				path, _ := strconv.Unquote(strings.TrimPrefix(args[i+1], "model_instructions_file="))
				contents, _ := os.ReadFile(path)
				instructions = string(contents)
			}
		}
		capture, _ := json.Marshal(map[string]any{"args": args, "body": string(body), "instructions": instructions})
		_ = os.WriteFile(os.Getenv("CLIGW_CAPTURE_PATH"), capture, 0o600)
		switch strings.TrimPrefix(args[0], "system-prompt-") {
		case "claude":
			fmt.Println(`{"type":"assistant","message":{"content":[{"type":"text","text":"ok"}]}}`)
			fmt.Println(`{"type":"result","is_error":false,"usage":{"input_tokens":1,"output_tokens":1}}`)
		case "agy":
			fmt.Println(`{"event":"delta","delta":{"text":"ok"}}`)
			fmt.Println(`{"event":"result","result":{"status":"SUCCESS"},"usage":{"input_tokens":1,"output_tokens":1}}`)
		case "codex":
			fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}`)
			fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	case "backend-tool":
		fmt.Println(`{"type":"item.completed","item":{"type":"agent_message","text":"{\"tool_calls\":[{\"name\":\"weather\",\"arguments\":{\"city\":\"Paris\"}}]}"}}`)
		fmt.Println(`{"type":"turn.completed","usage":{"input_tokens":9,"output_tokens":7}}`)
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
	if err == nil || got.Outcome != OutcomeError || !strings.Contains(err.Error(), "fake crash") {
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
			want: []string{"agy", "--model", "M", "--input-format", "stream-json", "--output-format", "stream-json", "-p="},
		},
		{
			name: "codex",
			w:    &Worker{cwd: "/tmp/cligw-test", mode: WarmStdin, launch: agentlaunch.Launch{Tool: "codex", Args: []string{"exec", "--skip-git-repo-check", "--sandbox", "read-only"}}, tool: fleet.Tool{Name: "codex", CLI: fleet.ToolCLI{Launch: fleet.ToolLaunch{EventsStdout: "--json"}}}},
			want: []string{"codex", "exec", "-c", `model_instructions_file="/tmp/cligw-test/instructions.md"`, "--json", "--skip-git-repo-check", "-s", "read-only"},
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
	} else if toolName == "agy" {
		launch.EventsDone = fleet.EventsDone{Field: "event", Values: []string{"result"}}
		launch.EventsOutcome = fleet.EventsOutcome{Path: "result.status", OK: []string{"SUCCESS"}}
	}
	if err := cat.SaveTool(fleet.Tool{Name: toolName, Kind: fleet.ToolKindCLI, CLI: fleet.ToolCLI{Binary: os.Args[0], Launch: launch}}); err != nil {
		t.Fatal(err)
	}
	if err := cat.SaveAgent(fleet.Agent{Name: "test-agent", Tool: toolName}); err != nil {
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
