package weave

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet"
)

// TestFleetRowDetectsMissingTool is the regression for the launch-failure
// class: a tool whose binary does not resolve on PATH MUST be reported not
// found and not available, so an orchestrator preflighting with `weave fleet`
// skips it instead of wasting a 0-second `weave start` (which is exactly the
// failure that motivated existence detection).
func TestFleetRowDetectsMissingTool(t *testing.T) {
	dir := t.TempDir()
	cache := map[string]fleetProbeEntry{}
	r, dirty := fleetRowFor(dir, "weave-bogus-tool-xyzzy-not-installed", time.Now(), true, cache)
	if r.Found {
		t.Fatalf("missing tool reported Found=true")
	}
	if r.Available {
		t.Fatalf("missing tool reported Available=true; an orchestrator would waste a launch on it")
	}
	if r.Probed {
		t.Fatalf("missing tool should not be capability-probed")
	}
	if dirty {
		t.Fatalf("missing tool should not dirty the probe cache")
	}
}

// TestFleetRowFindsInstalledTool checks the positive path: a real executable on
// PATH is installed with its resolved path. Without --probe, it is deliberately
// not claimed usable.
func TestFleetRowFindsInstalledTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("temp-exec + PATH shimming is fiddly on windows; existence logic is shared")
	}
	bindir := t.TempDir()
	tool := "weave-fake-tool"
	exe := filepath.Join(bindir, tool)
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho v1.2.3\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+string(os.PathListSeparator)+os.Getenv("PATH"))

	r, _ := fleetRowFor(t.TempDir(), tool, time.Now(), false, map[string]fleetProbeEntry{})
	if !r.Found || r.Available {
		t.Fatalf("installed tool: Found=%v Available=%v, want true false", r.Found, r.Available)
	}
	if r.Path != exe {
		t.Fatalf("resolved path = %q, want %q", r.Path, exe)
	}
}

// TestFleetReportsQuotaExhaustedNotAvailable is THE regression for the lying
// availability check (observed live 2026-07-21): `weave fleet` printed "codex
// available (/opt/homebrew/bin/codex)" while codex's quota was exhausted until
// Jul 24 — so the orchestrator dispatched review after review into the same
// wall. With a quota-exhausted cooldown record seeded (what the last real
// invocation now writes), fleet must report the member QUOTA-EXHAUSTED WITH
// the reset time — never "available" — until the reset passes.
func TestFleetReportsQuotaExhaustedNotAvailable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("temp-exec + PATH shimming is fiddly on windows; the logic is shared")
	}
	bindir := t.TempDir()
	exe := filepath.Join(bindir, "codex")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+string(os.PathListSeparator)+os.Getenv("PATH"))

	dir := t.TempDir()
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local)
	reset := time.Date(2026, 7, 24, 21, 45, 0, 0, time.Local)
	if err := recordToolCooldownCause(dir, "codex", reset, weaveCooldownQuota); err != nil {
		t.Fatalf("seed quota-exhausted record: %v", err)
	}

	r, _ := fleetRowFor(dir, "codex", now, false, map[string]fleetProbeEntry{})
	if !r.Found {
		t.Fatal("test shim not found on PATH; fixture broken")
	}
	if r.Available {
		t.Fatal("quota-exhausted member reported Available=true; the orchestrator would dispatch into the exhausted quota again")
	}
	if r.CooldownCause != weaveCooldownQuota {
		t.Fatalf("CooldownCause=%q want %q", r.CooldownCause, weaveCooldownQuota)
	}
	line := r.statusLine("codex")
	if !strings.Contains(line, "QUOTA-EXHAUSTED") {
		t.Fatalf("status line lacks the distinct QUOTA-EXHAUSTED status: %q", line)
	}
	if !strings.Contains(line, "2026-07-24 21:45") {
		t.Fatalf("status line lacks the reset time: %q", line)
	}
	if strings.Contains(strings.ToLower(line), "available") {
		t.Fatalf("status line must not read as available: %q", line)
	}

	// Once the reset passes, the member re-engages without manual cleanup.
	r2, _ := fleetRowFor(dir, "codex", reset.Add(time.Minute), false, map[string]fleetProbeEntry{})
	if !r2.Found || r2.Available {
		t.Fatal("member should return to installed/unprobed after its reset passed")
	}
}

