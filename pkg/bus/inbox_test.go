package bus

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
