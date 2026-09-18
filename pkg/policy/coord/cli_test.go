package coord

import (
	"bytes"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

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
