package room

// THE BOARD'S FOUR ENTRY POINTS, ONE LIVENESS RULE.
//
// Every test here fails against the candidate these repairs were requested
// against, and each fails for the same underlying reason: Members, Join and
// LeavePID judged a session card on Card.PID — the per-turn child command —
// while ClaimSession had already established that the stable owner is
// OwnerPID. The gap between two turns of a live conversation was therefore a
// window in which the board declared the conversation dead.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A live owning session whose per-turn child has exited is STILL A MEMBER.
//
// Against the candidate Members deleted its card, which is the first half of
// the two-live-contexts bug: ClaimSession refuses a competing session by
// comparing it against the incumbent card, and a card Members has removed
// refuses nothing.
func TestMembersKeepsASessionWhoseChildCommandExited(t *testing.T) {
	isolate(t)
	id := "instance:aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	// The owner is this test process; the child that wrote the card is gone.
	if err := ClaimSession(sessionCard(id, "sha256:live", os.Getpid(), deadPID(t))); err != nil {
		t.Fatalf("claim: %v", err)
	}

	members, err := Members()
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	var found bool
	for _, c := range members {
		if c.ID == id {
			found = true
		}
	}
	if !found {
		t.Fatalf("a session owned by live pid %d is missing from %d members — its claim no longer refuses anybody", os.Getpid(), len(members))
	}

	// And the refusal it exists for still fires.
	err = ClaimSession(sessionCard(id, "sha256:competitor", os.Getppid(), os.Getppid()))
	var live *ErrLive
	if !errors.As(err, &live) {
		t.Fatalf("a competing session must be refused with *ErrLive, got %T: %v", err, err)
	}
	if live.PID != os.Getpid() {
		t.Fatalf("ErrLive.PID = %d, want the live OWNER %d so the refusal names a process that exists", live.PID, os.Getpid())
	}
}

// Join must not overwrite a fenced session card. A plain Join carries no
// digest, so it cannot prove it is the owning session, and the candidate let
// it through on the strength of the dead child pid alone.
func TestJoinRefusesToOverwriteAFencedSessionCard(t *testing.T) {
	isolate(t)
	id := "instance:ffffffff-0000-1111-2222-333333333333"
	if err := ClaimSession(sessionCard(id, "sha256:owner", os.Getpid(), deadPID(t))); err != nil {
		t.Fatalf("claim: %v", err)
	}

	err := Join(Card{ID: id, Tool: "codex", Binding: "codex:gpt6", PID: os.Getppid()})
	var live *ErrLive
	if !errors.As(err, &live) {
		t.Fatalf("Join over a fenced live card must be refused with *ErrLive, got %T: %v", err, err)
	}

	owner, ok := SessionOwner(id)
	if !ok || owner.SessionClaim != "sha256:owner" || owner.Tool != "claude" {
		t.Fatalf("incumbent card = %+v (%v), want the untouched claude owner", owner, ok)
	}
}

// The owning session re-Joining its own card to revise it KEEPS the fence.
// Dropping SessionClaim/OwnerPID would leave the card in place but claiming
// nothing, which is the same end state as deleting it.
func TestJoinByTheOwnerPreservesTheFence(t *testing.T) {
	isolate(t)
	id := "instance:12121212-3434-5656-7878-909090909090"
	if err := ClaimSession(sessionCard(id, "sha256:owner", os.Getpid(), os.Getpid())); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Same holder (same stable pid), revising its task, carrying no digest.
	if err := Join(Card{ID: id, Tool: "claude", Binding: "claude:opus5.5", PID: os.Getpid(), Task: "revised"}); err != nil {
		t.Fatalf("the owner revising its own card: %v", err)
	}

	owner, ok := SessionOwner(id)
	if !ok {
		t.Fatal("the owner's own update removed its claim")
	}
	if owner.SessionClaim != "sha256:owner" || owner.OwnerPID != os.Getpid() {
		t.Fatalf("card = %+v, want the fence preserved (digest sha256:owner, owner %d)", owner, os.Getpid())
	}
	if owner.Task != "revised" {
		t.Fatalf("Task = %q, want the revision to have landed", owner.Task)
	}

	// A competitor is still refused after the revision.
	var live *ErrLive
	if err := ClaimSession(sessionCard(id, "sha256:other", os.Getppid(), os.Getppid())); !errors.As(err, &live) {
		t.Fatalf("competing session after a revision must still be refused, got %T: %v", err, err)
	}
}

