package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func runInboxCommand(t *testing.T, ctx context.Context, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewInboxCmd()
	var out, errOut bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errOut)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	err := cmd.Execute()
	return out.String(), errOut.String(), err
}

func notifyForInbox(t *testing.T, to, subject string) {
	t.Helper()
	if err := Publish(Notification{Principal: "scheduler", To: to, Body: subject}); err != nil {
		t.Fatal(err)
	}
}

func TestInboxReadsOnlyMineAndUsesTheBusCursor(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "meet 3 — your turn")
	notifyForInbox(t, "bob", "not yours")

	out, _, err := runInboxCommand(t, context.Background(), "--as", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "meet 3 — your turn") || strings.Contains(out, "not yours") {
		t.Fatalf("inbox did not select alice's 1:1 notifications:\n%s", out)
	}

	again, _, err := runInboxCommand(t, context.Background(), "--as", "alice")
	if err != nil {
		t.Fatal(err)
	}
	if again != "" {
		t.Fatalf("a second read re-delivered drained mail: %q", again)
	}
}

func TestDirectDrainDoesNotReappearInUnifiedInbox(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "shown by direct drain")
	notifyForInbox(t, "bob", "another subscriber")
	if _, err := SnapshotInbox("alice"); err != nil {
		t.Fatal(err)
	}
	if out, _, err := runInboxCommand(t, context.Background(), "--as", "alice"); err != nil || !strings.Contains(out, "shown by direct drain") {
		t.Fatalf("direct drain = %q, %v", out, err)
	}
	again, err := SnapshotInbox("alice")
	if err != nil || len(again.Items) != 0 {
		t.Fatalf("direct drain reappeared in unified inbox: %+v, %v", again.Items, err)
	}
	other, err := SnapshotInbox("bob")
	if err != nil || len(other.Items) != 1 || other.Items[0].Body != "another subscriber" {
		t.Fatalf("cross-subscriber mail was consumed: %+v, %v", other.Items, err)
	}
}

func TestUnreadNotificationsAcknowledgesOnlyTheRenderedSnapshot(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "first")
	events, through, err := UnreadNotifications("alice")
	if err != nil || len(events) != 1 {
		t.Fatalf("snapshot len=%d through=%d err=%v", len(events), through, err)
	}
	// Arrives after the snapshot but before its acknowledgement.
	notifyForInbox(t, "alice", "second")
	if err := MarkNotificationsRead("alice", through); err != nil {
		t.Fatal(err)
	}
	remaining, _, err := UnreadNotifications("alice")
	if err != nil || len(remaining) != 1 || remaining[0].Body != "second" {
		t.Fatalf("concurrent arrival was consumed: %+v err=%v", remaining, err)
	}
}

func TestSnapshotInboxReconcilesTimelineAndPendingByProvenanceOnly(t *testing.T) {
	isolate(t)
	if _, err := EnsureSubscription("alice"); err != nil {
		t.Fatal(err)
	}
	notifyForInbox(t, "alice", "same body")
	notifyForInbox(t, "alice", "same body")
	snapshot, err := SnapshotInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	// Each timeline event exists in both the direct and materialized view, but
	// only equal sequence+fields proves identity. Repeated text at a different
	// sequence remains two genuine messages.
	if len(snapshot.Items) != 2 || snapshot.Items[0].Seq == snapshot.Items[1].Seq {
		t.Fatalf("snapshot collapsed genuine repeats or duplicated a view: %+v", snapshot.Items)
	}
	if err := snapshot.Commit(); err != nil {
		t.Fatal(err)
	}
	again, err := SnapshotInbox("alice")
	if err != nil || len(again.Items) != 0 {
		t.Fatalf("committed snapshot reappeared: %+v err=%v", again.Items, err)
	}
}

func TestSnapshotInboxCommitPrefixKeepsUnshownBusAndRoleMail(t *testing.T) {
	for _, reader := range []string{"alice", "conductor.408"} {
		t.Run(reader, func(t *testing.T) {
			isolate(t)
			for i := 0; i < 13; i++ {
				notifyForInbox(t, reader, "batch "+strconv.Itoa(i))
			}
			notifyForInbox(t, "other", "other reader's mail")
			shown, err := SnapshotInbox(reader)
			if err != nil || len(shown.Items) != 13 {
				t.Fatalf("snapshot: %d items, %v", len(shown.Items), err)
			}
			peek, err := SnapshotInbox(reader)
			if err != nil || len(peek.Items) != 13 {
				t.Fatalf("peek consumed mail: %d items, %v", len(peek.Items), err)
			}
			// This arrival is newer than the batch captured above.
			notifyForInbox(t, reader, "new arrival")
			if err := shown.CommitPrefix(12); err != nil {
				t.Fatal(err)
			}
			again, err := SnapshotInbox(reader)
			if err != nil || len(again.Items) != 2 || again.Items[0].Body != "batch 12" || again.Items[1].Body != "new arrival" {
				t.Fatalf("unshown mail lost or shown mail repeated: %+v, %v", again.Items, err)
			}
			if err := again.Commit(); err != nil {
				t.Fatal(err)
			}
			empty, err := SnapshotInbox(reader)
			if err != nil || len(empty.Items) != 0 {
				t.Fatalf("second drain repeated mail: %+v, %v", empty.Items, err)
			}
			other, err := SnapshotInbox("other")
			if err != nil || len(other.Items) != 1 || other.Items[0].Body != "other reader's mail" {
				t.Fatalf("another subscriber was consumed: %+v, %v", other.Items, err)
			}
		})
	}
}

