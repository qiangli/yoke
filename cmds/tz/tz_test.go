package tzcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

func runTool(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{Ctx: context.Background(), Dir: t.TempDir(), Stdio: tool.Stdio{Out: &out, Err: &errb, In: strings.NewReader("")}}
	code := cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestListAndConvert(t *testing.T) {
	out, errb, code := runTool(t, "list", "New_York")
	if code != 0 || errb != "" || !strings.Contains(out, "America/New_York\n") {
		t.Fatalf("list: code=%d out=%q err=%q", code, out, errb)
	}
	out, errb, code = runTool(t, "convert", "2024-01-02 15:04", "America/New_York", "UTC")
	if code != 0 || errb != "" || strings.TrimSpace(out) != "2024-01-02T20:04:00Z" {
		t.Fatalf("convert: code=%d out=%q err=%q", code, out, errb)
	}
}

func TestJSONInfo(t *testing.T) {
	out, errb, code := runTool(t, "--json", "info", "UTC")
	if code != 0 || errb != "" {
		t.Fatalf("info json: code=%d err=%q", code, errb)
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(out), &got); err != nil || got["zone"] != "UTC" || got["ok"] != true {
		t.Fatalf("bad json %q err=%v", out, err)
	}
}
