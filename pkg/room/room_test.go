package room

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/coreutils/pkg/lockfile"
)

func isolate(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if strings.HasPrefix(key, "BASHY_") {
			t.Setenv(key, "")
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
}

// TestJoinLeaveRoundTrip — join publishes a member and a timeline event; leave
// removes it and records a leave.
func TestJoinLeaveRoundTrip(t *testing.T) {
	isolate(t)
	c := Card{
		ID: "codex-1", Binding: "codex:gpt-5.5", Nick: "Bruno", Tool: "codex",
		Mode: "interactive", PID: os.Getpid(), // alive: this test process
	}
	if err := Join(c); err != nil {
		t.Fatalf("join: %v", err)
	}
	members, err := Members()
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) != 1 || members[0].ID != "codex-1" {
		t.Fatalf("members = %+v, want the one card", members)
	}
	if got, ok, _ := Find("Bruno"); !ok || got.ID != "codex-1" {
		t.Fatalf("find by nick = %+v (%v)", got, ok)
	}

	Leave("codex-1")
	members, _ = Members()
	if len(members) != 0 {
		t.Fatalf("after leave, want 0 members, got %d", len(members))
	}

	// The timeline recorded both a join and a leave.
	events, _ := Timeline(0)
	if len(events) != 2 || events[0].Type != EventJoin || events[1].Type != EventLeave {
		t.Fatalf("timeline = %+v, want join then leave", events)
	}
	if events[0].Seq != 1 || events[1].Seq != 2 {
		t.Fatalf("seq should be 1,2 got %d,%d", events[0].Seq, events[1].Seq)
	}
}

func TestMemberIDIsPortableOpaqueData(t *testing.T) {
	isolate(t)
	id := `shell:codex:8800/../../outside`
	if err := Join(Card{ID: id, Binding: "shell:codex", PID: os.Getpid()}); err != nil {
		t.Fatalf("join opaque id: %v", err)
	}
	got, ok, err := Find(id)
	if err != nil || !ok || got.ID != id {
		t.Fatalf("find opaque id: card=%+v ok=%v err=%v", got, ok, err)
	}
	Leave(id)
	if _, ok, _ := Find(id); ok {
		t.Fatal("opaque member survived Leave")
	}
}

