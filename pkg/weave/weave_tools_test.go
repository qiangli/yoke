package weave

import (
	"os"
	"runtime"
	"testing"
	"time"
)

// TestInterviewToolSeedsContract checks that interviewing a known tool records
// its headless launch contract (seeded from the cheat-sheet) and persists it —
// the knowledge that prevents a campaign from bare-launching a tool into its
// trust/welcome prompt.
func TestInterviewToolSeedsContract(t *testing.T) {
	dir := t.TempDir()
	p, err := interviewTool(dir, "claude", time.Now(), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.HeadlessArgs) == 0 || p.HeadlessArgs[0] != "--dangerously-skip-permissions" {
		t.Fatalf("claude headless_args = %v, want [--dangerously-skip-permissions …]", p.HeadlessArgs)
	}
	if p.TrustClear != "say:1" {
		t.Fatalf("claude trust_clear = %q, want say:1", p.TrustClear)
	}
	if p.TrustPreseed != ".claude.json" {
		t.Fatalf("claude trust_preseed = %q, want .claude.json", p.TrustPreseed)
	}
	// Persisted + reloadable.
	got, ok := loadToolProfile(dir, "claude")
	if !ok || len(got.HeadlessArgs) != len(p.HeadlessArgs) {
		t.Fatalf("profile did not round-trip: ok=%v", ok)
	}
}

// TestRecordToolOutcome checks the per-role track record accrues.
func TestRecordToolOutcome(t *testing.T) {
	dir := t.TempDir()
	if err := recordToolOutcome(dir, "codex", "coder", true, "fixed the bug"); err != nil {
		t.Fatal(err)
	}
	if err := recordToolOutcome(dir, "codex", "coder", false, "regressed a test"); err != nil {
		t.Fatal(err)
	}
	p, ok := loadToolProfile(dir, "codex")
	if !ok {
		t.Fatal("profile not written")
	}
	r := p.Roles["coder"]
	if r == nil || r.Runs != 2 || r.Passed != 1 || r.Failed != 1 {
		t.Fatalf("coder record = %+v, want runs=2 passed=1 failed=1", r)
	}
}

// TestLiveProbeContractDetectsStaleFlag is the regression for the codex
// --workspace→--sandbox drift: a flag the tool rejects must be caught as STALE,
// while an accepted flag (tool echoes the PROBE_OK prompt) reads as OK.
func TestLiveProbeContractDetectsStaleFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("sh fake-tool")
	}
	dir := t.TempDir()
	script := dir + "/faketool"
	body := "#!/bin/sh\ncase \"$1\" in --bad) echo \"error: unexpected argument '--bad'\" >&2; exit 2;; esac\necho \"$2\"\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	if ok, note := liveProbeContract(script, []string{"--bad"}, time.Now()); ok {
		t.Fatalf("stale flag --bad should be caught, got ok: %s", note)
	}
	if ok, _ := liveProbeContract(script, []string{"--good"}, time.Now()); !ok {
		t.Fatal("accepted flag --good should read OK")
	}
}
