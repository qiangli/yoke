// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"runtime"
	"testing"
	"time"

	shape "github.com/qiangli/yoke/pkg/shape/state"
)

func observeBroken(t *testing.T, s *incidentStore, base string) Incident {
	t.Helper()
	in, start, err := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/list", Status: 500, Message: "unit list failed", Evidence: "exit 1", BaseRev: base}, time.Now())
	if err != nil || !start {
		t.Fatalf("observe: %v %v", start, err)
	}
	return in
}

func autoEvents(t *testing.T, home string) []string {
	t.Helper()
	log, err := shape.ReadAutoLog(home)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, d := range log {
		if d.Time.IsZero() {
			t.Fatalf("decision without a time: %+v", d)
		}
		if d.Source == "incident" && (d.App != "notes" || d.Ref == "") {
			t.Fatalf("incident decision without app or ref: %+v", d)
		}
		out = append(out, d.Event+":"+d.Result)
	}
	return out
}

func sameEvents(t *testing.T, got []string, want ...string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("decision log = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("decision log = %v, want %v", got, want)
		}
	}
}

// The host bound wins: shaped.json may lower it, never raise it and never
// make it unlimited. The loop stops at the host bound.
func TestRepairHostBoundWins(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	for _, tc := range []struct {
		name, manifest string
		host           *int
		want           int
	}{
		{"larger app value", `{"repair": {"max_attempts": 5}}`, repairIntPtr(1), 1},
		{"unlimited app value", `{"repair": {"max_attempts": "unlimited"}}`, repairIntPtr(1), 1},
		{"unlimited over the default", `{"repair": {"max_attempts": "unlimited"}}`, nil, DefaultMaxRepairAttempts},
		{"smaller app value", `{"repair": {"max_attempts": 1}}`, repairIntPtr(3), 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			delegate := &repairShaper{mode: "fail"}
			app, base := repairLimitFixture(t, delegate, tc.manifest)
			app.opts.RepairMaxAttempts = tc.host
			if got := app.repairLimitForRev(base); got != tc.want {
				t.Fatalf("limit = %d, want %d", got, tc.want)
			}
			s := repairLimitStore(t, app, "device:alice", base)
			in := observeBroken(t, s, base)
			app.repairIncident(context.Background(), "device:alice", in.ID)
			got, err := s.Get(in.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.State != IncidentFailed || len(got.Attempts) != tc.want || len(delegate.tasks) != tc.want {
				t.Fatalf("incident = %+v with %d delegate calls, want failed after %d attempts", got, len(delegate.tasks), tc.want)
			}
		})
	}
}

// With unattended shaping off, capture still records the incident but no
// repair starts, and the refusal is logged once.
func TestRepairAutoOffBlocksNewRepair(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	delegate := &repairShaper{mode: "fix"}
	app, base := repairLimitFixture(t, delegate, "")
	if err := shape.SetAuto(app.opts.Home, false); err != nil {
		t.Fatal(err)
	}
	started := 0
	old := startRepair
	startRepair = func(*shapedApp, string, Incident) { started++ }
	defer func() { startRepair = old }()
	in, ok := app.recordIncident("device:alice", IncidentAPI, "api/list", 500, "unit list failed", "exit 1", base)
	if !ok || in.ID == "" {
		t.Fatal("incident was not recorded while off")
	}
	if in.State != IncidentFailed || started != 0 {
		t.Fatalf("off: state = %q, repairs started = %d", in.State, started)
	}
	// A repair asked for directly is refused as well.
	app.repairIncident(context.Background(), "device:alice", in.ID)
	if len(delegate.tasks) != 0 {
		t.Fatalf("off: delegate called %d times", len(delegate.tasks))
	}
	if activeRev(t, app, "device:alice") != base {
		t.Fatal("off: active revision moved")
	}
	sameEvents(t, autoEvents(t, app.opts.Home), "switch:off", "refusal:off")

	// Back on, the same capture triggers a repair again.
	if err := shape.SetAuto(app.opts.Home, true); err != nil {
		t.Fatal(err)
	}
	if _, ok := app.recordIncident("device:alice", IncidentAPI, "api/other", 500, "unit other failed", "exit 1", base); !ok || started != 1 {
		t.Fatalf("on: repairs started = %d", started)
	}
}

// offAfterFirst fails every attempt and turns unattended shaping off during
// the first one.
type offAfterFirst struct {
	repairShaper
	home string
}

func (f *offAfterFirst) Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error) {
	if len(f.tasks) == 0 {
		if err := shape.SetAuto(f.home, false); err != nil {
			return DelegateResult{}, err
		}
	}
	return f.repairShaper.Delegate(ctx, req)
}

