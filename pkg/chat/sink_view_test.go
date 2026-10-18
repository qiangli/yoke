package chat

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/reduce"
)

func recoverFromNote(t *testing.T, dir, out string) []byte {
	t.Helper()
	i := strings.LastIndex(out, "canonical: bashy out ")
	if i < 0 {
		t.Fatalf("no canonical recovery pointer: %q", out)
	}
	handle := strings.TrimRight(strings.Fields(out[i+len("canonical: bashy out "):])[0], "]\n")
	var buf bytes.Buffer
	if err := reduce.Recover(reduce.NewStore(dir), handle, &buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func sinkHome(t *testing.T) string {
	t.Helper()
	h := t.TempDir()
	setTestHome(t, h)
	return h + "/.bashy/chat/output"
}

func TestSinkAppliesDeclaredViewWithRecovery(t *testing.T) {
	dir := sinkHome(t)
	canonical := `{"name":"x","items":[1,2,3]}`
	plan := fleet.SinkPlan{View: fleet.ViewYAML, ViewSource: fleet.SourceEnv}
	out, err := reduceChatBytes([]byte(canonical), plan)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: x") || strings.Contains(out, `{"name"`) {
		t.Fatalf("not rendered as yaml: %q", out)
	}
	if !strings.Contains(out, "view=yaml(env) from json · lossless") {
		t.Fatalf("view note missing: %q", out)
	}
	if got := recoverFromNote(t, dir, out); string(got) != canonical {
		t.Fatalf("canonical not recoverable: %q", got)
	}
}

func TestSinkLossyViewIsLabelled(t *testing.T) {
	dir := sinkHome(t)
	canonical := `<html><body><h1>Title</h1><p>hello <b>world</b></p></body></html>`
	out, err := reduceChatBytes([]byte(canonical), fleet.SinkPlan{View: fleet.ViewMD, ViewSource: fleet.SourceEnv})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Title") || !strings.Contains(out, "from html · lossy") || strings.Contains(out, "<h1>") {
		t.Fatalf("lossy md view wrong: %q", out)
	}
	if got := recoverFromNote(t, dir, out); string(got) != canonical {
		t.Fatalf("canonical html not recoverable: %q", got)
	}
}

func TestSinkViewBypassedAndSafe(t *testing.T) {
	sinkHome(t)
	canonical := `{"a":1}`
	out, _ := reduceChatBytes([]byte(canonical), fleet.PlanSink(fleet.ReaderPrefs{View: fleet.ViewYAML}, fleet.ReaderPrefsSources{}, true))
	if out != canonical {
		t.Fatalf("--full must keep canonical bytes: %q", out)
	}
	out, _ = reduceChatBytes([]byte(canonical), fleet.SinkPlan{Canonical: true, View: fleet.ViewYAML})
	if out != canonical {
		t.Fatalf("canonical plan converted: %q", out)
	}
	// Undetectable prose and invalid JSON pass through untouched.
	for _, in := range []string{"plain prose answer", `{"a":`} {
		out, _ = reduceChatBytes([]byte(in), fleet.SinkPlan{View: fleet.ViewYAML})
		if out != in {
			t.Fatalf("%q altered: %q", in, out)
		}
	}
	// No view preference: unchanged.
	if out, _ = reduceChatBytes([]byte(canonical), fleet.SinkPlan{}); out != canonical {
		t.Fatalf("no-prefs golden changed: %q", out)
	}
}

func TestSinkViewThenBudget(t *testing.T) {
	sinkHome(t)
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i < 400; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"k":"value value value"}`)
	}
	b.WriteString("]")
	out, err := reduceChatBytes([]byte(b.String()), fleet.SinkPlan{View: fleet.ViewYAML, BudgetBytes: 1024})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 1024 || !strings.Contains(out, "full: bashy out ") || !strings.Contains(out, "canonical: bashy out ") {
		t.Fatalf("budget not applied after view (%d bytes): %q", len(out), out)
	}
}

func TestSinkLossyViewTightBudgetKeepsCanonicalPointer(t *testing.T) {
	dir := sinkHome(t)
	canonical := "<html><body>" + strings.Repeat("<p>hello <b>world</b> and more text here</p>", 300) + "</body></html>"
	out, err := reduceChatBytes([]byte(canonical), fleet.SinkPlan{View: fleet.ViewMD, ViewSource: fleet.SourceEnv, BudgetBytes: 512})
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 512 || !strings.Contains(out, "full: bashy out ") {
		t.Fatalf("not reduced (%d bytes): %q", len(out), out)
	}
	if got := recoverFromNote(t, dir, out); string(got) != canonical {
		t.Fatalf("canonical html not recoverable after budget: %d bytes", len(got))
	}
}
