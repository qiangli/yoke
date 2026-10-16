package weave

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const capFinalSHA = "0123456789abcdef0123456789abcdef01234567"

// A card listing no capability IDs is unaffected, even with no repository.
func TestSprintCapabilityGateNoIDsUnaffected(t *testing.T) {
	s := &weaveStory{ID: 7}
	if err := sprintCapabilityGate(s, t.TempDir(), time.Now()); err != nil {
		t.Fatalf("card with no capability IDs refused: %v", err)
	}
}

func TestSprintCapabilityGateRefusePassWaiver(t *testing.T) {
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		cap  sprintCapability
		ok   bool
		want string
	}{
		{"no verdict refuses", sprintCapability{ID: "fleet.otel"}, false, "no PASS verdict"},
		{"pass on final sha", sprintCapability{ID: "fleet.otel", Verdicts: []sprintCapabilityVerdict{{SHA: capFinalSHA, Result: "PASS"}}}, true, ""},
		{"abbreviated pass", sprintCapability{ID: "fleet.otel", Verdicts: []sprintCapabilityVerdict{{SHA: capFinalSHA[:9], Result: "PASS"}}}, true, ""},
		{"pass on older sha refuses", sprintCapability{ID: "fleet.otel", Verdicts: []sprintCapabilityVerdict{{SHA: "fedcba9876543210", Result: "PASS"}}}, false, "no PASS verdict"},
		{"later fail revokes pass", sprintCapability{ID: "fleet.otel", Verdicts: []sprintCapabilityVerdict{{SHA: capFinalSHA, Result: "PASS"}, {SHA: capFinalSHA, Result: "FAIL"}}}, false, "no PASS verdict"},
		{"too-short sha refuses", sprintCapability{ID: "fleet.otel", Verdicts: []sprintCapabilityVerdict{{SHA: "0123", Result: "PASS"}}}, false, "no PASS verdict"},
		{"unexpired waiver passes", sprintCapability{ID: "fleet.otel", Waiver: &sprintCapabilityWaiver{Reason: "proof host down", By: "mgr", Expires: now.Add(time.Hour)}}, true, ""},
		{"expired waiver refuses", sprintCapability{ID: "fleet.otel", Waiver: &sprintCapabilityWaiver{Reason: "proof host down", By: "mgr", Expires: now.Add(-time.Minute)}}, false, "expired"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := &weaveStory{ID: 7, Capabilities: []sprintCapability{tc.cap}}
			err := sprintCapabilityGateFor(s, capFinalSHA, now)
			if tc.ok && err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !tc.ok {
				if err == nil {
					t.Fatal("gate passed, want refusal")
				}
				if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "fleet.otel") {
					t.Fatalf("refusal %q must name the ID and contain %q", err, tc.want)
				}
			}
		})
	}
}

// The verbs record onto the card and the gate reads the result against the
// HEAD of a real (scratch) checkout: refuse → PASS verdict → end allowed; a
// second ID refused until a recorded, expiring waiver covers it.
func TestSprintCapabilityVerbsDriveGate(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_HOME", filepath.Join(home, ".bashy"))
	repo := t.TempDir()
	git := func(args ...string) string {
		t.Helper()
		out, err := gitOutputForTest(repo, args...)
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(out)
	}
	git("init", "-q")
	git("config", "user.name", "test")
	git("config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", ".")
	git("commit", "-qm", "base")
	head := git("rev-parse", "HEAD")

	run := func(args ...string) string {
		t.Helper()
		out, code := runSprint(t, args...)
		if code != 0 {
			t.Fatalf("sprint %v exit=%d: %s", args, code, out)
		}
		return out
	}
	card := func() *weaveStory {
		t.Helper()
		dir, err := sprintStoreDir()
		if err != nil {
			t.Fatal(err)
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			t.Fatal(err)
		}
		return findWeaveStory(q, 1)
	}

	run("add", "capability gate")
	if err := sprintCapabilityGate(card(), repo, time.Now()); err != nil {
		t.Fatalf("card with no IDs refused: %v", err)
	}
	run("capability", "add", "1", "fleet.otel")
	run("capability", "add", "1", "fleet.podman")
	if err := sprintCapabilityGate(card(), repo, time.Now()); err == nil {
		t.Fatal("gate passed with two unproven capability IDs")
	}
	run("capability", "verdict", "1", "fleet.otel", "--sha", head, "--result", "pass", "-m", "tier smoke green")
	err := sprintCapabilityGate(card(), repo, time.Now())
	if err == nil || strings.Contains(err.Error(), "fleet.otel") || !strings.Contains(err.Error(), "fleet.podman") {
		t.Fatalf("after otel PASS want refusal naming only fleet.podman, got %v", err)
	}
	if out, code := runSprint(t, "capability", "waive", "1", "fleet.podman", "--reason", "no reason duration"); code == 0 {
		t.Fatalf("waiver without --for accepted: %s", out)
	}
	run("capability", "waive", "1", "fleet.podman", "--reason", "proof host offline", "--for", "1h")
	if err := sprintCapabilityGate(card(), repo, time.Now()); err != nil {
		t.Fatalf("PASS + unexpired waiver still refused: %v", err)
	}
	if err := sprintCapabilityGate(card(), repo, time.Now().Add(2*time.Hour)); err == nil {
		t.Fatal("expired waiver still accepted")
	}
	// A new commit moves the final SHA: the old PASS no longer counts.
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("commit", "-qam", "next")
	if err := sprintCapabilityGate(card(), repo, time.Now()); err == nil || !strings.Contains(err.Error(), "fleet.otel") {
		t.Fatalf("PASS on a superseded SHA accepted: %v", err)
	}
	c := card()
	if c.Capabilities[1].Waiver == nil || c.Capabilities[1].Waiver.Reason != "proof host offline" {
		t.Fatalf("waiver not recorded on the card: %+v", c.Capabilities)
	}
}

// The gate must sit on the `sprint end` path itself.
func TestSprintEndCallsCapabilityGate(t *testing.T) {
	src, err := os.ReadFile("weave_story_box.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(src), "sprintCapabilityGate(s, gateDir, now)") {
		t.Error("sprint end does not call the capability close-out gate")
	}
}
