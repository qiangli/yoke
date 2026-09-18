package foreman

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/room"
)

// isolateRoom points the room store at a scratch dir so these tests cannot see
// or touch the operator's live membership. The guard at the end of each test
// asserts the OUTCOME rather than trusting this call — an isolation helper that
// names the right store still has to be proven to have worked.
func isolateRoom(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("BASHY_ROOM_DIR", dir)
	return dir
}

func roomCardCount(t *testing.T, dir string) int {
	t.Helper()
	entries, err := os.ReadDir(filepath.Join(dir, "members"))
	if err != nil {
		return 0 // no members dir is zero cards, not an error
	}
	return len(entries)
}

// A FAILED eager attach must leave NOTHING behind. This is the leak path, and
// it is the one worth a test: a half-started owner session that published a
// card and then errored would be an unreachable name that every admission check
// reads as reachable — the exact state the whole design refuses.
func TestEagerAttachFailureLeavesNoSessionAndNoCard(t *testing.T) {
	roomDir := isolateRoom(t)

	s, err := Start(context.Background(), Options{
		ID:    "eager-fail",
		Goal:  "become reachable",
		Agent: "definitely-not-a-registered-tool",
		Root:  t.TempDir(),
		Eager: true,
	})
	if err == nil {
		if s != nil {
			s.Close()
		}
		t.Fatal("eager Start succeeded against an unresolvable agent; it must fail loudly")
	}
	if s != nil {
		t.Fatalf("eager Start returned a session alongside its error: %+v", s)
	}
	if !strings.Contains(err.Error(), "eager attach") {
		t.Errorf("error %q does not say which step failed; a caller cannot act on that", err)
	}
	// HONESTY ABOUT WHICH HALF OF THIS TEST IS LOAD-BEARING TODAY.
	//
	// The session assertion above is the one with teeth: mutating Start to return
	// `s, err` instead of `nil, err` fails it. This card assertion currently
	// passes TRIVIALLY, because attach publishes s.live only after chat.Start
	// succeeds, so a failing attach never reaches a card to begin with.
	//
	// It stays because it is the regression that matters if that order ever
	// changes — publish-then-verify would produce an unreachable name that every
	// admission check reads as reachable. Recording that it is a guard rather
	// than a proof, so nobody later cites it as evidence attach was audited.
	if n := roomCardCount(t, roomDir); n != 0 {
		t.Fatalf("a failed eager attach published %d room card(s); an unreachable name that\n"+
			"reads as reachable is precisely what this must never produce", n)
	}
}

// Lazy stays the default. An operator who starts a foreman and never speaks to
// it must not be paying for a model to sit at a prompt.
func TestStartIsLazyByDefault(t *testing.T) {
	roomDir := isolateRoom(t)

	s, err := Start(context.Background(), Options{
		ID:     "lazy",
		Goal:   "wait quietly",
		Agent:  "stub",
		Root:   t.TempDir(),
		Runner: &stubRunner{out: "ack"},
	})
	if err != nil {
		t.Fatalf("lazy start: %v", err)
	}
	defer s.Close()

	if got := s.State().Status; got != StatusIdle {
		t.Errorf("status = %q, want %q — a lazy start has not begun work", got, StatusIdle)
	}
	if s.getLive() != nil {
		t.Error("a lazy start brought an agent up; it must wait for the first message")
	}
	if n := roomCardCount(t, roomDir); n != 0 {
		t.Errorf("a lazy start published %d room card(s); it advertises nothing until it is live", n)
	}
}

// Close is the release, and it must hold whether or not anything was ever
// attached — a stop path that only works after a successful start is a leak
// waiting for the first failure.
func TestCloseIsSafeOnANeverAttachedSession(t *testing.T) {
	isolateRoom(t)
	s, err := Start(context.Background(), Options{
		ID: "never", Goal: "never attached", Root: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	s.Close()
	s.Close() // twice: a supervisor that stops an already-stopped session must not panic
	if s.State().Steering {
		t.Error("Steering stayed true after Close")
	}
}

// attach() runs under s.mu and closeLive() takes s.mu, so the cleanup on the
// error path cannot run inside the locked region. This test would hang rather
// than fail if that ordering is ever reversed, which is why it carries its own
// name: a deadlock in a start path is not obvious from a stack trace taken
// later.
func TestEagerAttachDoesNotDeadlockOnCleanup(t *testing.T) {
	isolateRoom(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		s, err := Start(context.Background(), Options{
			ID: "deadlock", Goal: "g", Agent: "definitely-not-a-registered-tool",
			Root: t.TempDir(), Eager: true,
		})
		if err == nil && s != nil {
			s.Close()
		}
	}()
	select {
	case <-done:
	case <-t.Context().Done():
		t.Fatal("eager attach cleanup deadlocked: attach holds s.mu and closeLive takes it")
	}
}

// The capability the whole design keys on. Kept as a compile-time reference so
// a rename of the constant reaches this package's tests rather than silently
// leaving them asserting on a string nothing publishes.
var _ = room.CapInboxDelivery
