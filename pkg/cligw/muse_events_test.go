package cligw

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/llmgw/openai"
)

// Muse Code (`muse exec --json`) writes one MSP record per line: the kind is
// the top-level payload_type, the answer streams as run.output.delta
// payload.text, and run.terminal.completed repeats the whole answer in
// payload.text. Measured against Muse Code 1.3.0 (muse-spark-1.3), 2026-09-28.

func museFixture(t *testing.T) {
	t.Helper()
	path, err := filepath.Abs(filepath.Join("testdata", "muse-exec-json-ok.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("CLIGW_MUSE_FIXTURE", path)
}

func museEventsDone() fleet.EventsDone {
	return fleet.EventsDone{Field: "payload_type", Values: []string{"run.terminal.completed", "run.terminal.failed", "run.terminal.cancelled"}}
}

func TestParseEventMuseOutputDelta(t *testing.T) {
	body, err := os.ReadFile(filepath.Join("testdata", "muse-exec-json-ok.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var text strings.Builder
	var done int
	for _, line := range strings.Split(strings.TrimSpace(string(body)), "\n") {
		ev, ok := parseEvent([]byte(line), museEventsDone())
		if !ok {
			t.Fatalf("unparsed line %s", line)
		}
		text.WriteString(ev.Text)
		if ev.Done {
			done++
		}
	}
	// The terminal record's payload.text repeats the deltas; it is never an
	// event delta of its own (no duplication).
	if text.String() != "ok" || done != 1 {
		t.Fatalf("text=%q done=%d", text.String(), done)
	}
}

func TestWorkerMuseRealStreamYieldsAnswer(t *testing.T) {
	museFixture(t)
	installFakeCatalog(t, "muse", WarmCold, "muse-fixture")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	var deltas []string
	got, err := w.Do(context.Background(), "Reply with exactly the word ok.", func(ev Event) {
		if ev.Text != "" {
			deltas = append(deltas, ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "ok" || got.Outcome == OutcomeError || !reflect.DeepEqual(deltas, []string{"ok"}) {
		t.Fatalf("result=%+v deltas=%q", got, deltas)
	}
	// Muse 1.3.0 reports no token usage in its stream: estimated, flagged.
	if !got.Usage.Estimated || got.Usage.OutputTokens == 0 {
		t.Fatalf("usage = %+v", got.Usage)
	}
}

func TestWorkerMuseTerminalTextWhenNoDeltas(t *testing.T) {
	installFakeCatalog(t, "muse", WarmCold, "muse-terminal-only")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Do(context.Background(), "hi", nil)
	if err != nil || got.Text != "only terminal" {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestWorkerMuseFailedTerminalIsError(t *testing.T) {
	installFakeCatalog(t, "muse", WarmCold, "muse-failed")
	w, err := NewWorker(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.Do(context.Background(), "hi", nil)
	if err == nil || got.Outcome != OutcomeError {
		t.Fatalf("result=%+v err=%v", got, err)
	}
}

func TestAgentBackendMuseRealStreamNonEmpty(t *testing.T) {
	museFixture(t)
	backend, cleanup := testBackend(t, "muse-fixture")
	defer cleanup()
	rec, attempt := serveBackend(t, backend, `{"model":"test-model","messages":[{"role":"user","content":"Reply with exactly the word ok."}]}`, nil)
	if attempt.Status != http.StatusOK {
		t.Fatalf("attempt = %+v", attempt)
	}
	var got openai.ChatCompletion
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Choices[0].Message.Content != "ok" {
		t.Fatalf("completion = %s", rec.Body.String())
	}
}

func TestAgentBackendMuseToolCallReachesEnvelope(t *testing.T) {
	for _, stream := range []bool{false, true} {
		backend, cleanup := testBackend(t, "muse-tool")
		modify := func(resp *http.Response) error {
			resp.Body = openai.ApplyToolCallExtractor(resp)
			return nil
		}
		body := `{"model":"test-model","messages":[{"role":"user","content":"weather"}],"tools":[{"type":"function","function":{"name":"weather","parameters":{"type":"object"}}}]}`
		if stream {
			body = strings.Replace(body, `{"model"`, `{"stream":true,"model"`, 1)
		}
		rec, attempt := serveBackend(t, backend, body, modify)
		cleanup()
		if attempt.Status != http.StatusOK {
			t.Fatalf("stream=%v attempt = %+v", stream, attempt)
		}
		out := rec.Body.String()
		if !strings.Contains(out, `"tool_calls"`) || !strings.Contains(out, `"finish_reason":"tool_calls"`) || !strings.Contains(out, "Paris") {
			t.Fatalf("stream=%v response = %s", stream, out)
		}
	}
}
