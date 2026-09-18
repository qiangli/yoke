package chat

import (
	"context"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

func isolatedRoom(t *testing.T) {
	t.Helper()
	t.Setenv("BASHY_ROOM_DIR", t.TempDir())
}

type blockingRunner struct {
	started chan struct{}
	release chan struct{}
	err     error
}

func (b *blockingRunner) Run(context.Context, string, []string, string) (string, int, error) {
	close(b.started)
	<-b.release
	if b.err != nil {
		return "", 1, b.err
	}
	return "done\n", 0, nil
}

type invokeOutcome struct {
	res Result
	err error
}

func waitFor[T any](t *testing.T, ch <-chan T, what string) T {
	t.Helper()
	select {
	case got := <-ch:
		return got
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		var zero T
		return zero
	}
}

func oneShotMembers(t *testing.T) []room.Card {
	t.Helper()
	members, err := room.Members()
	if err != nil {
		t.Fatalf("room.Members: %v", err)
	}
	var out []room.Card
	for _, c := range members {
		if c.Mode == "oneshot" {
			out = append(out, c)
		}
	}
	return out
}

func TestInvokePublishesTaskCardWhileRunningAndRemovesOnCompletion(t *testing.T) {
	isolatedRoom(t)
	r := &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
	done := make(chan invokeOutcome, 1)
	cwd := t.TempDir()
	go func() {
		res, err := Invoke(context.Background(), Options{
			Agent: "codex", Role: "reviewer", Task: "review login visibility",
			Instruction: "fix the login\nbug in detail", Cwd: cwd, ReadOnly: true,
		}, r)
		done <- invokeOutcome{res, err}
	}()

	waitFor(t, r.started, "runner start")
	live := oneShotMembers(t)
	if len(live) != 1 {
		t.Fatalf("live one-shot cards = %d, want 1", len(live))
	}
	c := live[0]
	if c.Mode != "oneshot" || c.Role != "reviewer" || c.PID != os.Getpid() {
		t.Errorf("card attribution = %+v", c)
	}
	if c.Tool == "" || c.Cwd != cwd || c.Task != "review login visibility" {
		t.Errorf("card details = %+v", c)
	}
	if !strings.HasPrefix(c.ID, "oneshot-") {
		t.Errorf("card id %q is not work-keyed", c.ID)
	}

	close(r.release)
	if outcome := waitFor(t, done, "Invoke completion"); outcome.err != nil {
		t.Fatalf("Invoke: %v", outcome.err)
	}
	if got := oneShotMembers(t); len(got) != 0 {
		t.Fatalf("one-shot cards after completion = %d, want 0", len(got))
	}
}

func TestInvokeRemovesTaskCardOnError(t *testing.T) {
	isolatedRoom(t)
	r := &blockingRunner{started: make(chan struct{}), release: make(chan struct{}), err: context.Canceled}
	done := make(chan invokeOutcome, 1)
	cwd := t.TempDir()
	go func() {
		res, err := Invoke(context.Background(), Options{
			Agent: "codex", Instruction: "run the failing task", Cwd: cwd, ReadOnly: true,
		}, r)
		done <- invokeOutcome{res, err}
	}()
	waitFor(t, r.started, "runner start")
	if len(oneShotMembers(t)) != 1 {
		t.Fatal("expected one live one-shot card while running")
	}
	close(r.release)
	if outcome := waitFor(t, done, "Invoke error completion"); outcome.err == nil {
		t.Fatal("Invoke error = nil, want runner error")
	}
	if got := oneShotMembers(t); len(got) != 0 {
		t.Fatalf("one-shot cards after error = %d, want 0", len(got))
	}
}

func TestConcurrentOneShotsDoNotCollide(t *testing.T) {
	isolatedRoom(t)
	const n = 3
	runners := make([]*blockingRunner, n)
	done := make(chan invokeOutcome, n)
	cwds := make([]string, n)
	for i := range cwds {
		cwds[i] = t.TempDir()
	}
	for i := range runners {
		runners[i] = &blockingRunner{started: make(chan struct{}), release: make(chan struct{})}
		r, cwd := runners[i], cwds[i]
		go func() {
			res, err := Invoke(context.Background(), Options{
				Agent: "codex", Instruction: "concurrent work", Cwd: cwd, ReadOnly: true,
			}, r)
			done <- invokeOutcome{res, err}
		}()
	}
	for _, r := range runners {
		waitFor(t, r.started, "concurrent runner start")
	}
	live := oneShotMembers(t)
	if len(live) != n {
		t.Fatalf("concurrent one-shot cards = %d, want %d", len(live), n)
	}
	ids := map[string]bool{}
	for _, c := range live {
		if ids[c.ID] {
			t.Fatalf("duplicate card id %q", c.ID)
		}
		ids[c.ID] = true
	}
	for _, r := range runners {
		close(r.release)
	}
	for range runners {
		if outcome := waitFor(t, done, "concurrent Invoke completion"); outcome.err != nil {
			t.Fatalf("Invoke: %v", outcome.err)
		}
	}
	if got := oneShotMembers(t); len(got) != 0 {
		t.Fatalf("one-shot cards after all complete = %d, want 0", len(got))
	}
}

func TestDryRunPublishesNoTaskCard(t *testing.T) {
	isolatedRoom(t)
	if _, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: "would run", Cwd: t.TempDir(), DryRun: true,
	}, &fakeRunner{}); err != nil {
		t.Fatal(err)
	}
	members, err := room.Members()
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("dry-run published %d cards, want 0", len(members))
	}
}

