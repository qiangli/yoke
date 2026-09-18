package weave

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

func TestAttachRouteLine(t *testing.T) {
	cases := []struct {
		in         string
		wantFrame  string
		wantDetach bool
	}{
		{"/detach", "", true},
		{"  /detach  ", "", true},
		{"/quit", "", true},
		{"\t/quit ", "", true},
		{"take over as orchestrator", "take over as orchestrator", false},
		{"/help", "/help", false},
	}
	for _, c := range cases {
		frame, detach := attachRouteLine(c.in)
		if frame != c.wantFrame || detach != c.wantDetach {
			t.Errorf("attachRouteLine(%q) = (%q, %v), want (%q, %v)", c.in, frame, detach, c.wantFrame, c.wantDetach)
		}
	}
}

func TestRunWeaveAttachPreconditionGuards(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("system git not available; weave attach guards need repo root detection")
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_AGENTIC", "")

	root := t.TempDir()
	runAttachTestGit(t, root, "init", "-q", "-b", "main")
	runAttachTestGit(t, root, "config", "user.email", "attach@test.local")
	runAttachTestGit(t, root, "config", "user.name", "Attach Test")
	if err := os.WriteFile(filepath.Join(root, "README.md"), []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runAttachTestGit(t, root, "add", "-A")
	runAttachTestGit(t, root, "commit", "-q", "-m", "init")
	t.Chdir(root)

	// macOS: t.TempDir() lives under /var, a symlink to /private/var. Inside
	// runWeaveAttach, os.Getwd() returns the RESOLVED /private/var path, which
	// changes the repo-hash weaveQueueDir derives. Resolve the root the same
	// way the runtime does so the test writes the queue where runWeaveAttach
	// will read it (otherwise the lookup misses and we get "not found"/exit 2
	// instead of the precondition error).
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root, err = weaveRepoRoot(cwd)
	if err != nil {
		t.Fatal(err)
	}
	queueDir, err := weaveQueueDir(root)
	if err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(queueDir, "logs", "issue-1.log")
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(logPath, []byte("existing log\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		id       int64
		q        *weaveQueue
		wantCode int
		wantText string
	}{
		{
			name: "non-working item",
			id:   1,
			q: &weaveQueue{NextID: 2, Root: root, Items: []*weaveItem{{
				ID:      1,
				Title:   "not running",
				State:   "todo",
				Created: time.Now().UTC(),
			}}},
			wantCode: weavecli.ExitStateConflict,
			wantText: "has no live subagent",
		},
		{
			name: "empty control socket",
			id:   1,
			q: &weaveQueue{NextID: 2, Root: root, Items: []*weaveItem{{
				ID:         1,
				Title:      "running old wrapper",
				State:      "working",
				WrapperPid: os.Getpid(),
				LogPath:    logPath,
				Created:    time.Now().UTC(),
			}}},
			wantCode: weavecli.ExitStateConflict,
			wantText: "has no control socket",
		},
		{
			name:     "missing issue",
			id:       42,
			q:        &weaveQueue{NextID: 2, Root: root, Items: []*weaveItem{}},
			wantCode: weavecli.ExitInvalidArg,
			wantText: "run #42 not found",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if err := saveWeaveQueue(queueDir, c.q); err != nil {
				t.Fatalf("save queue: %v", err)
			}
			var out, errOut bytes.Buffer
			cmd := &cobra.Command{Use: "attach"}
			cmd.SetOut(&out)
			cmd.SetErr(&errOut)
			cmd.SetIn(strings.NewReader(""))
			err := runWeaveAttach(cmd, c.id, &weaveOutputFlags{})
			if err == nil {
				t.Fatalf("runWeaveAttach returned nil; out=%q err=%q", out.String(), errOut.String())
			}
			var ec interface{ ExitCode() int }
			if !errors.As(err, &ec) {
				t.Fatalf("runWeaveAttach error has no exit code: %v", err)
			}
			if ec.ExitCode() != c.wantCode {
				t.Fatalf("exit code = %d, want %d; err=%q", ec.ExitCode(), c.wantCode, errOut.String())
			}
			if !strings.Contains(errOut.String(), c.wantText) {
				t.Fatalf("error output %q does not contain %q", errOut.String(), c.wantText)
			}
		})
	}
}

func runAttachTestGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}
