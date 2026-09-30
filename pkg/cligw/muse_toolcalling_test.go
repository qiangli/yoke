package cligw

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// museAgentReport is what door-muse answered a genie tool-calling turn with
// (Sprint 340 story 19): Muse reasons about ITS session instead of acting as
// a bare completion for the caller's tools.
const museAgentReport = "Workspace is empty and session is read-only... Requested Bashy tool is not in this session's tool list"

func TestMuseSeatRefusesToolCallingRequest(t *testing.T) {
	tools := `[{"type":"function","function":{"name":"bash","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}}]`
	var offered []openai.Tool
	if err := json.Unmarshal([]byte(tools), &offered); err != nil {
		t.Fatal(err)
	}
	// The recorded answer is not a tool call, and no recovery can make it one.
	if got := recoverToolAnswer(museAgentReport, offered); got != museAgentReport {
		t.Fatalf("agent report decoded to %q", got)
	}
	if ToolCallingCapable("muse") || !ToolCallingCapable("claude") || !ToolCallingCapable("codex") {
		t.Fatal("ToolCallingCapable: muse must be the only refused tool")
	}

	backend := NewAgentBackend("door-muse", "spark", nil)
	backend.Tool = "muse"
	rec, attempt := serveBackend(t, backend, `{"model":"door-muse","messages":[{"role":"user","content":"ls"}],"tools":`+tools+`}`, nil)
	if attempt.Status != http.StatusBadRequest || attempt.CanRetry {
		t.Fatalf("attempt = %+v, want non-retryable 400", attempt)
	}
	if body := rec.Body.String(); !strings.Contains(body, "door-muse") || !strings.Contains(body, "tool") {
		t.Fatalf("refusal must name the seat: %s", body)
	}

	// Plain-text chat still goes to the pool (nil here: 503, not a refusal).
	_, attempt = serveBackend(t, backend, `{"model":"door-muse","messages":[{"role":"user","content":"hi"}]}`, nil)
	if attempt.Status != http.StatusServiceUnavailable {
		t.Fatalf("plain chat attempt = %+v, want the pool path", attempt)
	}
}