// TestMembersPrunesDead — a card whose pid is gone is pruned on read, so the room
// never asserts a dead member is live (absence-of-evidence).
func TestMembersPrunesDead(t *testing.T) {
	isolate(t)
	if err := Join(Card{ID: "codex-dead", Binding: "codex:gpt-5.5", Tool: "codex", PID: 2147483000}); err != nil {
		t.Fatalf("join: %v", err)
	}
	members, err := Members()
	if err != nil {
		t.Fatalf("members: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("a dead member should be pruned on read, got %d", len(members))
	}
}

// TestTimelineTail — Emit appends; Timeline(n) returns the last n, oldest-first.
func TestTimelineTail(t *testing.T) {
	isolate(t)
	for _, b := range []string{"a", "b", "c"} {
		if err := Emit(Event{Type: EventNote, Body: b}); err != nil {
			t.Fatal(err)
		}
	}
	got, _ := Timeline(2)
	if len(got) != 2 || got[0].Body != "b" || got[1].Body != "c" {
		t.Fatalf("tail(2) = %+v, want [b c]", got)
	}
}

// --- the singleton claim -----------------------------------------------------

// TestJoinRefusesLiveDuplicate is the regression test for the whole
// agent-identity model: an id held by a LIVE member cannot be taken by a second
// one. Before Join was a claim this silently overwrote the first card, which is
// how two processes came to share one identity while looking like two healthy
// members.
func TestJoinRefusesLiveDuplicate(t *testing.T) {
	isolate(t)
	first := Card{ID: "elif", Binding: "ycode:glm-5.2", Nick: "elif", Tool: "ycode", PID: os.Getpid()}
	if err := Join(first); err != nil {
		t.Fatalf("first join: %v", err)
	}

	// A DIFFERENT live pid — stand in for a second process. os.Getppid() is alive
	// by construction (it is this test's parent).
	second := first
	second.PID = os.Getppid()
	err := Join(second)
	if err == nil {
		t.Fatal("second join of a live id must be refused, got nil")
	}
	var live *ErrLive
	if !errors.As(err, &live) {
		t.Fatalf("want *ErrLive, got %T: %v", err, err)
	}
	if live.ID != "elif" || live.PID != os.Getpid() {
		t.Fatalf("ErrLive = %+v, want the FIRST holder's id and pid", live)
	}

	// The incumbent's card is untouched — a refused claim must not have written.
	got, ok, _ := Find("elif")
	if !ok || got.PID != os.Getpid() {
		t.Fatalf("incumbent card = %+v (%v), want the original pid %d", got, ok, os.Getpid())
	}
}

func TestJoinSerializesClaimReadCheckWrite(t *testing.T) {
	isolate(t)
	lock, err := lockfile.Acquire(memberClaimsLockPath(), lockfile.Holder{Intent: "test hold"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- Join(Card{ID: "serialized", Binding: "codex:test", PID: os.Getpid()})
	}()
	select {
	case err := <-done:
		t.Fatalf("Join crossed a held cross-process claim lock: %v", err)
	case <-time.After(40 * time.Millisecond):
	}
	if err := lock.Release(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Join did not resume after claim lock release")
	}
}

func TestJoinKeepsSynchronizationMetadataOutsideMembershipSet(t *testing.T) {
	isolate(t)
	if err := Join(Card{ID: "only-card", Binding: "codex:test", PID: os.Getpid()}); err != nil {
		t.Fatal(err)
	}
	dir, err := membersDir()
	if err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].IsDir() || filepath.Ext(entries[0].Name()) != ".json" {
		t.Fatalf("membership set contains non-card synchronization metadata: %#v", entries)
	}
	if _, err := os.Stat(memberClaimsLockPath()); err != nil {
		t.Fatalf("member claim lock was not created beside membership set: %v", err)
	}
}

// TestJoinSameProcessIsAnUpdate — a member revises its own card as it works
// (task, caps, log path), so re-joining an id this process already holds must
// succeed rather than being mistaken for a rival.
func TestJoinSameProcessIsAnUpdate(t *testing.T) {
	isolate(t)
	c := Card{ID: "elif", Binding: "ycode:glm-5.2", Tool: "ycode", PID: os.Getpid()}
	if err := Join(c); err != nil {
		t.Fatalf("join: %v", err)
	}
	c.LogPath = "/tmp/elif.log"
	c.Task = "#412 fix the parser"
	if err := Join(c); err != nil {
		t.Fatalf("re-join by the same pid must be an update, got: %v", err)
	}
	got, ok, _ := Find("elif")
	if !ok || got.LogPath != "/tmp/elif.log" || got.Task != "#412 fix the parser" {
		t.Fatalf("card = %+v, want the updated fields", got)
	}
	if members, _ := Members(); len(members) != 1 {
		t.Fatalf("an update must not add a member, got %d", len(members))
	}
}

// TestJoinReclaimsStaleCard — a card left behind by a crash is not a conflict.
// Its pid is dead, so the id is free, exactly as Members would prune it on read.
func TestJoinReclaimsStaleCard(t *testing.T) {
	isolate(t)
	stale := Card{ID: "elif", Binding: "ycode:glm-5.2", Tool: "ycode", PID: 2147483000}
	if err := Join(stale); err != nil {
		t.Fatalf("seed stale card: %v", err)
	}
	fresh := stale
	fresh.PID = os.Getpid()
	if err := Join(fresh); err != nil {
		t.Fatalf("claiming an id held only by a DEAD pid must succeed, got: %v", err)
	}
	if got, ok, _ := Find("elif"); !ok || got.PID != os.Getpid() {
		t.Fatalf("card = %+v (%v), want the live claimant", got, ok)
	}
}

// TestLeaveDoesNotEvictAnotherHolder is the other half of the claim, and it is
// why Leave checks the pid at all: every caller pairs Join with a deferred
// Leave, and that defer runs even when the Join was REFUSED. Without the check,
// the process that lost the claim would delete the winner's card on its way out.
func TestLeaveDoesNotEvictAnotherHolder(t *testing.T) {
	isolate(t)
	// The incumbent is some other live process, not us.
	if err := Join(Card{ID: "elif", Binding: "ycode:glm-5.2", Tool: "ycode", PID: os.Getppid()}); err != nil {
		t.Fatalf("join: %v", err)
	}
	Leave("elif") // us — we never held it
	if got, ok, _ := Find("elif"); !ok || got.PID != os.Getppid() {
		t.Fatalf("incumbent must survive a loser's Leave; card = %+v (%v)", got, ok)
	}
}

func TestLeavePIDRetiresParentOwnedWorkWithoutEvictingAnotherOwner(t *testing.T) {
	isolate(t)
	parent := os.Getppid()
	if err := Join(Card{ID: "external-review", Binding: "codex:gpt", PID: parent}); err != nil {
		t.Fatalf("join parent-owned work: %v", err)
	}

	Leave("external-review")
	if _, ok, _ := Find("external-review"); !ok {
		t.Fatal("ordinary child Leave evicted its live parent-owned card")
	}

	LeavePID("external-review", parent)
	if _, ok, _ := Find("external-review"); ok {
		t.Fatal("LeavePID did not retire the card owned by the supplied parent")
	}
}

// TestTaskCardsCoexistWithAgentCard — the singleton is on the IDENTITY, not on
// the board. Many tasks of one agent may be live at once; they are keyed by work
// and must not be refused.
func TestTaskCardsCoexistWithAgentCard(t *testing.T) {
	isolate(t)
	for _, c := range []Card{
		{ID: "elif", Binding: "ycode:glm-5.2", Nick: "elif", Mode: "interactive", PID: os.Getpid()},
		{ID: "weave-412-9931", Binding: "ycode:glm-5.2", Mode: "weave", PID: os.Getpid()},
		{ID: "weave-413-9932", Binding: "ycode:glm-5.2", Mode: "weave", PID: os.Getpid()},
	} {
		if err := Join(c); err != nil {
			t.Fatalf("join %s: %v", c.ID, err)
		}
	}
	members, _ := Members()
	if len(members) != 3 {
		t.Fatalf("want 3 live members (1 agent + 2 tasks), got %d: %+v", len(members), members)
	}
}

func TestJoinHeartbeatPreservesStartAndRefreshesUpdate(t *testing.T) {
	isolate(t)
	first := Card{ID: "external-review", Binding: "codex:gpt", PID: os.Getpid(), Joined: "2026-01-01T00:00:00Z"}
	if err := Join(first); err != nil {
		t.Fatalf("first join: %v", err)
	}
	if err := Join(Card{ID: first.ID, Binding: first.Binding, PID: first.PID}); err != nil {
		t.Fatalf("heartbeat join: %v", err)
	}
	got, ok, err := Find(first.ID)
	if err != nil || !ok {
		t.Fatalf("find heartbeat card: ok=%v err=%v", ok, err)
	}
	if got.Joined != first.Joined {
		t.Fatalf("heartbeat rewrote assignment start: got %q want %q", got.Joined, first.Joined)
	}
	if got.Updated == "" {
		t.Fatal("heartbeat did not record Updated")
	}
}
