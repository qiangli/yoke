package bus

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

func runBusWithContext(t *testing.T, ctx context.Context, args ...string) (string, string, error) {
	t.Helper()
	cmd := NewBusCmd()
	var out, errb bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errb)
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	err := cmd.Execute()
	return out.String(), errb.String(), err
}

func TestWatchWaitReturnsOnRelevantNotification(t *testing.T) {
	isolate(t)
	published := make(chan error, 1)
	go func() {
		time.Sleep(25 * time.Millisecond)
		published <- room.Notify(room.Event{
			Type:      room.EventNotify,
			Topic:     "build",
			Principal: "alice",
			Body:      "shared gate changed",
		})
	}()

	out, _, err := runBusWithContext(t, context.Background(), "watch", "--topic", "build", "--drain", "--as", "profile-b", "--wait", "2s")
	if err != nil {
		t.Fatal(err)
	}
	if err := <-published; err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "shared gate changed") {
		t.Fatalf("wait returned without the new notification:\n%s", out)
	}
	out, _, err = runBusWithContext(t, context.Background(), "watch", "--topic", "build", "--drain", "--as", "profile-b")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "shared gate changed") {
		t.Fatalf("waited drain did not advance the cursor:\n%s", out)
	}
}

func TestWatchWaitReturnsBacklogImmediately(t *testing.T) {
	isolate(t)
	publish(t, "--topic", "build", "--as", "alice", "already waiting")
	start := time.Now()

	out, _, err := runBusWithContext(t, context.Background(), "watch", "--topic", "build", "--drain", "--as", "profile-b", "--wait", "2s")
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("existing notification waited %s instead of returning immediately", elapsed)
	}
	if !strings.Contains(out, "already waiting") {
		t.Fatalf("existing notification was not rendered:\n%s", out)
	}
}

func TestWatchWaitTimeoutIsAnEmptySuccessfulRead(t *testing.T) {
	isolate(t)
	start := time.Now()
	out, errOut, err := runBusWithContext(t, context.Background(), "watch", "--topic", "build", "--drain", "--as", "profile-b", "--wait", "40ms")
	if err != nil {
		t.Fatal(err)
	}
	if out != "" {
		t.Fatalf("timeout wrote bus output %q", out)
	}
	if !strings.Contains(errOut, "nothing new") {
		t.Fatalf("timeout did not report the empty read: %q", errOut)
	}
	if elapsed := time.Since(start); elapsed < 30*time.Millisecond {
		t.Fatalf("wait returned before its bound: %s", elapsed)
	}
}

func TestWatchWaitHonorsCancellation(t *testing.T) {
	isolate(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err := runBusWithContext(t, ctx, "watch", "--topic", "build", "--drain", "--as", "profile-b", "--wait", "2s")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled wait returned %v", err)
	}
}

func TestWatchWaitRejectsAll(t *testing.T) {
	isolate(t)
	_, _, err := runBusWithContext(t, context.Background(), "watch", "--all", "--drain", "--as", "profile-b", "--wait", "1s")
	if err == nil || !strings.Contains(err.Error(), "cannot be combined with --all") {
		t.Fatalf("--all --wait returned %v", err)
	}
}

// See TestPublishPrincipalIsRemoved: retired flags are removed, not aliased.
func TestWatchIntervalFlagAndPollIsRemoved(t *testing.T) {
	cmd := newWatchCmd()
	if cmd.Flags().Lookup("interval") == nil {
		t.Fatal("--interval flag is missing")
	}
	if poll := cmd.Flags().Lookup("poll"); poll != nil {
		t.Fatalf("--poll must not be registered at all, got %#v", poll)
	}

	isolate(t)
	publish(t, "--topic", "build", "--as", "alice", "already waiting")
	if _, _, err := runBusWithContext(t, context.Background(), "watch", "--topic", "build", "--drain", "--as", "profile-b", "--poll", "10ms"); err == nil {
		t.Fatal("--poll must be rejected; the canonical spelling is --interval")
	}
}

func TestWatchWaitSkipsFullParseWhenTimelineIsUnchanged(t *testing.T) {
	isolate(t)
	oldTimeline := watchTimeline
	oldStat := watchTimelineStat
	defer func() {
		watchTimeline = oldTimeline
		watchTimelineStat = oldStat
	}()

	parses := 0
	watchTimeline = func(int) ([]room.Event, error) {
		parses++
		return nil, nil
	}
	unchanged := timelineStat{size: 100, mtime: time.Unix(10, 0)}
	watchTimelineStat = func() (timelineStat, error) {
		return unchanged, nil
	}

	err := waitForDrain(context.Background(), eventFilter{topic: "build"}, 0, 260*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	if parses != 1 {
		t.Fatalf("unchanged timeline was parsed %d times, want 1", parses)
	}
}
