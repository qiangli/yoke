// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package resources

import (
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/artifact"
)

func TestCollectInodesPlatformHonesty(t *testing.T) {
	sys, err := Collect(t.Context(), Options{Interval: 0})
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	switch runtime.GOOS {
	case "darwin", "linux":
		sawInodes := false
		for _, d := range sys.Disks {
			if d.InodesTotal > 0 {
				sawInodes = true
				if d.InodesFree > d.InodesTotal {
					t.Errorf("mount %s InodesFree (%d) > InodesTotal (%d)", d.Mount, d.InodesFree, d.InodesTotal)
				}
				if d.InodesUsed != d.InodesTotal-d.InodesFree {
					t.Errorf("mount %s InodesUsed = %d, want %d", d.Mount, d.InodesUsed, d.InodesTotal-d.InodesFree)
				}
			}
		}
		if !sawInodes {
			t.Fatalf("expected at least one filesystem on %s to report InodesTotal > 0; disks=%+v", runtime.GOOS, sys.Disks)
		}
	case "windows":
		for _, d := range sys.Disks {
			if d.InodesTotal != 0 || d.InodesUsed != 0 || d.InodesFree != 0 || d.InodesUsedPercent != 0 {
				t.Errorf("windows disk invented inode telemetry: %+v", d)
			}
			raw, err := json.Marshal(d)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(raw), "inodes_") {
				t.Errorf("expected zero inode fields omitted on windows, got %s", raw)
			}
		}
	}
}

func TestEvaluateInodeExhaustionThresholds(t *testing.T) {
	now := time.Now().UTC()

	if err := ValidateWatchOverride("inode-warn-used", "80"); err != nil {
		t.Fatalf("ValidateWatchOverride inode-warn-used: %v", err)
	}
	if err := ValidateWatchOverride("inode-crit-used", "92"); err != nil {
		t.Fatalf("ValidateWatchOverride inode-crit-used: %v", err)
	}
	cfg := OverrideOnly(DefaultWatchConfig(), map[string]string{
		"inode-warn-used": "85",
		"inode-crit-used": "95",
	})

	// 86% inodes used with ample free bytes -> warning on inode-used.
	wWarn := NewWatcher(cfg)
	sysWarn := fakeSystem()
	dWarn := fakeDisk("/data", 30, 500*gib)
	dWarn.InodesTotal = 1_000_000
	dWarn.InodesUsed = 860_000
	dWarn.InodesFree = 140_000
	dWarn.InodesUsedPercent = 86.0
	sysWarn.Disks = []Disk{dWarn}
	evs := wWarn.Observe(now, sysWarn)
	var gotWarn *Event
	for i := range evs {
		if evs[i].Metric == metricInodeUsed {
			gotWarn = &evs[i]
		}
	}
	if gotWarn == nil || gotWarn.Level != LevelWarning {
		t.Fatalf("expected inode-used warning at 86%%, got %v", summarize(evs))
	}

	// 96% inodes used -> critical on inode-used.
	wCrit := NewWatcher(cfg)
	sysCrit := fakeSystem()
	dCrit := fakeDisk("/data", 30, 500*gib)
	dCrit.InodesTotal = 1_000_000
	dCrit.InodesUsed = 960_000
	dCrit.InodesFree = 40_000
	dCrit.InodesUsedPercent = 96.0
	sysCrit.Disks = []Disk{dCrit}
	evsCrit := wCrit.Observe(now, sysCrit)
	var gotCrit *Event
	for i := range evsCrit {
		if evsCrit[i].Metric == metricInodeUsed {
			gotCrit = &evsCrit[i]
		}
	}
	if gotCrit == nil || gotCrit.Level != LevelCritical {
		t.Fatalf("expected inode-used critical at 96%%, got %v", summarize(evsCrit))
	}
}

