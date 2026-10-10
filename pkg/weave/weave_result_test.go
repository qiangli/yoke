package weave

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWeaveExtractResult_ClaudeStreamJSON(t *testing.T) {
	// Recorded sample of Claude stream-json ending in a result event.
	sample := `{"type":"system","message":{"role":"system","content":"You are a helpful assistant."}}
{"type":"assistant","message":{"id":"msg_1","role":"assistant","content":[{"type":"text","text":"I will examine the codebase."}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"file content"}]}}
{"type":"assistant","message":{"id":"msg_2","role":"assistant","content":[{"type":"text","text":"Part 1 of the report: work in progress..."}]}}
{"type":"result","subtype":"success","num_turns":3,"duration_ms":2450,"total_cost_usd":0.0345,"result":"# Final Report\n\nAll tasks completed successfully.\n- Fixed race condition in queue lock\n- Added regression tests in weave_result_test.go\n\nVerification: green."}`

	got := weaveExtractResult(sample)
	want := "# Final Report\n\nAll tasks completed successfully.\n- Fixed race condition in queue lock\n- Added regression tests in weave_result_test.go\n\nVerification: green."
	if got != want {
		t.Fatalf("weaveExtractResult() =\n%q\nwant:\n%q", got, want)
	}
}

func TestWeaveExtractResult_ClaudeStreamJSON_MultipleResults(t *testing.T) {
	// When multiple result events occur, the final result event's result field is taken.
	sample := `{"type":"assistant","message":{"content":[{"type":"text","text":"First pass"}]}}
{"type":"result","subtype":"intermediate","result":"Intermediate findings"}
{"type":"assistant","message":{"content":[{"type":"text","text":"Second pass"}]}}
{"type":"result","subtype":"success","result":"Final conclusions and results."}`

	got := weaveExtractResult(sample)
	want := "Final conclusions and results."
	if got != want {
		t.Fatalf("weaveExtractResult() = %q, want %q", got, want)
	}
}

func TestWeaveExtractResult_ClaudeStreamJSON_ANSIWrapped(t *testing.T) {
	// ANSI/TUI noise wrapping stream-json lines (the Sprint 412 PTY log corruption problem).
	sample := "\x1b[0m\x1b[34m{\"type\":\"assistant\",\"message\":{\"content\":[{\"type\":\"text\",\"text\":\"Starting...\"}]}}\x1b[0m\r\n" +
		"\x1b[1;32m{\"type\":\"result\",\"subtype\":\"success\",\"result\":\"# Report with ANSI noise stripped\\n- Verified clean.\"}\x1b[0m\n"

	got := weaveExtractResult(sample)
	want := "# Report with ANSI noise stripped\n- Verified clean."
	if got != want {
		t.Fatalf("weaveExtractResult() =\n%q\nwant:\n%q", got, want)
	}
}

func TestWeaveExtractResult_CodexExec(t *testing.T) {
	// Recorded sample of Codex exec with item.completed agent_message events.
	sample := `{"type":"thread.started","thread_id":"th_123"}
{"type":"turn.started"}
{"type":"item.started","item":{"id":"item_0","type":"agent_message"}}
{"type":"item.completed","item":{"id":"item_0","type":"agent_message","text":"I am looking at the repository structure."}}
{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"go test ./..."}}
{"type":"item.completed","item":{"id":"item_1","type":"command_execution","status":"ok","exit_code":0}}
{"type":"item.started","item":{"id":"item_2","type":"agent_message"}}
{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"## Codex Summary\n\nFixed issue #1812: persist each run's final report to logs/issue-N.result.md.\nAll unit tests pass."}}
{"type":"turn.completed","usage":{"input_tokens":1200,"output_tokens":350}}`

	got := weaveExtractResult(sample)
	want := "## Codex Summary\n\nFixed issue #1812: persist each run's final report to logs/issue-N.result.md.\nAll unit tests pass."
	if got != want {
		t.Fatalf("weaveExtractResult() =\n%q\nwant:\n%q", got, want)
	}
}

func TestWeaveExtractResult_PlainText(t *testing.T) {
	// Plain text tool output with ANSI color escapes and carriage return rewrites.
	sample := "\x1b[36mRunning tests...\x1b[0m\n" +
		"Compiling dependencies...\n" +
		"\x1b[33mProgress: 50%\x1b[0m\r\x1b[32mProgress: 100%\x1b[0m\n" +
		"\x1b[1;32mPASS: all 12 tests passed\x1b[0m\n" +
		"Final report: service build completed without errors."

	got := weaveExtractResult(sample)
	want := "Running tests...\nCompiling dependencies...\nProgress: 100%\nPASS: all 12 tests passed\nFinal report: service build completed without errors."
	if got != want {
		t.Fatalf("weaveExtractResult() =\n%q\nwant:\n%q", got, want)
	}
}

