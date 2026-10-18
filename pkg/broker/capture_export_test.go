package broker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCaptureCommands(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	if err := writeCaptureSession("s-abcdef123456", "episode"); err != nil {
		t.Fatal(err)
	}
	dir, err := captureSessionDir("s-abcdef123456")
	if err != nil {
		t.Fatal(err)
	}
	turn := captureTurn{SchemaVersion: "bashy-capture-v1", At: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), Session: "s-abcdef123456", Episode: "episode", Response: `{"choices":[{"message":{"content":"hello"}}]}`}
	b, err := json.Marshal(turn)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(dir, "turns.jsonl"), append(b, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		args     []string
		contains string
	}{
		{[]string{"capture", "list", "--json"}, `"turns":1`},
		{[]string{"capture", "show", "s-abcdef123456", "--json"}, `"bashy-capture-v1"`},
		{[]string{"capture", "export", "s-abcdef123456", "--format", "atif"}, `"ATIF-v1.7"`},
	} {
		cmd := NewCmd()
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetArgs(tc.args)
		if err := cmd.Execute(); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out.Bytes(), []byte(tc.contains)) {
			t.Fatalf("%v: %s", tc.args, out.String())
		}
	}
}

func TestExportATIFGolden(t *testing.T) {
	turns := []captureTurn{{At: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC), Session: "s-abcdef123456", Model: "L4", Response: `{"choices":[{"message":{"content":"hello","tool_calls":[{"id":"call_1","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}]}`, Usage: &captureUsage{PromptTokens: 3, CompletionTokens: 2}}}
	got, err := exportATIF("s-abcdef123456", turns)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(filepath.Join("testdata", "atif_golden.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bytes.TrimSpace(got), bytes.TrimSpace(want)) {
		t.Fatalf("ATIF differs from golden:\n%s", got)
	}
}