func TestPressureCLIEvaluateStatusAndAlerts(t *testing.T) {
	watchRoots(t)
	artifactRoot := t.TempDir()

	// 1. Initial status before evaluation -> normal, advisory.
	statusOut, err := run(t, "pressure", "status", "--artifact-root", artifactRoot, "--json")
	if err != nil {
		t.Fatalf("pressure status: %v\n%s", err, statusOut)
	}
	var initState artifact.PressureStateV1
	if err := json.Unmarshal([]byte(statusOut), &initState); err != nil {
		t.Fatalf("decode initial status: %v\n%s", err, statusOut)
	}
	if initState.Activated || initState.Level != artifact.PressureNormal || initState.GrowthBlocked {
		t.Fatalf("unexpected initial state: %+v", initState)
	}

	// Stub WatchSampler with a critical disk sample covering artifactRoot.
	prevSampler := WatchSampler
	WatchSampler = func(context.Context) (*System, error) {
		sys := fakeSystem()
		d := fakeDisk(artifactRoot, 96, 200*gib)
		d.FreeBytes = 8 * gib
		sys.Disks = []Disk{d}
		return sys, nil
	}
	t.Cleanup(func() { WatchSampler = prevSampler })

	// 2. Advisory evaluate (without --activate) -> observes critical, GrowthBlocked stays false.
	advOut, err := run(t, "pressure", "evaluate", "--artifact-root", artifactRoot, "--json")
	if err != nil {
		t.Fatalf("advisory evaluate: %v\n%s", err, advOut)
	}
	var advRes artifact.PressureResult
	if err := json.Unmarshal([]byte(advOut), &advRes); err != nil {
		t.Fatalf("decode advisory evaluate: %v\n%s", err, advOut)
	}
	if advRes.State.Activated || advRes.State.GrowthBlocked || advRes.State.ObservedLevel != artifact.PressureCritical {
		t.Fatalf("unexpected advisory evaluate result: %+v", advRes.State)
	}

	// 3. Activated evaluate (--activate) -> GrowthBlocked=true, ThrottleConcurrency=1, UnsatisfiedFloor=true.
	actOut, err := run(t, "pressure", "evaluate", "--artifact-root", artifactRoot, "--activate", "--json")
	if err != nil {
		t.Fatalf("activated evaluate: %v\n%s", err, actOut)
	}
	var actRes artifact.PressureResult
	if err := json.Unmarshal([]byte(actOut), &actRes); err != nil {
		t.Fatalf("decode activated evaluate: %v\n%s", err, actOut)
	}
	if !actRes.State.Activated || actRes.State.Level != artifact.PressureCritical || !actRes.State.GrowthBlocked || !actRes.State.UnsatisfiedFloor {
		t.Fatalf("unexpected activated evaluate result: %+v", actRes.State)
	}

	// 4. Persisted status reflects critical blocked state.
	persistedOut, err := run(t, "pressure", "status", "--artifact-root", artifactRoot, "--json")
	if err != nil {
		t.Fatalf("persisted status: %v\n%s", err, persistedOut)
	}
	var persisted artifact.PressureStateV1
	if err := json.Unmarshal([]byte(persistedOut), &persisted); err != nil {
		t.Fatalf("decode persisted status: %v", err)
	}
	if !persisted.Activated || !persisted.GrowthBlocked || persisted.ThrottleConcurrency != 1 {
		t.Fatalf("unexpected persisted status: %+v", persisted)
	}

	// 5. Alerts command reads alerts.jsonl.
	alertsOut, err := run(t, "pressure", "alerts", "--artifact-root", artifactRoot, "--limit", "10", "--json")
	if err != nil {
		t.Fatalf("pressure alerts: %v\n%s", err, alertsOut)
	}
	var alerts []artifact.AlertRecordV1
	if err := json.Unmarshal([]byte(alertsOut), &alerts); err != nil {
		t.Fatalf("decode alerts: %v\n%s", err, alertsOut)
	}
	if len(alerts) == 0 {
		t.Fatal("expected alerts recorded in alerts.jsonl")
	}
}