func TestSnapshotInboxOlderAckCannotRewindSubscriber(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "first")
	older, err := SnapshotInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	notifyForInbox(t, "alice", "second")
	newer, err := SnapshotInbox("alice")
	if err != nil {
		t.Fatal(err)
	}
	if err := newer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := older.Commit(); err != nil {
		t.Fatal(err)
	}
	again, err := SnapshotInbox("alice")
	if err != nil || len(again.Items) != 0 {
		t.Fatalf("older ack rewound cursor: %+v, %v", again.Items, err)
	}
}

func TestInboxPeekDoesNotAdvanceCursor(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "gate finished")

	peeked, _, err := runInboxCommand(t, context.Background(), "--as", "alice", "--peek")
	if err != nil || !strings.Contains(peeked, "gate finished") {
		t.Fatalf("peek = %q, %v", peeked, err)
	}
	out, _, err := runInboxCommand(t, context.Background(), "--as", "alice")
	if err != nil || !strings.Contains(out, "gate finished") {
		t.Fatalf("peek consumed the notification: %q, %v", out, err)
	}
}

func TestInboxLimitDoesNotConsumeWhatItOmits(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "one")
	notifyForInbox(t, "alice", "two")
	notifyForInbox(t, "alice", "three")

	first, _, err := runInboxCommand(t, context.Background(), "--as", "alice", "--limit", "2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(first, "one") || !strings.Contains(first, "two") || strings.Contains(first, "three") {
		t.Fatalf("limited read = %q", first)
	}
	second, _, err := runInboxCommand(t, context.Background(), "--as", "alice")
	if err != nil || !strings.Contains(second, "three") || strings.Contains(second, "one") {
		t.Fatalf("remainder = %q, %v", second, err)
	}
}

func TestInboxJSONUsesTheBusEventEnvelope(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "machine readable")
	out, _, err := runInboxCommand(t, context.Background(), "--as", "alice", "--json")
	if err != nil {
		t.Fatal(err)
	}
	var got WatchEvent
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("inbox --json: %v\n%s", err, out)
	}
	if got.To != "alice" || got.Body != "machine readable" || got.Principal != "scheduler" {
		t.Fatalf("inbox envelope = %+v", got)
	}
}

func TestInboxOpenByIDRetrievesWithoutConsuming(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "retrievable body")
	snapshot, err := SnapshotInbox("alice")
	if err != nil || len(snapshot.Items) != 1 {
		t.Fatalf("snapshot=%+v err=%v", snapshot.Items, err)
	}
	id := "bus:" + strconv.FormatInt(snapshot.Items[0].Seq, 10)
	out, _, err := runInboxCommand(t, context.Background(), "--as", "alice", "--id", id, "--peek")
	if err != nil || !strings.Contains(out, "retrievable body") {
		t.Fatalf("open = %q, %v", out, err)
	}
	again, err := SnapshotInbox("alice")
	if err != nil || len(again.Items) != 1 {
		t.Fatalf("open-by-id consumed inbox: %+v, %v", again.Items, err)
	}
}

func TestInboxWaitReturnsNewNotificationAndAdvancesCursor(t *testing.T) {
	isolate(t)
	published := make(chan error, 1)
	go func() {
		time.Sleep(25 * time.Millisecond)
		published <- Publish(Notification{Principal: "scheduler", To: "alice", Body: "timer fired"})
	}()
	out, _, err := runInboxCommand(t, context.Background(), "--as", "alice", "--wait", "2s")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "timer fired") {
		t.Fatalf("wait returned without the notification:\n%s", out)
	}
	again, _, err := runInboxCommand(t, context.Background(), "--as", "alice")
	if err != nil || again != "" {
		t.Fatalf("waited read did not drain: %q, %v", again, err)
	}
}

func TestInboxWaitReturnsBacklogImmediately(t *testing.T) {
	isolate(t)
	notifyForInbox(t, "alice", "already waiting")
	start := time.Now()
	out, _, err := runInboxCommand(t, context.Background(), "--as", "alice", "--wait", "2s")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("existing notification waited %s", elapsed)
	}
	if !strings.Contains(out, "already waiting") {
		t.Fatalf("backlog missing: %q", out)
	}
}

