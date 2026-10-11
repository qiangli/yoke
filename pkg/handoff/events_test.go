package handoff

import (
	"bytes"
	"os"
	"os/exec"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

func TestHandoffCreateAndClaimEmitHostEvents(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
	t.Setenv("BASHY_HANDOFF_DIR", t.TempDir())
	repo := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		c := exec.Command("git", args...)
		c.Dir = repo
		if out, err := c.CombinedOutput(); err != nil {
			t.Skipf("git unavailable: %v\n%s", err, out)
		}
	}
	prev, _ := os.Getwd()
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(prev) })

	h := NewHandoffCmd()
	var out bytes.Buffer
	h.SetOut(&out)
	h.SetErr(&out)
	h.SetArgs([]string{"-m", "parking the events work"})
	if err := h.Execute(); err != nil {
		t.Fatalf("handoff: %v\n%s", err, out.String())
	}
	r := NewResumeCmd()
	out.Reset()
	r.SetOut(&out)
	r.SetErr(&out)
	r.SetArgs([]string{"--claim"})
	if err := r.Execute(); err != nil {
		t.Fatalf("resume --claim: %v\n%s", err, out.String())
	}

	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	var verbs []string
	for i, e := range events {
		if e.Seq != int64(i+1) {
			t.Fatalf("non-contiguous seq: %+v", events)
		}
		if e.Type == room.EventHandoff {
			verbs = append(verbs, e.Topic)
		}
	}
	if len(verbs) != 2 || verbs[0] != "created" || verbs[1] != "claimed" {
		t.Fatalf("handoff events = %v (all: %+v)", verbs, events)
	}
}
