package weave

import (
	"bytes"
	"strings"
	"testing"
)

func TestWeaveDistillStreamJSONLine(t *testing.T) {
	tests := []struct {
		name string
		line string
		want string
		ok   bool
	}{
		{
			name: "assistant tool use description",
			line: `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Read","input":{"description":"inspect the PTY logging path","file_path":"pkg/weave/weave_pty.go"}}]},"uuid":"noise","usage":{"input_tokens":123}}`,
			want: "-> Read inspect the PTY logging path",
			ok:   true,
		},
		{
			name: "assistant tool use command",
			line: `{"type":"assistant","content":[{"type":"tool_use","name":"Bash","input":{"command":"go test ./..."}}]}`,
			want: "-> Bash go test ./...",
			ok:   true,
		},
		{
			name: "assistant text",
			line: `{"type":"assistant","message":{"content":[{"type":"text","text":"  reviewing the failing test\n"}]}}`,
			want: "reviewing the failing test",
			ok:   true,
		},
		{
			name: "assistant thinking dropped",
			line: `{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"private","signature":"noise"}]}}`,
			want: "",
			ok:   true,
		},
		{
			name: "user tool result ok",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","content":"PASS\nok pkg/weave"}]}}`,
			want: "   ok: PASS",
			ok:   true,
		},
		{
			name: "user tool result err",
			line: `{"type":"user","message":{"content":[{"type":"tool_result","is_error":true,"content":[{"type":"text","text":"panic: boom\nstack"}]}]}}`,
			want: "   err: panic: boom",
			ok:   true,
		},
		{
			name: "system dropped",
			line: `{"type":"system","subtype":"init","session_id":"uuid","tools":["Bash"]}`,
			want: "",
			ok:   true,
		},
		{
			name: "result summarized",
			line: `{"type":"result","subtype":"success","duration_ms":2030,"total_cost_usd":0.01234,"num_turns":4}`,
			want: "[result success turns=4 duration=2s cost=$0.0123]",
			ok:   true,
		},
		{
			name: "codex thread started",
			line: `{"type":"thread.started","thread_id":"uuid"}`,
			want: "[thread started]",
			ok:   true,
		},
		{
			name: "codex command started",
			line: `{"type":"item.started","item":{"type":"command_execution","command":"go test ./...","status":"in_progress"}}`,
			want: "-> command execution go test ./...",
			ok:   true,
		},
		{
			name: "codex command completed drops bulky output",
			line: `{"type":"item.completed","item":{"type":"command_execution","command":"go test ./...","aggregated_output":"PASS\n` + strings.Repeat("x", 500) + `","exit_code":0,"status":"completed"}}`,
			want: "   ok: PASS",
			ok:   true,
		},
		{
			name: "codex agent message",
			line: `{"type":"item.completed","item":{"type":"agent_message","text":"CODEX_EVENTS_OK"}}`,
			want: "CODEX_EVENTS_OK",
			ok:   true,
		},
		{
			name: "codex reasoning dropped",
			line: `{"type":"item.completed","item":{"type":"reasoning","text":"private"}}`,
			want: "",
			ok:   true,
		},
		{
			name: "codex turn completed",
			line: `{"type":"turn.completed","usage":{"input_tokens":72641,"cached_input_tokens":61440,"output_tokens":477}}`,
			want: "[turn completed input=72641 cached=61440 output=477]",
			ok:   true,
		},
		{
			name: "opencode step started",
			line: `{"type":"step_start","part":{"type":"step-start"}}`,
			want: "[step started]",
			ok:   true,
		},
		{
			name: "opencode tool use drops output",
			line: `{"type":"tool_use","part":{"type":"tool","tool":"read","state":{"status":"completed","input":{"filePath":"README.md"},"output":"` + strings.Repeat("large", 200) + `","title":"README.md"}}}`,
			want: "-> read README.md [completed]",
			ok:   true,
		},
		{
			name: "opencode text",
			line: `{"type":"text","part":{"type":"text","text":"OPENCODE_JSON_OK"}}`,
			want: "OPENCODE_JSON_OK",
			ok:   true,
		},
		{
			name: "opencode step finished",
			line: `{"type":"step_finish","part":{"type":"step-finish","reason":"stop","tokens":{"input":6141,"output":6},"cost":0.002700045}}`,
			want: "[step finished reason=stop input=6141 output=6 cost=$0.0027]",
			ok:   true,
		},
		{
			name: "plain text unchanged by caller",
			line: "plain non-json text",
			want: "",
			ok:   false,
		},
		{
			name: "malformed json unchanged by caller",
			line: `{"type":"assistant"`,
			want: "",
			ok:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := weaveDistillStreamJSONLine(tt.line)
			if ok != tt.ok || got != tt.want {
				t.Fatalf("weaveDistillStreamJSONLine()=(%q,%v), want (%q,%v)", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestWeaveStreamJSONLogWriter(t *testing.T) {
	var out bytes.Buffer
	w := newWeaveStreamJSONLogWriter(&out)
	input := strings.Join([]string{
		"plain before",
		`{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Grep","input":{"pattern":"idle"}}]}}`,
		`{"type":"assistant","message":{"content":[{"type":"thinking","thinking":"hidden"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"match one\nmatch two"}]}}`,
		"{bad json",
		"plain after",
	}, "\n")

	if _, err := w.Write([]byte(input[:90])); err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write([]byte(input[90:])); err != nil {
		t.Fatal(err)
	}
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}

	want := strings.Join([]string{
		"plain before",
		"-> Grep idle",
		"   ok: match one",
		"{bad json",
		"plain after",
	}, "\n")
	if out.String() != want {
		t.Fatalf("writer output:\n%s\nwant:\n%s", out.String(), want)
	}
}

func TestWeaveStreamJSONLogWriterPassesPlainTextWithoutNewline(t *testing.T) {
	var out bytes.Buffer
	w := newWeaveStreamJSONLogWriter(&out)
	if _, err := w.Write([]byte("plain partial")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "plain partial" {
		t.Fatalf("plain text was buffered: got %q", out.String())
	}
}