func TestInboxWaitTimeoutIsEmptySuccess(t *testing.T) {
	isolate(t)
	start := time.Now()
	out, errOut, err := runInboxCommand(t, context.Background(), "--as", "alice", "--wait", "40ms")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" || !strings.Contains(errOut, "nothing new") {
		t.Fatalf("timeout output = stdout %q, stderr %q", out, errOut)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("wait returned before its bound: %s", elapsed)
	}
}

func TestInboxWaitHonorsCancellation(t *testing.T) {
	isolate(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := runInboxCommand(t, ctx, "--as", "alice", "--wait", "2s")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled inbox returned %v", err)
	}
}

func TestInboxRejectsNegativeLimitAndWait(t *testing.T) {
	for _, args := range [][]string{{"--as", "alice", "--limit", "-1"}, {"--as", "alice", "--wait", "-1s"}} {
		_, _, err := runInboxCommand(t, context.Background(), args...)
		if err == nil {
			t.Fatalf("inbox %v succeeded", args)
		}
	}
}

// A zero acknowledgement on a reader with no cursor must still establish the
// cursor: that file is the reader's addressability proof, so notify to an
// unknown name fails closed until it exists and then delivers unread mail.
func TestMarkNotificationsReadZeroEstablishesReachableReader(t *testing.T) {
	isolate(t)
	if err := NotifyEvent("scheduler", "zeroreader", "before"); err == nil {
		t.Fatal("notify to a reader with no cursor must fail closed")
	}
	if err := MarkNotificationsRead("zeroreader", 0); err != nil {
		t.Fatal(err)
	}
	if cur, ok := inboxCursorSeq("zeroreader"); !ok || cur != 0 {
		t.Fatalf("cursor after zero ack = %d, exists=%v; want 0, true", cur, ok)
	}
	if err := NotifyEvent("scheduler", "zeroreader", "after"); err != nil {
		t.Fatalf("notify after establishing the cursor: %v", err)
	}
	events, _, err := UnreadNotifications("zeroreader")
	if err != nil || len(events) != 1 || events[0].Body != "after" {
		t.Fatalf("established reader unread = %+v, %v; want exactly the one delivered mail", events, err)
	}
	if err := NotifyEvent("scheduler", "ghostreader", "x"); err == nil {
		t.Fatal("an unrelated unknown reader must stay unreachable")
	}
}

func TestMarkNotificationsReadStaysMonotonic(t *testing.T) {
	isolate(t)
	if err := MarkNotificationsRead("mono", 5); err != nil {
		t.Fatal(err)
	}
	for _, through := range []int64{0, 3, -1} {
		if err := MarkNotificationsRead("mono", through); err != nil {
			t.Fatal(err)
		}
		if cur, _ := inboxCursorSeq("mono"); cur != 5 {
			t.Fatalf("ack through %d moved the cursor to %d; want it held at 5", through, cur)
		}
	}
	if err := MarkNotificationsRead("mono", 9); err != nil {
		t.Fatal(err)
	}
	if cur, _ := inboxCursorSeq("mono"); cur != 9 {
		t.Fatalf("forward ack left cursor at %d, want 9", cur)
	}
}

// A cursor that exists but cannot be read must surface the read error, not be
// treated as absent and replaced: replacing it would reset the reader's
// high-water mark. A directory at the cursor path is a deterministic read
// failure on every platform and needs no permission tricks.
func TestMarkNotificationsReadUnreadableCursorIsAnErrorNotAReset(t *testing.T) {
	isolate(t)
	path, err := cursorPath("blocked")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(path, "keep")
	if err := os.WriteFile(sentinel, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, through := range []int64{0, 7} {
		err := MarkNotificationsRead("blocked", through)
		if err == nil {
			t.Fatalf("ack through %d on an unreadable cursor succeeded; want the read error", through)
		}
		if strings.Contains(err.Error(), "writing the drain cursor") {
			t.Fatalf("ack through %d reached the write path instead of failing on read: %v", through, err)
		}
		if st, statErr := os.Stat(path); statErr != nil || !st.IsDir() {
			t.Fatalf("cursor path was replaced (stat=%v, err=%v); want the directory left alone", st, statErr)
		}
		if _, statErr := os.Stat(sentinel); statErr != nil {
			t.Fatalf("cursor directory contents were disturbed: %v", statErr)
		}
		if _, statErr := os.Stat(path + ".tmp"); !os.IsNotExist(statErr) {
			t.Fatalf("a temp cursor was left behind after a failed read: %v", statErr)
		}
	}
	if _, ok := inboxCursorSeq("blocked"); ok {
		t.Fatal("an unreadable cursor must not read as established")
	}
}
