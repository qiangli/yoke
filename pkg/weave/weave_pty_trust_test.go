//go:build !windows

package weave

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRunWeaveToolPTYClearsTrustPromptReactively(t *testing.T) {
	dir := t.TempDir()
	script := filepath.Join(dir, "fake-tool")
	out := filepath.Join(dir, "said")
	body := "#!/bin/sh\nprintf 'Do you trust this directory?\\n1. Yes\\n'\nIFS= read -r line\nprintf '%s' \"$line\" > \"$1\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	exit, reason, _, _, err := runWeaveToolPTY(
		exec.Command(script, out),
		io.Discard,
		weaveGuards{
			ctlSock:    filepath.Join(dir, "issue.sock"),
			maxRuntime: 5 * time.Second,
		},
	)
	if err != nil {
		t.Fatalf("runWeaveToolPTY: %v", err)
	}
	if exit != 0 || reason != "" {
		t.Fatalf("exit=%d reason=%q, want clean exit", exit, reason)
	}
	got, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(got)) != "1" {
		t.Fatalf("reactive trust clear payload = %q, want 1", got)
	}
}
