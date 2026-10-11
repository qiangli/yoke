package handoff

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
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

func TestHandoffReadsPortableEnvelopeFromStdin(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", home)
	t.Setenv("BASHY_HANDOFF_DIR", t.TempDir())
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "init"}} {
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

	const envelope = `{"tool":"claude","command":"plan","capability":"planning","outcome":"success","text":"plan artifact","episode":"ep-plan"}`
	h := NewHandoffCmd()
	var out bytes.Buffer
	h.SetOut(&out)
	h.SetErr(&out)
	h.SetIn(strings.NewReader(envelope + "\n"))
	h.SetArgs([]string{"--json", "-m", "-", "--next", "codex implements the cited plan"})
	if err := h.Execute(); err != nil {
		t.Fatalf("handoff stdin: %v\n%s", err, out.String())
	}
	var rec Record
	if err := json.Unmarshal(out.Bytes(), &rec); err != nil {
		t.Fatalf("handoff JSON: %v\n%s", err, out.String())
	}
	if rec.Continuity != envelope || rec.NextAction != "codex implements the cited plan" {
		t.Fatalf("handoff did not carry envelope: %+v", rec)
	}
}
