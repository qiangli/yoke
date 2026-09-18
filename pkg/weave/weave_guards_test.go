package weave

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The --max-runtime watchdog must kill a hung subagent after the ceiling.
// Regression for the dogfood miss where a 35m ceiling went unenforced
// across a laptop suspend: the guard used a monotonic timer (time.After)
// that pauses during system sleep; it now polls a real wall-clock deadline.
// (This test doesn't simulate sleep — it just pins that the guard fires.)
func TestRunWeaveToolPTYMaxRuntimeFires(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("PTY watchdog is unix-only")
	}
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not on PATH")
	}
	cmd := exec.Command("sleep", "60")
	type res struct {
		exit      int
		reason    string
		coachMode string
		err       error
	}
	done := make(chan res, 1)
	go func() {
		exit, reason, _, coachMode, err := runWeaveToolPTY(cmd, io.Discard, weaveGuards{maxRuntime: time.Second})
		done <- res{exit, reason, coachMode, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("runWeaveToolPTY: %v", r.err)
		}
		if r.exit == 0 {
			t.Errorf("want non-zero exit after watchdog kill, got 0")
		}
		if !strings.Contains(r.reason, "max-runtime") {
			t.Errorf("kill reason = %q, want it to mention max-runtime", r.reason)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("max-runtime guard did not fire within 20s for a 1s ceiling on a 60s sleep")
	}
}

func TestParseWeaveMemLimit(t *testing.T) {
	cases := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"16g", 16 << 30, false},
		{"16G", 16 << 30, false},
		{"16gb", 16 << 30, false},
		{"512m", 512 << 20, false},
		{"1024k", 1024 << 10, false},
		{"123", 123, false},
		{" 2g ", 2 << 30, false},
		{"-1g", 0, true},
		{"abc", 0, true},
		{"g", 0, true},
	}
	for _, c := range cases {
		got, err := parseWeaveMemLimit(c.in)
		if c.wantErr {
			if err == nil {
				t.Errorf("parseWeaveMemLimit(%q): expected error, got %d", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseWeaveMemLimit(%q): unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("parseWeaveMemLimit(%q) = %d, want %d", c.in, got, c.want)
		}
	}
}

func TestTailOffset(t *testing.T) {
	write := func(content string) *os.File {
		t.Helper()
		f, err := os.CreateTemp(t.TempDir(), "tail")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(content); err != nil {
			t.Fatal(err)
		}
		return f
	}
	cases := []struct {
		name    string
		content string
		n       int
		want    string // expected substring from offset to EOF
	}{
		{"whole file when n exceeds lines", "a\nb\nc\n", 10, "a\nb\nc\n"},
		{"last two lines", "a\nb\nc\n", 2, "b\nc\n"},
		{"last line with trailing newline", "a\nb\nc\n", 1, "c\n"},
		{"last line without trailing newline", "a\nb\nc", 1, "c"},
		{"zero lines returns end", "a\nb\n", 0, ""},
		{"empty file", "", 3, ""},
		{"single line no newline", "abc", 1, "abc"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := write(c.content)
			defer f.Close()
			off, err := tailOffset(f, c.n)
			if err != nil {
				t.Fatalf("tailOffset: %v", err)
			}
			got := c.content[off:]
			if got != c.want {
				t.Errorf("tailOffset(%q, %d): got %q, want %q", c.content, c.n, got, c.want)
			}
		})
	}
}

func TestWeaveDurationCol(t *testing.T) {
	now := time.Now().UTC()
	cases := []struct {
		name string
		it   weaveItem
		want string
	}{
		{"never started", weaveItem{State: "todo"}, "-"},
		{"finished seconds", weaveItem{State: "submitted", StartedAt: now.Add(-90 * time.Second), FinishedAt: now.Add(-48 * time.Second)}, "42s"},
		{"finished minutes", weaveItem{State: "done", StartedAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-7*time.Minute - 55*time.Second)}, "2m05s"},
		{"finished hours", weaveItem{State: "failed", StartedAt: now.Add(-3 * time.Hour), FinishedAt: now.Add(-95 * time.Minute)}, "1h25m"},
		{"started but no finish, not working", weaveItem{State: "submitted", StartedAt: now.Add(-time.Minute)}, "-"},
		{"clock skew negative", weaveItem{State: "done", StartedAt: now, FinishedAt: now.Add(-time.Minute)}, "-"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := weaveDurationCol(&c.it); got != c.want {
				t.Errorf("weaveDurationCol(%s) = %q, want %q", c.name, got, c.want)
			}
		})
	}
	w := weaveItem{State: "working", StartedAt: now.Add(-5 * time.Second)}
	if got := weaveDurationCol(&w); got == "-" || got == "0s" {
		t.Errorf("working item should show live duration, got %q", got)
	}
}

func TestSafeRemoveWorkspaceRefusesLiveWrapper(t *testing.T) {
	queueDir := filepath.Join(t.TempDir(), "queue")
	workspace := filepath.Join(queueDir, "workspaces", "issue-1")
	if err := os.MkdirAll(workspace, 0o755); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	q := &weaveQueue{NextID: 2, Items: []*weaveItem{{
		ID:         1,
		Title:      "live",
		State:      "working",
		Workspace:  workspace,
		WrapperPid: os.Getpid(),
		Created:    time.Now(),
	}}}
	if err := os.MkdirAll(queueDir, 0o755); err != nil {
		t.Fatalf("mkdir queue: %v", err)
	}
	if err := saveWeaveQueue(queueDir, q); err != nil {
		t.Fatalf("save queue: %v", err)
	}
	err := safeRemoveWorkspace(queueDir, workspace)
	if err == nil {
		t.Fatalf("expected live-wrapper refusal")
	}
	if !strings.Contains(err.Error(), "live wrapper pid") {
		t.Fatalf("expected live-wrapper error, got %v", err)
	}
	if _, statErr := os.Stat(workspace); statErr != nil {
		t.Fatalf("workspace should remain: %v", statErr)
	}
}