func TestWeaveExtractResult_PlainTextTailLines(t *testing.T) {
	// Plain text tool output exceeding weaveResultPlainTailLines (50 lines).
	var lines []string
	for i := 1; i <= 80; i++ {
		lines = append(lines, fmt.Sprintf("Log line %02d of verbose tool output", i))
	}
	sample := strings.Join(lines, "\n")

	got := weaveExtractResult(sample)
	gotLines := strings.Split(got, "\n")
	if len(gotLines) != weaveResultPlainTailLines {
		t.Fatalf("len(gotLines) = %d, want %d", len(gotLines), weaveResultPlainTailLines)
	}
	if gotLines[0] != "Log line 31 of verbose tool output" {
		t.Errorf("gotLines[0] = %q, want Log line 31", gotLines[0])
	}
	if gotLines[len(gotLines)-1] != "Log line 80 of verbose tool output" {
		t.Errorf("gotLines[last] = %q, want Log line 80", gotLines[len(gotLines)-1])
	}
}

func TestWeaveStatus_ShowsResultPath(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	out, code := runWeave(t, "add", "result path test run", "--json")
	if code != 0 {
		t.Fatalf("weave add failed (exit %d): %s", code, out)
	}

	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}

	// Write the result file next to the log file.
	logsDir := filepath.Join(dir, "logs")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	resultPath := filepath.Join(logsDir, "issue-1.result.md")
	resultContent := "# Test Run 1 Final Report\nEverything completed."
	if err := os.WriteFile(resultPath, []byte(resultContent), 0o644); err != nil {
		t.Fatal(err)
	}

	// Set item finished state and ResultPath.
	if err := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, 1)
		it.State = "submitted"
		it.ResultPath = resultPath
		it.LogPath = filepath.Join(logsDir, "issue-1.log")
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Test weave status in human text mode.
	textOut, code := runWeave(t, "status", "1")
	if code != 0 {
		t.Fatalf("weave status failed (exit %d): %s", code, textOut)
	}
	if !strings.Contains(textOut, "result:") || !strings.Contains(textOut, "issue-1.result.md") {
		t.Fatalf("weave status output missing result path:\n%s", textOut)
	}

	// Test weave status in JSON mode.
	jsonOut, code := runWeave(t, "status", "1", "--json")
	if code != 0 {
		t.Fatalf("weave status --json failed (exit %d): %s", code, jsonOut)
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &res); err != nil {
		t.Fatalf("unmarshal json error: %v, output: %s", err, jsonOut)
	}
	data, ok := res["result"].(map[string]any)
	if !ok {
		t.Fatalf("missing result envelope in %s", jsonOut)
	}
	gotPath, ok := data["result_path"].(string)
	if !ok || !strings.HasSuffix(gotPath, "issue-1.result.md") {
		t.Fatalf("expected result_path ending in issue-1.result.md, got %q in %v", gotPath, data)
	}
}

func TestWeaveRunEnd_PersistsResultFile(t *testing.T) {
	root := setupIsolationFixture(t)
	t.Chdir(root)

	if _, code := runWeave(t, "add", "run end persistence test", "--json"); code != 0 {
		t.Fatal("weave add failed")
	}

	// Run tool that outputs a plain report.
	out, code := runWeave(t, "start", "--issue", "1", "--pty", "never", "--", "sh", "-c", "echo 'Line 1'; echo 'Final report: task finished'")
	if code != 0 {
		t.Fatalf("weave start failed (exit %d): %s", code, out)
	}

	dir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		t.Fatal(err)
	}
	it := findWeaveItem(q, 1)
	if it == nil {
		t.Fatal("run #1 not found")
	}
	if it.ResultPath == "" {
		t.Fatal("it.ResultPath was not persisted on run completion")
	}

	content, err := os.ReadFile(it.ResultPath)
	if err != nil {
		t.Fatalf("failed to read result file %s: %v", it.ResultPath, err)
	}
	if !strings.Contains(string(content), "Final report: task finished") {
		t.Fatalf("result file content does not contain expected final report: %q", string(content))
	}
}