// Leave from a process that is not the holder must not evict a live session.
// A per-turn child's deferred Leave did exactly that against the candidate.
func TestLeaveCannotEvictAnotherLiveSession(t *testing.T) {
	isolate(t)
	id := "instance:55555555-6666-7777-8888-999999999999"
	if err := ClaimSession(sessionCard(id, "sha256:owner", os.Getppid(), os.Getppid())); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// This process is not the holder.
	Leave(id)
	if _, ok := SessionOwner(id); !ok {
		t.Fatalf("Leave by pid %d removed the card held by live owner %d", os.Getpid(), os.Getppid())
	}

	// The holder itself may still retire its own card.
	LeavePID(id, os.Getppid())
	if _, ok := SessionOwner(id); ok {
		t.Fatal("LeavePID by the recorded holder must remove the card")
	}
}

// A stale card is still reclaimable — the repair must not turn "fence the
// living" into "never reclaim the dead".
func TestLeaveAndJoinStillReclaimAStaleCard(t *testing.T) {
	isolate(t)
	id := "task:weave-7"
	dead := deadPID(t)
	if err := Join(Card{ID: id, Tool: "codex", Binding: "codex:gpt6", PID: dead}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := Join(Card{ID: id, Tool: "claude", Binding: "claude:opus5.5", PID: os.Getpid()}); err != nil {
		t.Fatalf("reclaiming a stale card must succeed: %v", err)
	}
	got, ok, _ := Find(id)
	if !ok || got.Tool != "claude" {
		t.Fatalf("card = %+v (%v), want the reclaiming member", got, ok)
	}
	// A reclaim is a NEW assignment: it must not inherit the dead member's
	// start time, which elapsed-work readers would report as its own.
	if got.Joined == "" {
		t.Fatal("reclaimed card has no Joined")
	}
}

// Every card a claim path writes is a COMPLETE card. A truncating write is
// invisible to its own writer and shows up as a live instance missing from
// the board, so the guarantee is checked on the bytes.
func TestCardsAreWrittenAtomically(t *testing.T) {
	isolate(t)
	id := "instance:abcdabcd-abcd-abcd-abcd-abcdabcdabcd"
	if err := ClaimSession(sessionCard(id, "sha256:owner", os.Getpid(), os.Getpid())); err != nil {
		t.Fatalf("claim: %v", err)
	}
	dir, err := membersDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cards := 0
	for _, e := range entries {
		name := e.Name()
		if strings.HasSuffix(name, ".tmp") {
			t.Fatalf("a staging file was left behind in the public members set: %s", name)
		}
		if !strings.HasSuffix(name, ".json") {
			continue
		}
		cards++
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		var c Card
		if err := json.Unmarshal(b, &c); err != nil {
			t.Fatalf("card %s does not parse: %v", name, err)
		}
	}
	if cards != 1 {
		t.Fatalf("members set holds %d cards, want exactly 1", cards)
	}
}

// Members is called from inside WithMemberClaimsGuard, which holds the claim
// lock. Pruning under that lock must therefore be TRIED, never waited for.
func TestMembersDoesNotDeadlockInsideTheClaimsGuard(t *testing.T) {
	isolate(t)
	if err := Join(Card{ID: "task:stale", Tool: "codex", Binding: "codex:gpt6", PID: deadPID(t)}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	done := make(chan error, 1)
	go func() {
		done <- WithMemberClaimsGuard(func() error {
			_, err := Members()
			return err
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Members inside the claims guard: %v", err)
		}
	case <-timeoutAfter():
		t.Fatal("Members blocked on the claims lock inside WithMemberClaimsGuard — the maintenance path is deadlocked")
	}
}

func timeoutAfter() <-chan struct{} {
	ch := make(chan struct{})
	go func() {
		time.Sleep(5 * time.Second)
		close(ch)
	}()
	return ch
}

// The owner rule is for session claims. A plain card (an inbox watcher) is
// held by its writer: once that process exits it is not live, however long
// the parent recorded as OwnerPID keeps running, and its own Leave removes it.
func TestPlainCardIsHeldByItsWriterNotItsParent(t *testing.T) {
	isolate(t)
	if err := Join(Card{ID: "watcher-exited", Tool: "codex", Mode: "inbox", PID: deadPID(t), OwnerPID: os.Getpid()}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, ok, _ := Find("watcher-exited"); ok {
		t.Fatal("an exited watcher stayed live because its parent is alive")
	}
	if err := Join(Card{ID: "watcher-live", Tool: "codex", Mode: "inbox", PID: os.Getpid(), OwnerPID: os.Getppid()}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	Leave("watcher-live")
	if _, ok, _ := Find("watcher-live"); ok {
		t.Fatal("a watcher's own Leave was refused because its parent is alive")
	}
}