// The off switch stops a running repair before its next attempt, and each
// decision on the way wrote one log line.
func TestRepairAutoOffStopsRunningRepair(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	delegate := &offAfterFirst{repairShaper: repairShaper{mode: "fail"}}
	app, base := repairLimitFixture(t, delegate, "")
	delegate.home = app.opts.Home
	app.opts.RepairMaxAttempts = repairIntPtr(3)
	s := repairLimitStore(t, app, "device:alice", base)
	in := observeBroken(t, s, base)
	app.repairIncident(context.Background(), "device:alice", in.ID)
	got, err := s.Get(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(delegate.tasks) != 1 || len(got.Attempts) != 1 || got.State != IncidentFailed {
		t.Fatalf("incident = %+v with %d delegate calls, want stopped after 1 of 3", got, len(delegate.tasks))
	}
	sameEvents(t, autoEvents(t, app.opts.Home), "switch:off", "attempt:failed", "verdict:pass", "refusal:off")
}

// A passing repair logs its trigger, attempt, verdict and activation, one
// line each, and still activates by itself.
func TestRepairDecisionLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	delegate := &repairShaper{mode: "fix"}
	app, base := repairLimitFixture(t, delegate, "")
	var queued []Incident
	old := startRepair
	startRepair = func(_ *shapedApp, _ string, in Incident) { queued = append(queued, in) }
	defer func() { startRepair = old }()
	in, ok := app.recordIncident("device:alice", IncidentAPI, "api/list", 500, "unit list failed", "exit 1", base)
	if !ok || len(queued) != 1 {
		t.Fatalf("capture: ok=%v queued=%d", ok, len(queued))
	}
	app.repairIncident(context.Background(), "device:alice", in.ID)
	if activeRev(t, app, "device:alice") == base {
		t.Fatal("passing repair was not activated")
	}
	sameEvents(t, autoEvents(t, app.opts.Home), "trigger:repair", "attempt:activated", "verdict:pass", "activation:activated")
}

// The wall-time bound stops an incident's repair before its next attempt.
func TestRepairWallTimeBound(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	delegate := &repairShaper{mode: "fail"}
	app, base := repairLimitFixture(t, delegate, "")
	app.opts.RepairMaxWallTime = time.Nanosecond
	s := repairLimitStore(t, app, "device:alice", base)
	in := observeBroken(t, s, base)
	app.repairIncident(context.Background(), "device:alice", in.ID)
	got, err := s.Get(in.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(delegate.tasks) != 0 || got.State != IncidentFailed {
		t.Fatalf("incident = %+v with %d delegate calls, want stopped by wall time", got, len(delegate.tasks))
	}
	sameEvents(t, autoEvents(t, app.opts.Home), "refusal:wall-time")
	if app.repairWallTime() != time.Nanosecond || (&shapedApp{}).repairWallTime() != DefaultRepairWallTime {
		t.Fatal("wall-time default or host option not applied")
	}
}

// An override needs a reason, is logged, and lets exactly one run exceed
// the host bound.
func TestRepairOverride(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	delegate := &repairShaper{mode: "fail"}
	app, base := repairLimitFixture(t, delegate, `{"repair": {"max_attempts": 3}}`)
	app.opts.RepairMaxAttempts = repairIntPtr(1)
	home := app.opts.Home
	if err := shape.ArmAutoOverride(home, "  "); err == nil {
		t.Fatal("override without a reason was accepted")
	}
	if _, ok := shape.AutoOverridePending(home); ok {
		t.Fatal("refused override was armed")
	}
	if err := shape.ArmAutoOverride(home, "known flaky verifier"); err != nil {
		t.Fatal(err)
	}
	s := repairLimitStore(t, app, "device:alice", base)
	in := observeBroken(t, s, base)
	app.repairIncident(context.Background(), "device:alice", in.ID)
	if len(delegate.tasks) != 3 {
		t.Fatalf("overridden run made %d attempts, want the app's 3", len(delegate.tasks))
	}
	log, err := shape.ReadAutoLog(home)
	if err != nil {
		t.Fatal(err)
	}
	armed, used := 0, 0
	for _, d := range log {
		if d.Event == "override" && d.Detail == "known flaky verifier" {
			switch d.Result {
			case "armed":
				armed++
			case "used":
				used++
			}
		}
	}
	if armed != 1 || used != 1 {
		t.Fatalf("override logged armed=%d used=%d, want 1 and 1: %+v", armed, used, log)
	}
	// The override is spent: the next run is back under the host bound.
	if _, ok := shape.AutoOverridePending(home); ok {
		t.Fatal("override survived its run")
	}
	delegate.tasks = nil
	next, start, err := s.Observe(Incident{App: "notes", Kind: IncidentAPI, Path: "api/other", Status: 500, Message: "unit other failed", BaseRev: base}, time.Now().Add(time.Minute))
	if err != nil || !start {
		t.Fatalf("observe: %v %v", start, err)
	}
	app.repairIncident(context.Background(), "device:alice", next.ID)
	if len(delegate.tasks) != 1 {
		t.Fatalf("run after the override made %d attempts, want the host's 1", len(delegate.tasks))
	}
}
