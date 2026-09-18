package clipcmd

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/atotto/clipboard"

	"github.com/qiangli/coreutils/tool"
)

func runClip(t *testing.T, stdin string, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Stdio: tool.Stdio{In: strings.NewReader(stdin), Out: &o, Err: &e},
	}
	code = cmd.Run(rc, args)
	return o.String(), e.String(), code
}

func TestClipRoundTrip(t *testing.T) {
	// Skip where no OS clipboard utility is available (headless CI).
	if err := clipboard.WriteAll("probe"); err != nil {
		t.Skipf("no system clipboard available: %v", err)
	}
	// Copy via args, then paste via -o.
	if _, errOut, code := runClip(t, "", "hello clip"); code != 0 {
		t.Fatalf("copy failed: code=%d err=%q", code, errOut)
	}
	out, _, code := runClip(t, "", "-o")
	if code != 0 {
		t.Fatalf("paste failed: code=%d", code)
	}
	if out != "hello clip" {
		t.Errorf("round-trip = %q, want 'hello clip'", out)
	}
	// Copy from stdin too.
	runClip(t, "from stdin")
	out, _, _ = runClip(t, "", "-o")
	if out != "from stdin" {
		t.Errorf("stdin round-trip = %q", out)
	}
}
