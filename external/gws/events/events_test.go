package events

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/external/gws"
)

// canary is a fake secret: it must never surface in errors, responses or
// persisted state.
const canary = "CANARY-ya29.s3cr3t-0f2e"

var bind = gws.Binding{Target: "gmail:users/test@example.invalid", Query: "history"}

func openStore(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, bind)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func codeOf(err error) gws.Code {
	var ge *gws.Error
	if errors.As(err, &ge) {
		return ge.Code
	}
	return ""
}

func noSleep(ctx context.Context, d time.Duration) error { return ctx.Err() }

// fakeSource replays a fixed script of batches keyed by cursor.
type fakeSource struct {
	batches map[string]Batch
	errs    map[string][]error // consumed in order before the batch is served
	calls   []string
}

func (f *fakeSource) Poll(ctx context.Context, cursor string) (Batch, error) {
	f.calls = append(f.calls, cursor)
	if q := f.errs[cursor]; len(q) > 0 {
		f.errs[cursor] = q[1:]
		return Batch{}, q[0]
	}
	b, ok := f.batches[cursor]
	if !ok {
		return Batch{Next: cursor}, nil
	}
	return b, nil
}

func ev(id string) Event { return Event{ID: id, Payload: json.RawMessage(`{"id":"` + id + `"}`)} }