func TestTaskLabelIsSafeAndConcise(t *testing.T) {
	long := strings.Repeat("a", 200)
	if got := taskLabel(Options{Instruction: "  first\tline  \x00 with\r\nsecond"}); got != "first line with" {
		t.Errorf("taskLabel sanitize = %q", got)
	}
	if got := taskLabel(Options{Instruction: long}); len([]rune(got)) != 81 || !strings.HasSuffix(got, "…") {
		t.Errorf("taskLabel truncation = %q", got)
	}
	if got := taskLabel(Options{Task: " safe explicit label ", Instruction: "secret prompt"}); got != "safe explicit label" {
		t.Errorf("explicit task label = %q", got)
	}
	if got := taskLabel(Options{}); got != "one-shot agent task" {
		t.Errorf("empty task label = %q", got)
	}
}

type countingRunner struct{ calls atomic.Int32 }

func (r *countingRunner) Run(context.Context, string, []string, string) (string, int, error) {
	r.calls.Add(1)
	return "", 0, nil
}

func TestInvokeFailsClosedWhenAssignmentCannotBePublished(t *testing.T) {
	badRoom := t.TempDir() + "/not-a-directory"
	if err := os.WriteFile(badRoom, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_ROOM_DIR", badRoom)
	r := &countingRunner{}
	res, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: "must remain visible", Cwd: t.TempDir(), ReadOnly: true,
	}, r)
	if err == nil || !strings.Contains(err.Error(), "publish live assignment") {
		t.Fatalf("Invoke error = %v", err)
	}
	if res.ExitCode != 2 || r.calls.Load() != 0 {
		t.Fatalf("ExitCode=%d runner calls=%d; invisible work must not start", res.ExitCode, r.calls.Load())
	}
}

func TestInvokeRollsBackCardWhenTimelinePublishFails(t *testing.T) {
	roomDir := t.TempDir()
	if err := os.Mkdir(roomDir+"/timeline.jsonl", 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("BASHY_ROOM_DIR", roomDir)
	r := &countingRunner{}
	_, err := Invoke(context.Background(), Options{
		Agent: "codex", Instruction: "must not leave phantom work", Cwd: t.TempDir(), ReadOnly: true,
	}, r)
	if err == nil || r.calls.Load() != 0 {
		t.Fatalf("Invoke error=%v runner calls=%d", err, r.calls.Load())
	}
	entries, readErr := os.ReadDir(roomDir + "/members")
	if readErr != nil {
		t.Fatal(readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("publication failure left %d phantom membership cards", len(entries))
	}
}

func TestOneShotCardIDIsBounded(t *testing.T) {
	id := oneShotCardID(Launch{Nick: strings.Repeat("very-long-name", 100)})
	if len(id) > 64 {
		t.Fatalf("one-shot card ID length = %d, want <=64: %q", len(id), id)
	}
}
