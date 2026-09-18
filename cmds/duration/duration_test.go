package durationcmd

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

func TestDurationBasics(t *testing.T) {
	out, errb, code := runTool(t, "to-secs", "2h30m")
	if code != 0 || errb != "" || strings.TrimSpace(out) != "9000" {
		t.Fatalf("to-secs: code=%d out=%q err=%q", code, out, errb)
	}
	out, errb, code = runTool(t, "humanize", "9045")
	if code != 0 || errb != "" || strings.TrimSpace(out) != "2h30m45s" {
		t.Fatalf("humanize: code=%d out=%q err=%q", code, out, errb)
	}
	out, errb, code = runTool(t, "between", "1970-01-01T00:00:00Z", "1970-01-01T01:01:01Z")
	if code != 0 || errb != "" || strings.TrimSpace(out) != "1h1m1s" {
		t.Fatalf("between: code=%d out=%q err=%q", code, out, errb)
	}
}

func TestDurationJSON(t *testing.T) {
	out, _, code := runTool(t, "--json", "to-secs", "90s")
	var got map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || got["seconds"].(float64) != 90 {
		t.Fatalf("bad json: code=%d out=%q", code, out)
	}
}