func TestOpenIsOptInAndClaimed(t *testing.T) {
	if _, err := Open("", bind); codeOf(err) != gws.CodeInvalidInput {
		t.Fatalf("empty dir must refuse (opt-in), got %v", err)
	}
	dir := t.TempDir()
	s := openStore(t, dir)
	if _, err := Open(dir, bind); err == nil || !strings.Contains(err.Error(), "another consumer") {
		t.Fatalf("second consumer must be refused, got %v", err)
	}
	if err := s.Resync("100", "initial watch historyId"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	other := gws.Binding{Target: bind.Target, Query: "other"}
	// Different binding → different file; same file under a forged binding → mismatch.
	raw, _ := os.ReadFile(s.Path())
	var cp Checkpoint
	_ = json.Unmarshal(raw, &cp)
	cp.Query = other.Query
	forged, _ := json.Marshal(cp)
	if err := os.WriteFile(s.Path(), forged, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(dir, bind); codeOf(err) != gws.CodeCursorMismatch {
		t.Fatalf("checkpoint bound elsewhere must fail cursor-mismatch, got %v", err)
	}
	if runtime.GOOS != "windows" {
		if fi, _ := os.Stat(s.Path()); fi.Mode().Perm() != 0o600 {
			t.Fatalf("checkpoint mode = %v, want 0600", fi.Mode().Perm())
		}
	}
}

func TestOrderedAckAndRestartRecovery(t *testing.T) {
	dir := t.TempDir()
	src := &fakeSource{batches: map[string]Batch{
		"100": {Events: []Event{ev("101"), ev("102"), ev("103")}, Next: "103"},
		"103": {Events: []Event{ev("104")}, Next: "104"},
	}}
	s := openStore(t, dir)
	_ = s.Resync("100", "initial")
	var got []string
	crash := true
	h := func(ctx context.Context, e Event) error {
		if e.ID == "102" && crash {
			return errors.New("downstream action crashed")
		}
		got = append(got, e.ID)
		return nil
	}
	p := &Poller{Source: src, Store: s, Handler: h, Sleep: noSleep}
	res, err := p.RunOnce(context.Background())
	if codeOf(err) != gws.CodePartialMutation || !res.Partial {
		t.Fatalf("handler failure must stop partial with unknown outcome, got %v %+v", err, res)
	}
	if c := s.Checkpoint(); c.Cursor != "100" || len(c.Seen) != 1 || c.Seen[0] != "101" {
		t.Fatalf("cursor must not advance past an unacknowledged event: %+v", c)
	}
	s.Close()

	// Restart: a new process reopens the checkpoint and resumes.
	crash = false
	s = openStore(t, dir)
	defer s.Close()
	p = &Poller{Source: src, Store: s, Handler: h, Sleep: noSleep}
	res, err = p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(got, ",") != "101,102,103,104" {
		t.Fatalf("delivery order = %v", got)
	}
	if res.Duplicates != 1 || res.Delivered != 3 || !res.CaughtUp || res.Cursor != "104" {
		t.Fatalf("restart result = %+v", res)
	}
	if s.Checkpoint().Cursor != "104" {
		t.Fatal("cursor not persisted")
	}
}

func TestDuplicateDelivery(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	_ = s.Resync("1", "initial")
	src := &fakeSource{batches: map[string]Batch{
		"1": {Events: []Event{ev("a"), ev("a"), ev("b")}, Next: "2"},
		"2": {Events: []Event{ev("b"), ev("c")}, Next: "3"}, // overlap across batches
	}}
	calls := map[string]int{}
	p := &Poller{Source: src, Store: s, Sleep: noSleep, Handler: func(ctx context.Context, e Event) error { calls[e.ID]++; return nil }}
	res, err := p.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for id, n := range calls {
		if n != 1 {
			t.Fatalf("event %s handled %d times", id, n)
		}
	}
	if res.Duplicates != 2 || res.Delivered != 3 {
		t.Fatalf("result = %+v", res)
	}
	// Events without ids cannot be deduplicated: refuse the batch.
	_ = s.Resync("9", "test")
	src.batches["9"] = Batch{Events: []Event{{ID: ""}}, Next: "10"}
	if _, err := p.RunOnce(context.Background()); codeOf(err) != gws.CodeProviderFailure || s.Checkpoint().Cursor != "9" {
		t.Fatalf("id-less event must not be acknowledged: %v", err)
	}
}

func TestDedupWindowIsBounded(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	s.window = 3
	for _, id := range []string{"1", "2", "3", "4"} {
		_ = s.Ack(id)
	}
	if s.Seen("1") || !s.Seen("4") || len(s.Checkpoint().Seen) != 3 {
		t.Fatalf("window = %v", s.Checkpoint().Seen)
	}
}

func TestExpiredCursorNeedsExplicitResync(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	_ = s.Resync("100", "initial")
	expired := gws.NewError(gws.CodeCursorExpired, "history expired", nil)
	src := &fakeSource{errs: map[string][]error{"100": {expired}}, batches: map[string]Batch{
		"500": {Events: []Event{ev("501")}, Next: "501"},
	}}
	p := &Poller{Source: src, Store: s, Sleep: noSleep, Handler: func(context.Context, Event) error { return nil }}
	if _, err := p.RunOnce(context.Background()); codeOf(err) != gws.CodeCursorExpired {
		t.Fatalf("want cursor-expired, got %v", err)
	}
	if len(src.calls) != 1 {
		t.Fatalf("expired cursor must not be retried: %d calls", len(src.calls))
	}
	if s.Checkpoint().Cursor != "100" {
		t.Fatal("expired cursor must not reset the checkpoint silently")
	}
	if err := s.Resync("500", ""); codeOf(err) != gws.CodeInvalidInput {
		t.Fatal("resync must require a reason")
	}
	if err := s.Resync("500", "historyId expired; re-watched"); err != nil {
		t.Fatal(err)
	}
	if res, err := p.RunOnce(context.Background()); err != nil || res.Cursor != "501" {
		t.Fatalf("after resync: %+v %v", res, err)
	}
	if c := s.Checkpoint(); c.Resyncs != 2 || c.LastResync != "historyId expired; re-watched" {
		t.Fatalf("resync not recorded: %+v", c)
	}
	// No starting cursor at all is refused, never guessed.
	s2, _ := Open(t.TempDir(), bind)
	defer s2.Close()
	if _, err := (&Poller{Source: src, Store: s2}).RunOnce(context.Background()); codeOf(err) != gws.CodeInvalidInput {
		t.Fatalf("missing cursor: %v", err)
	}
}

func TestRetryBackoffRateLimitAndRetryAfter(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	_ = s.Resync("1", "initial")
	rl := gws.NewError(gws.CodeRateLimit, "slow down", &gws.Error{Retryable: true, RetryAfter: "7"})
	src := &fakeSource{errs: map[string][]error{"1": {rl, rl}}, batches: map[string]Batch{"1": {Events: []Event{ev("x")}, Next: "2"}}}
	var waits []time.Duration
	sleep := func(ctx context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	clock := time.Unix(0, 0)
	lim := &Limiter{Interval: 3 * time.Second, Now: func() time.Time { return clock }}
	p := &Poller{Source: src, Store: s, Sleep: sleep, Limiter: lim,
		Backoff: Backoff{Base: time.Second, Max: 5 * time.Second, Attempts: 4},
		Handler: func(context.Context, Event) error { return nil }}
	res, err := p.RunOnce(context.Background())
	if err != nil || res.Delivered != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	// Two retry waits: retry-after 7s capped at Max 5s, both times; then the
	// limiter spaces the second poll 3s after the first.
	want := []time.Duration{5 * time.Second, 5 * time.Second, 3 * time.Second}
	if fmt.Sprint(waits) != fmt.Sprint(want) {
		t.Fatalf("waits = %v, want %v", waits, want)
	}
	// Exhaustion surfaces the typed error.
	_ = s.Resync("7", "test")
	src.errs["7"] = []error{rl, rl, rl, rl}
	if _, err := p.RunOnce(context.Background()); codeOf(err) != gws.CodeRateLimit {
		t.Fatalf("exhausted retries: %v", err)
	}
	if d := (Backoff{Base: time.Second, Max: time.Minute}).delay(2, ""); d != 4*time.Second {
		t.Fatalf("exponential delay = %v", d)
	}
}

func TestCancellationKeepsCheckpoint(t *testing.T) {
	s := openStore(t, t.TempDir())
	defer s.Close()
	_ = s.Resync("1", "initial")
	src := &fakeSource{batches: map[string]Batch{"1": {Events: []Event{ev("a"), ev("b")}, Next: "2"}}}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Poller{Source: src, Store: s, Sleep: noSleep, Handler: func(context.Context, Event) error { cancel(); return nil }}
	res, err := p.RunOnce(ctx)
	if codeOf(err) != gws.CodeCancelled || !res.Partial {
		t.Fatalf("want cancelled partial, got %v %+v", err, res)
	}
	if c := s.Checkpoint(); c.Cursor != "1" || len(c.Seen) != 1 {
		t.Fatalf("cancellation advanced past completed processing: %+v", c)
	}
	// Cancellation interrupts a backoff wait too.
	ctx2, cancel2 := context.WithCancel(context.Background())
	cancel2()
	err = retry(ctx2, DefaultBackoff, ctxSleep, func() error { return nil })
	if codeOf(err) != gws.CodeCancelled {
		t.Fatalf("retry under cancelled ctx: %v", err)
	}
	dctx, dcancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer dcancel()
	err = retry(dctx, Backoff{Base: time.Hour, Max: time.Hour, Attempts: 2}, ctxSleep, func() error {
		return gws.NewError(gws.CodeProviderFailure, "503", nil)
	})
	if codeOf(err) != gws.CodeTimeout {
		t.Fatalf("deadline during backoff: %v", err)
	}
}

func TestCheckpointHoldsNoCredentials(t *testing.T) {
	dir := t.TempDir()
	s := openStore(t, dir)
	_ = s.Resync("1", "initial")
	src := &fakeSource{batches: map[string]Batch{"1": {Events: []Event{ev("a")}, Next: "2"}}}
	p := &Poller{Source: src, Store: s, Sleep: noSleep, Secrets: []string{canary},
		Handler: func(context.Context, Event) error { return nil }}
	if _, err := p.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	s.Close()
	matches, _ := filepath.Glob(filepath.Join(dir, "*"))
	for _, m := range matches {
		raw, _ := os.ReadFile(m)
		if strings.Contains(string(raw), canary) {
			t.Fatalf("%s persisted a credential", m)
		}
	}
}

func TestNoListenerInPackage(t *testing.T) {
	files, _ := filepath.Glob("*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		raw, _ := os.ReadFile(f)
		for _, bad := range []string{"net.Listen", "ListenAndServe", "http.Serve("} {
			if strings.Contains(string(raw), bad) {
				t.Fatalf("%s contains %s: hosting a receiver is a managed service (Rule 1)", f, bad)
			}
		}
	}
	if codeOf(ServeAdmission()) != gws.CodeUnsupported {
		t.Fatal("hosting must be disabled until Rule 1 evidence exists")
	}
}
