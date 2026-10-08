package cligw

// Sticky sessions: one warm CLI process kept across turns for sticky
// bind=worker/reset=none bindings. Only stdin-stream-json tools (claude, agy)
// can take further turns on stdin; every other warm mode stays one-shot.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestStickySessionRefusesColdTools(t *testing.T) {
	installFakeCatalog(t, "fakecold", WarmCold, "cold")
	if _, err := NewStickySession(context.Background(), "test-agent"); err == nil {
		t.Fatal("cold tool accepted a sticky session")
	} else if !strings.Contains(err.Error(), "sticky") {
		t.Fatalf("error %q does not name sticky sessions", err)
	}
}

func TestStickySessionTwoTurnsShareOneProcess(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "sticky-loop")
	sess, err := NewStickySession(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	if sess.Agent() != "test-agent" {
		t.Fatalf("agent = %q", sess.Agent())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	first, err := sess.Turn(ctx, CompletionPrompt{Prompt: "first"}, nil)
	if err != nil || first.Outcome != OutcomeOK {
		t.Fatalf("turn1 = %+v err=%v", first, err)
	}
	second, err := sess.Turn(ctx, CompletionPrompt{Prompt: "second"}, nil)
	if err != nil || second.Outcome != OutcomeOK {
		t.Fatalf("turn2 = %+v err=%v", second, err)
	}
	pid := func(text string) string {
		head, _, _ := strings.Cut(text, ":")
		return head
	}
	if !strings.HasSuffix(first.Text, ":first") || !strings.HasSuffix(second.Text, ":second") {
		t.Fatalf("texts = %q %q (want per-turn answers)", first.Text, second.Text)
	}
	if pid(first.Text) == "" || pid(first.Text) != pid(second.Text) {
		t.Fatalf("turns reached different processes: %q vs %q", first.Text, second.Text)
	}
}

func TestStickySessionClaudePartialsAreNotDuplicated(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "sticky-partials")
	sess, err := NewStickySession(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var deltas []string
	got, err := sess.Turn(ctx, CompletionPrompt{Prompt: "hi"}, func(ev Event) {
		if ev.Text != "" {
			deltas = append(deltas, ev.Text)
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Text != "half:hi" || len(deltas) != 1 || deltas[0] != "half:hi" {
		t.Fatalf("result=%q deltas=%q (want one delta, no snapshot duplication)", got.Text, deltas)
	}
	again, err := sess.Turn(ctx, CompletionPrompt{Prompt: "yo"}, nil)
	if err != nil || again.Text != "half:yo" {
		t.Fatalf("turn2 = %q err=%v", again.Text, err)
	}
}

func TestStickySessionNativeSystemOnce(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "sticky-loop")
	sess, err := NewStickySession(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := sess.Turn(ctx, CompletionPrompt{System: "Be concise.", Prompt: "hi"}, nil); err != nil {
		t.Fatal(err)
	}
	// The second turn carries no system: the CLI already holds it, so the
	// bare prompt must not be wrapped in another inline System block.
	got, err := sess.Turn(ctx, CompletionPrompt{Prompt: "hi again"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got.Text, "System:") {
		t.Fatalf("second turn re-sent system: %q", got.Text)
	}
}

func TestDialStickyHonorsSupportAndCap(t *testing.T) {
	catalog := installServerFleet(t)
	t.Setenv("BASHY_HOME", t.TempDir())
	server, err := NewServer(ServerOptions{
		Catalog: catalog,
		Pool:    PoolConfig{StartServers: 0, MinSpare: 0, MaxSpare: 0, MaxWorkers: 1, IdleTTL: time.Minute},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := server.DialSticky(ctx, "cold-four"); !errors.Is(err, ErrStickyUnsupported) {
		t.Fatalf("cold DialSticky = %v, want ErrStickyUnsupported", err)
	}
	if _, err := server.DialSticky(ctx, "no-such-agent"); err == nil {
		t.Fatal("unknown agent dialed a session")
	}
	first, err := server.DialSticky(ctx, "warm-four")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := server.DialSticky(ctx, "warm-four"); !errors.Is(err, ErrStickyCapped) {
		t.Fatalf("second DialSticky = %v, want ErrStickyCapped at ceiling 1", err)
	}
	// Closing the reservation re-opens the room.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := server.DialSticky(ctx, "warm-four")
	if err != nil {
		t.Fatalf("DialSticky after close = %v", err)
	}
	defer second.Close()
}

func TestStickySessionCloseIsIdempotent(t *testing.T) {
	installFakeCatalog(t, "claude", WarmStdinStreamJSON, "sticky-loop")
	sess, err := NewStickySession(context.Background(), "test-agent")
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatal(err)
	}
	if err := sess.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := sess.Turn(ctx, CompletionPrompt{Prompt: "late"}, nil); err == nil {
		t.Fatal("turn after close succeeded")
	}
}
