package src_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/shape/state"
	"github.com/qiangli/yoke/pkg/src"
)

func TestActivateExpectedBaseRefusesBeforeGate(t *testing.T) {
	home := t.TempDir()
	_, base, _, a, b, _ := s525a7Fixture(t, home)
	if _, err := src.Activate(home, s525a7App, a, s525a7Alice, src.ActivateOptions{Gate: s525a7AllowGate(), ExpectedBase: base}); err != nil {
		t.Fatal(err)
	}
	called := false
	gate := state.GateFunc(func(string, string, string) (state.EvidenceRef, error) {
		called = true
		return state.EvidenceRef{Verdict: "pass"}, nil
	})
	if _, err := src.Activate(home, s525a7App, b, s525a7Alice, src.ActivateOptions{Gate: gate, ExpectedBase: base}); !errors.Is(err, src.ErrStaleBase) {
		t.Fatalf("got %v, want stale base", err)
	}
	if called {
		t.Fatal("stale candidate reached gate")
	}
	if got := s525a7State(t, home, s525a7Alice); got.ActiveRevision != a {
		t.Fatalf("stale candidate activated: %#v", got)
	}
}

func TestConcurrentActivationsUseOneExpectedBase(t *testing.T) {
	home := t.TempDir()
	_, base, _, a, b, _ := s525a7Fixture(t, home)
	entered := make(chan struct{}, 2)
	release := make(chan struct{})
	var calls atomic.Int32
	gate := state.GateFunc(func(string, string, string) (state.EvidenceRef, error) {
		calls.Add(1)
		entered <- struct{}{}
		<-release
		return state.EvidenceRef{Verdict: "pass"}, nil
	})
	results := make(chan error, 2)
	for _, rev := range []string{a, b} {
		go func(rev string) {
			_, err := src.Activate(home, s525a7App, rev, s525a7Alice, src.ActivateOptions{Gate: gate, ExpectedBase: base})
			results <- err
		}(rev)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("gate never entered")
	}
	select {
	case <-entered:
		close(release)
		t.Fatal("second activation entered gate before first finished")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	good, stale := 0, 0
	for range 2 {
		select {
		case err := <-results:
			if err == nil {
				good++
			} else if errors.Is(err, src.ErrStaleBase) {
				stale++
			} else {
				t.Fatal(err)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("activation deadlocked")
		}
	}
	if good != 1 || stale != 1 || calls.Load() != 1 {
		t.Fatalf("good=%d stale=%d gate calls=%d", good, stale, calls.Load())
	}
}

func TestRestoreSerializesWithActivation(t *testing.T) {
	home := t.TempDir()
	_, base, _, a, _, _ := s525a7Fixture(t, home)
	entered := make(chan struct{})
	release := make(chan struct{})
	activation := make(chan error, 1)
	gate := state.GateFunc(func(string, string, string) (state.EvidenceRef, error) {
		close(entered)
		<-release
		return state.EvidenceRef{Verdict: "pass"}, nil
	})
	go func() {
		_, err := src.Activate(home, s525a7App, a, s525a7Alice, src.ActivateOptions{Gate: gate, ExpectedBase: base})
		activation <- err
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("activation gate never entered")
	}
	restored := make(chan src.ActivateResult, 1)
	failed := make(chan error, 1)
	restoreGate := make(chan struct{}, 1)
	go func() {
		result, err := src.Restore(home, s525a7App, base, s525a7Alice, src.RestoreOptions{Gate: state.GateFunc(func(string, string, string) (state.EvidenceRef, error) {
			restoreGate <- struct{}{}
			return state.EvidenceRef{Verdict: "pass"}, nil
		})})
		if err != nil {
			failed <- err
		} else {
			restored <- result
		}
	}()
	select {
	case <-restoreGate:
		close(release)
		t.Fatal("restore raced active transaction")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-activation; err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-failed:
		t.Fatal(err)
	case result := <-restored:
		if result.Previous != a || result.Revision != base {
			t.Fatalf("restore used stale snapshot: %#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("restore deadlocked")
	}
	got := s525a7State(t, home, s525a7Alice)
	if got.ActiveRevision != base || got.LastKnownGood != a {
		t.Fatalf("lost serialized state: %#v", got)
	}
}
