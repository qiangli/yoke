// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

// S549.R1e: one root cause is one incident, and repairs are serialized per
// (app, principal). Two incidents opened back to back for the same principal
// share one repair line: the second waits while the first runs; when the
// first repair fixes both, the second closes as healed with no delegate
// call; otherwise the second repairs on the new base, so no attempt is
// refused by the evidence gate for a moved base.

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	shape "github.com/qiangli/yoke/pkg/shape/state"
	shapeverify "github.com/qiangli/yoke/pkg/shape/verify"
	"github.com/qiangli/yoke/pkg/src"
)

const serialBrokenList = "```bsh\nexit 1\n```\n"
const serialBrokenAdd = "```bsh\nexit 1\n```\n"
const serialFixedList = "```bsh\nprintf 'repaired\\n'\n```\n"
const serialFixedAdd = "```bsh\nprintf '{\"ok\":true}\\n'\n```\n"

// serialFixture breaks both api/list and api/add on the base revision and
// pins device:alice there, so two different signatures can open.
func serialFixture(t *testing.T, delegate Delegator) (*shapedApp, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source, _ := productionNotesSource(t, home)
	if err := os.WriteFile(filepath.Join(source, "api", "list.md"), []byte(serialBrokenList), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "api", "add.md"), []byte(serialBrokenAdd), 0600); err != nil {
		t.Fatal(err)
	}
	r, err := gogit.PlainOpen(source)
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := r.Worktree()
	base, err := wt.Commit("both broken", &gogit.CommitOptions{All: true, Author: &object.Signature{Name: "fixture", Email: "test@example.test", When: time.Now()}})
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Storer.SetReference(plumbing.NewHashReference("refs/heads/pristine", base)); err != nil {
		t.Fatal(err)
	}
	if _, err := src.Release(home, "notes", base.String(), "device:alice", src.ReleaseOptions{Shared: true}); err != nil {
		t.Fatal(err)
	}
	for _, principal := range []string{"device:alice", "device:bob"} {
		st, err := shape.OpenAt(home, "notes", principal)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Write(shape.State{ActiveRevision: base.String()}); err != nil {
			t.Fatal(err)
		}
	}
	opts := productionShapeOptions()
	opts.Home, opts.WorkRoot, opts.Runner = home, filepath.Join(t.TempDir(), "work"), []string{"/bin/sh"}
	opts.Delegate = delegate
	opts.Verifier = productionVerifier{home: home, runner: shapeverify.TrustedRunner{Bin: "/bin/sh", Effects: []string{"read"}}}
	opts.Activator.(*productionActivator).home = home
	return &shapedApp{name: "notes", repo: source, opts: opts}, base.String()
}

