package cligw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// This checks the prompt delivered to the CLI, not model compliance. The fixture
// is constructed, not a captured live request. Its system text matches
// ycode/examples/genie/prompts/system.md at 22fe94f96191; the user request is the
// reported failing scratch-file task with a minimal bashy tool schema.
func TestMuseGenieCompletionContext(t *testing.T) {
	data, err := os.ReadFile("testdata/genie-muse-request.json")
	if err != nil {
		t.Fatal(err)
	}
	var req openai.ChatRequest
	if err := json.Unmarshal(data, &req); err != nil {
		t.Fatal(err)
	}
	input, err := RenderCompletionPrompt(&req)
	if err != nil {
		t.Fatal(err)
	}
	capturePath := filepath.Join(t.TempDir(), "capture.json")
	t.Setenv("CLIGW_CAPTURE_PATH", capturePath)
	installFakeCatalog(t, "muse", WarmCold, "system-prompt-muse")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.DoCompletion(context.Background(), input, nil); err != nil {
		t.Fatal(err)
	}
	data, err = os.ReadFile(capturePath)
	if err != nil {
		t.Fatal(err)
	}
	var capture struct {
		Args []string `json:"args"`
	}
	if err := json.Unmarshal(data, &capture); err != nil {
		t.Fatal(err)
	}
	prompt := capture.Args[len(capture.Args)-1]
	if !strings.HasPrefix(prompt, "Complete the supplied conversation") {
		t.Fatal("Muse receives an inline system prompt without completion framing")
	}
	for _, part := range []string{input.System, input.Prompt, "caller executes", "never answer with a", `"name": "bashy"`} {
		if !strings.Contains(prompt, part) {
			t.Errorf("missing completion context %q", part)
		}
	}
	// Resolve the caller's native-tool wording AFTER the supplied conversation.
	if strings.LastIndex(prompt, "serialize") < strings.Index(prompt, input.Prompt) {
		t.Error("missing final transport instruction after the conversation")
	}
	for _, flag := range []string{"--no-session-log", "--no-foreign-personal-context", "--disable-web-tools", "--disable-shell", "--disable-write"} {
		if !containsSequence(capture.Args, []string{flag}) {
			t.Errorf("missing isolation flag %s", flag)
		}
	}
	for _, flag := range []string{"--yolo", "--disable-sandbox", "--enable-shell-tool", "--trust-workspace"} {
		if containsSequence(capture.Args, []string{flag}) {
			t.Errorf("unsafe flag %s", flag)
		}
	}
}
