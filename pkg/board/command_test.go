package board

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func emptySources() []Source { return []Source{} }

func runCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	cmd := NewStewardCommand(func(w io.Writer) error {
		_, err := io.WriteString(w, "existing steward skill\n")
		return err
	}, emptySources())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func TestStewardNamespaceKeepsBareAndSkill(t *testing.T) {
	for _, args := range [][]string{nil, {"skill"}} {
		out, err := runCommand(t, args...)
		if err != nil || out != "existing steward skill\n" {
			t.Fatalf("steward %v: out=%q err=%v", args, out, err)
		}
	}
}

func TestStewardHelpNamesResourceDuty(t *testing.T) {
	out, err := runCommand(t, "--help")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Resource health is a standing steward duty", "resources,utilization", "never prune shared container stores"} {
		if !strings.Contains(out, want) {
			t.Fatalf("steward help missing %q:\n%s", want, out)
		}
	}
}

func TestDashboardFormatsAndOut(t *testing.T) {
	out, err := runCommand(t, "dashboard", "--json")
	if err != nil || !strings.Contains(out, `"schema_version": "bashy-board-v1"`) {
		t.Fatalf("dashboard --json: %v\n%s", err, out)
	}
	out, err = runCommand(t, "dashboard", "--html")
	if err != nil || !strings.HasPrefix(out, "<!doctype html>") {
		t.Fatalf("dashboard --html: %v\n%s", err, out)
	}
	path := filepath.Join(t.TempDir(), "board.html")
	out, err = runCommand(t, "dashboard", "--html", "--out", path)
	if err != nil || out != "" {
		t.Fatalf("dashboard --out: out=%q err=%v", out, err)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.HasPrefix(raw, []byte("<!doctype html>")) {
		t.Fatalf("dashboard output file: %v", err)
	}
}

func TestDashboardRejectsUnknownPanel(t *testing.T) {
	_, err := runCommand(t, "dashboard", "--expand", "future")
	if err == nil || !strings.Contains(err.Error(), "unknown panel") {
		t.Fatalf("error = %v", err)
	}
}

// board is a projection over weave/sprint/fleet state, not an initializer for
// those stores. In particular, rendering it from a repository with no queue
// must not mint the per-cwd queue tag that repeated dashboard polling used to
// recreate after cleanup.
func TestBoardReadDoesNotCreateWeaveState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	repo := filepath.Join(t.TempDir(), "repo")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	t.Chdir(repo)

	cmd := NewCommand([]Source{NewWeaveSource(), NewSprintSource(), NewFleetSource()})
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--json"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("board --json: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".bashy", "weave")); !os.IsNotExist(err) {
		t.Fatalf("read-only board created weave state (stat err %v)", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".bashy", "sprint")); !os.IsNotExist(err) {
		t.Fatalf("read-only board created sprint state (stat err %v)", err)
	}
}