func serialStore(t *testing.T, app *shapedApp, principal string) *incidentStore {
	t.Helper()
	st, err := app.store(principal)
	if err != nil {
		t.Fatal(err)
	}
	s, err := openIncidents(st)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func serialObserve(t *testing.T, s *incidentStore, app, kind, path, msg, base string) Incident {
	t.Helper()
	in, start, err := s.Observe(Incident{App: app, Kind: IncidentKind(kind), Path: path, Status: 500, Message: msg, Evidence: "exit 1", BaseRev: base}, time.Now())
	if err != nil || !start {
		t.Fatalf("observe %s: %+v start=%v err=%v", path, in, start, err)
	}
	return in
}

// waitSerial polls until every named incident leaves the repairing state.
func waitSerial(t *testing.T, s *incidentStore, ids []string, timeout time.Duration) map[string]Incident {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		done := true
		got := map[string]Incident{}
		for _, id := range ids {
			in, err := s.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			got[id] = in
			if in.State == IncidentRepairing {
				done = false
			}
		}
		if done {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("incidents still repairing after %v: %+v", timeout, got)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// gatedFixAll blocks every delegate call until release is closed, so the test
// can prove the second incident waits instead of repairing in parallel.
type gatedFixAll struct {
	mu      sync.Mutex
	calls   int
	release chan struct{}
}

func (f *gatedFixAll) Calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *gatedFixAll) Delegate(ctx context.Context, req DelegateRequest) (DelegateResult, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	select {
	case <-f.release:
	case <-ctx.Done():
		return DelegateResult{}, ctx.Err()
	}
	if err := os.WriteFile(filepath.Join(req.Dir, "api", "list.md"), []byte(serialFixedList), 0600); err != nil {
		return DelegateResult{}, err
	}
	if err := os.WriteFile(filepath.Join(req.Dir, "api", "add.md"), []byte(serialFixedAdd), 0600); err != nil {
		return DelegateResult{}, err
	}
	return DelegateResult{Agent: "test:serial-gated"}, nil
}

// TestRepairSerialSecondWaitsAndHeals: the second incident waits while the
// first repair runs, and when that repair fixes both requests the second
// closes with no delegate call.
func TestRepairSerialSecondWaitsAndHeals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	oldHook := startRepair
	startRepair = startRepairAsync
	t.Cleanup(func() { startRepair = oldHook })

	gate := &gatedFixAll{release: make(chan struct{})}
	app, base := serialFixture(t, gate)
	const principal = "device:alice"
	s := serialStore(t, app, principal)
	in1 := serialObserve(t, s, "notes", "api", "api/list", "unit list failed", base)
	in2 := serialObserve(t, s, "notes", "api", "api/add", "unit add failed", base)

	startRepair(app, principal, in1)
	// The first repair must reach the delegate before the second starts.
	deadline := time.Now().Add(30 * time.Second)
	for gate.Calls() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("first repair never reached the delegate")
		}
		time.Sleep(50 * time.Millisecond)
	}
	startRepair(app, principal, in2)
	// The second incident waits: no second delegate call while blocked.
	time.Sleep(time.Second)
	if got := gate.Calls(); got != 1 {
		t.Fatalf("delegate calls while first repair runs = %d, want 1 (second waits)", got)
	}
	close(gate.release)
	final := waitSerial(t, s, []string{in1.ID, in2.ID}, 90*time.Second)
	if final[in1.ID].State != IncidentActivated {
		t.Fatalf("first incident = %+v, want activated", final[in1.ID])
	}
	if final[in2.ID].State != IncidentActivated {
		t.Fatalf("second incident = %+v, want healed to activated", final[in2.ID])
	}
	if len(final[in2.ID].Attempts) != 0 {
		t.Fatalf("healed incident has %d attempts, want none", len(final[in2.ID].Attempts))
	}
	if got := gate.Calls(); got != 1 {
		t.Fatalf("delegate calls total = %d, want 1 (healed with no new attempt)", got)
	}
}

// perPathShaper fixes only the incident named in the repair brief, so the
// first repair leaves the second request broken: the second must repair on
// the new base. A short sleep forces the two repairs to overlap when they
// run unserialized.
type perPathShaper struct {
	mu    sync.Mutex
	calls int
}

func (f *perPathShaper) Delegate(_ context.Context, req DelegateRequest) (DelegateResult, error) {
	time.Sleep(500 * time.Millisecond)
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	if strings.Contains(req.Task, "path=api/add") {
		if err := os.WriteFile(filepath.Join(req.Dir, "api", "add.md"), []byte(serialFixedAdd), 0600); err != nil {
			return DelegateResult{}, err
		}
	} else {
		if err := os.WriteFile(filepath.Join(req.Dir, "api", "list.md"), []byte(serialFixedList), 0600); err != nil {
			return DelegateResult{}, err
		}
	}
	return DelegateResult{Agent: "test:serial-per-path"}, nil
}

// TestRepairSerialRebaseOnNewBase: when the first repair does not fix the
// second request, the second repairs on the new base — no attempt is refused
// by the evidence gate for a moved base.
func TestRepairSerialRebaseOnNewBase(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses /bin/sh")
	}
	oldHook := startRepair
	startRepair = startRepairAsync
	t.Cleanup(func() { startRepair = oldHook })

	delegate := &perPathShaper{}
	app, base := serialFixture(t, delegate)
	const principal = "device:alice"
	s := serialStore(t, app, principal)
	in1 := serialObserve(t, s, "notes", "api", "api/list", "unit list failed", base)
	in2 := serialObserve(t, s, "notes", "api", "api/add", "unit add failed", base)

	startRepair(app, principal, in1)
	startRepair(app, principal, in2)
	final := waitSerial(t, s, []string{in1.ID, in2.ID}, 120*time.Second)
	for _, id := range []string{in1.ID, in2.ID} {
		in := final[id]
		if in.State != IncidentActivated {
			t.Fatalf("incident %s = %+v, want activated", id, in)
		}
		if len(in.Attempts) != 1 {
			t.Fatalf("incident %s has %d attempts, want exactly 1 (no wasted stale attempt): %+v", id, len(in.Attempts), in)
		}
		if r := in.Attempts[0].Reason; strings.Contains(r, "bound to different inputs") {
			t.Fatalf("incident %s refused for a moved base: %q", id, r)
		}
	}
}