// TestPairQuotaThrottleRecordsReviewerCooldown pins the write half: a pair
// review that harness-errors on the reviewer's quota message must land the
// reviewer's TOOL on cooldown with the parsed reset, so the next `weave fleet`
// holds instead of burning another workspace pull (runs #140/#146).
func TestPairQuotaThrottleRecordsReviewerCooldown(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 7, 21, 10, 0, 0, 0, time.Local)
	pr := weavePairReviewResult{
		ReviewAgent: "codex:gpt-5.5",
		Verdict:     weavePairHarnessError,
		ExitCode:    weavePairHarnessErrorExit,
		Reason:      "bashy pair exited 1: ERROR: You've hit your usage limit. Visit https://chatgpt.com/codex/settings/usage to purchase more credits or try again at Jul 24th, 2026 9:45 PM.",
	}
	weaveRecordPairThrottle(dir, pr, now)

	until, cause, ok := toolCooldownStatus(dir, "codex")
	if !ok {
		t.Fatal("no cooldown recorded for the reviewer's tool")
	}
	if want := time.Date(2026, 7, 24, 21, 45, 0, 0, time.Local); !until.Equal(want) {
		t.Fatalf("cooldown until %v, want %v", until, want)
	}
	if cause != weaveCooldownQuota {
		t.Fatalf("cause=%q want %q", cause, weaveCooldownQuota)
	}

	// A non-throttle harness error must NOT invent a cooldown.
	dir2 := t.TempDir()
	pr2 := weavePairReviewResult{
		ReviewAgent: "codex:gpt-5.5",
		Verdict:     weavePairHarnessError,
		ExitCode:    weavePairHarnessErrorExit,
		Reason:      "bashy pair returned malformed verdict",
	}
	weaveRecordPairThrottle(dir2, pr2, now)
	if _, _, ok := toolCooldownStatus(dir2, "codex"); ok {
		t.Fatal("non-throttle harness error must not record a cooldown")
	}

	// A pass must never cool the reviewer down, whatever its output mentions.
	dir3 := t.TempDir()
	pr3 := weavePairReviewResult{
		ReviewAgent: "codex:gpt-5.5",
		Verdict:     weavePairPass,
		ExitCode:    weavePairPassExit,
		Reason:      "pair attacked the change and the gate stayed green",
		Output:      "note: near the usage limit",
	}
	weaveRecordPairThrottle(dir3, pr3, now)
	if _, _, ok := toolCooldownStatus(dir3, "codex"); ok {
		t.Fatal("a passing pair review must not record a cooldown")
	}
}

// A smoke probe executes the exact path LookPath resolved for this host, not
// the executable name declared by the catalog.
func TestSmokeProbeUsesResolvedPathAndRejectsExitZeroError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	cat := pinFleetWith(t)
	if err := cat.SaveTool(fleet.Tool{Name: "smoke", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "smoke", Launch: fleet.ToolLaunch{Exec: "smoke {prompt}"}}}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "resolved-smoke")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho 'Error: provider unavailable'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	ent := probeToolCapability("smoke", path, time.Now())
	if ent.Capable {
		t.Fatalf("exit-0 error payload was classified usable: %+v", ent)
	}
}

func TestFleetReportsFailingSmokeAsUnusable(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	cat := pinFleetWith(t)
	if err := cat.SaveTool(fleet.Tool{Name: "failing-smoke", Kind: fleet.ToolKindCLI,
		CLI: fleet.ToolCLI{Binary: "failing-smoke", Launch: fleet.ToolLaunch{Exec: "failing-smoke {prompt}"}}}); err != nil {
		t.Fatal(err)
	}
	bindir := t.TempDir()
	if err := os.WriteFile(filepath.Join(bindir, "failing-smoke"), []byte("#!/bin/sh\necho 'Error: unexpected server error'\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+string(os.PathListSeparator)+os.Getenv("PATH"))
	now := time.Date(2026, 8, 25, 10, 30, 0, 0, time.Local)
	r, dirty := fleetRowFor(t.TempDir(), "failing-smoke", now, true, map[string]fleetProbeEntry{})
	if !dirty || !r.Found || !r.Probed || r.Capable || r.Available {
		t.Fatalf("exit-0 error probe row = %+v", r)
	}
	line := r.statusLine("failing-smoke")
	if !strings.Contains(line, "UNUSABLE") || !strings.Contains(line, "probed 2026-08-25 10:30") {
		t.Fatalf("failing smoke must show verdict and timestamp: %q", line)
	}
}

func TestFleetDefaultCheckDoesNotExecuteTool(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("shell fixture")
	}
	bindir, dir := t.TempDir(), t.TempDir()
	marker := filepath.Join(dir, "executed")
	exe := filepath.Join(bindir, "quiet-smoke")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\ntouch '"+marker+"'\necho SMOKE-OK\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bindir+string(os.PathListSeparator)+os.Getenv("PATH"))
	r, _ := fleetRowFor(dir, "quiet-smoke", time.Now(), false, map[string]fleetProbeEntry{})
	if !r.Found || r.Probed || r.Available {
		t.Fatalf("default row = %+v; want installed, unprobed, not claimed usable", r)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("default listing executed the tool: %v", err)
	}
}
