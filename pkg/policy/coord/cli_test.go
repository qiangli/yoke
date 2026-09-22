package coord

import (
	"bytes"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func TestClaimNamedResourceCLI(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	t.Setenv("BASHY_AGENT_ID", "codex-b")
	t.Setenv(EpisodeEnv, "ep-bbb")

	run := func(args ...string) string {
		t.Helper()
		cmd := NewClaimCmd(func() []string { return []string{"/w/bashy"} })
		var out bytes.Buffer
		cmd.SetOut(&out)
		cmd.SetErr(&out)
		cmd.SetArgs(args)
		if err := cmd.Execute(); err != nil {
			t.Fatalf("claim %v: %v\n%s", args, err, out.String())
		}
		return out.String()
	}
	if out := run("do1", "--intent", "leaf replay"); !strings.Contains(out, "do1 held on") {
		t.Fatalf("claim output = %q", out)
	}
	if out := run("list"); !strings.Contains(out, "do1") || !strings.Contains(out, "lease") {
		t.Fatalf("list output = %q", out)
	}
	run("release", "do1")
	claims, err := List(dir)
	if err != nil || len(claims) != 0 {
		t.Fatalf("after release: claims=%#v err=%v", claims, err)
	}
}

func TestClaimChildExitStatusPropagatesAndReleases(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", dir)
	t.Setenv("BASHY_AGENT_ID", "codex-b")
	t.Setenv(EpisodeEnv, "ep-bbb")
	cmd := NewClaimCmd(func() []string { return []string{"/w/bashy"} })
	cmd.SetArgs([]string{"do1", "--", "sh", "-c", "exit 23"})
	err := cmd.Execute()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 23 {
		t.Fatalf("exit = %T %v, want child status 23", err, err)
	}
	c, l, err := AcquireAttached(dir, "do1", agentA(), "after child", 0)
	if err != nil {
		t.Fatalf("child-scoped hold was not released: %v", err)
	}
	if err := ReleaseAttached(dir, c, l); err != nil {
		t.Fatal(err)
	}
}

func TestClaimRequestNotifiesConflictingOwnerWithoutStealing(t *testing.T) {
	coordDir := t.TempDir()
	t.Setenv("BASHY_COORD_DIR", coordDir)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_PRINCIPAL", "")
	t.Setenv("BASHY_AGENT_ID", "codex-b")
	t.Setenv(EpisodeEnv, "ep-bbb")

	if _, err := Acquire(coordDir, []string{"/w/bashy"}, agentA(), "integrating", false); err != nil {
		t.Fatal(err)
	}
	cmd := NewClaimCmd(func() []string { return []string{"/w/bashy"} })
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"request", "-m", "please merge my reviewed commit"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "sent to claude-a") || !strings.Contains(out.String(), "lock remains enforced") {
		t.Fatalf("request output = %q", out.String())
	}
	events, err := room.Timeline(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].To != "claude-a" || events[0].Principal != "codex-b" || events[0].Body != "please merge my reviewed commit" || events[0].Priority != "interrupt" {
		t.Fatalf("request event = %#v", events)
	}
	if _, err := Acquire(coordDir, []string{"/w/bashy"}, agentB(), "write anyway", false); err == nil {
		t.Fatal("request silently stole or released the owner's claim")
	}
}
