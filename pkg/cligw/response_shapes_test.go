package cligw

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentlaunch"
	"github.com/qiangli/yoke/pkg/fleet"
)

// Synthetic content in measured Claude/Agy/Muse envelopes. Never record live
// prompts, replies, credentials, conversation IDs, or tool input as fixtures.
func TestWorkerResponseShapes(t *testing.T) {
	for _, tc := range []struct {
		name, tool, file, body string
		failed                 bool
	}{
		{name: "claude stream", tool: "claude", file: "claude-stream.jsonl"},
		{name: "agy stream", tool: "agy", file: "agy-stream.jsonl"},
		{name: "muse stream", tool: "muse", file: "muse-exec-json-ok.jsonl"},
		{name: "claude terminal", tool: "claude", body: `{"type":"result","is_error":false,"result":"ok"}`},
		{name: "agy terminal", tool: "agy", body: `{"event":"result","result":{"status":"SUCCESS","response":"ok"}}`},
		{name: "muse terminal", tool: "muse", body: `{"payload_type":"run.terminal.completed","payload":{"terminal":"completed","text":"ok"}}`},
		{name: "claude failure", tool: "claude", body: `{"type":"result","is_error":true,"result":"PRIVATE_FAILURE"}`, failed: true},
		{name: "agy failure", tool: "agy", body: `{"event":"result","result":{"status":"ERROR","response":"PRIVATE_FAILURE"}}`, failed: true},
		{name: "muse failure", tool: "muse", body: `{"payload_type":"run.terminal.failed","payload":{"terminal":"failed","text":"PRIVATE_FAILURE"}}`, failed: true},
		{name: "muse cancelled", tool: "muse", body: `{"payload_type":"run.terminal.cancelled","payload":{"terminal":"cancelled"}}`, failed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var path string
			if tc.file != "" {
				path, _ = filepath.Abs(filepath.Join("testdata", tc.file))
			} else {
				path = filepath.Join(t.TempDir(), "events.jsonl")
				if err := os.WriteFile(path, []byte(tc.body+"\n"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("CLIGW_MUSE_FIXTURE", path)
			installFakeCatalog(t, tc.tool, WarmCold, "response-fixture")
			w, err := NewWorker(context.Background(), "test-agent")
			if err != nil {
				t.Fatal(err)
			}
			var text strings.Builder
			got, err := w.Do(context.Background(), "PRIVATE_PROMPT", func(ev Event) { text.WriteString(ev.Text) })
			if tc.failed {
				if err == nil || got.Outcome != OutcomeError {
					t.Fatal("failed terminal accepted")
				}
				if strings.Contains(err.Error(), "PRIVATE_") {
					t.Fatal("error disclosed provider content")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got.Text != "ok" || text.String() != "ok" {
				t.Fatal("answer missing, duplicated, or contaminated")
			}
			if tc.file == "agy-stream.jsonl" && (got.Usage.CachedInputTokens != 4 || got.Usage.TotalTokens != 12) {
				t.Fatal("Agy usage lost")
			}
		})
	}
}

func TestWorkerErrorDoesNotExposeStderr(t *testing.T) {
	w := &Worker{agent: "fixture", stderr: &lockedBuffer{}}
	w.stderr.Write([]byte("PRIVATE_PROMPT PRIVATE_REPLY PRIVATE_CREDENTIAL"))
	if strings.Contains(w.runError(nil, nil).Error(), "PRIVATE_") {
		t.Fatal("stderr disclosed")
	}
}

func TestDoorIsolationArgv(t *testing.T) {
	for _, mode := range []WarmMode{WarmCold, WarmStdinStreamJSON} {
		w := &Worker{mode: mode, cwd: "/isolated", tool: fleet.Tool{Name: "agy"}, launch: agentlaunch.Launch{Tool: "agy", Args: []string{"--model", "fixture", "-p"}}}
		args := w.argv("fixture prompt", "")
		if !containsSequence(args, []string{"--new-project", "--add-dir", "/isolated", "--log-file", os.DevNull}) {
			t.Fatal("Agy isolation missing")
		}
		if mode == WarmCold && !containsSequence(args, []string{"-p", "fixture prompt"}) {
			t.Fatal("Agy flags swallowed by prompt")
		}
	}
	w := &Worker{mode: WarmCold, tool: fleet.Tool{Name: "muse"}, launch: agentlaunch.Launch{Tool: "muse", Args: []string{"exec"}}}
	args := w.argv("fixture prompt", "")
	for _, flag := range []string{"--no-session-log", "--no-foreign-personal-context", "--disable-web-tools", "--disable-shell", "--disable-write"} {
		if !containsSequence(args, []string{flag}) {
			t.Errorf("missing %s", flag)
		}
	}
}
