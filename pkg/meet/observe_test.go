package meet

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// pinStore points the meeting store at a scratch dir and returns the meeting's
// transcript path, so a test can play the writer.
func pinStore(t *testing.T, st *State) string {
	t.Helper()
	t.Setenv("BASHY_MEET_DIR", t.TempDir())
	if err := st.save(); err != nil {
		t.Fatal(err)
	}
	dir, err := storeDir(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "transcript.jsonl")
}

func turn(t *testing.T, id string, e Event) {
	t.Helper()
	if err := appendEvent(id, e); err != nil {
		t.Fatal(err)
	}
}

func testState() *State {
	return &State{
		ID: "m1", Topic: "the cache", Status: "open",
		Participants: []string{"claude-fable5", "codex-gpt-5.5"},
		Secretary:    "claude",
	}
}

// Attaching replays the WHOLE history, in full, before anything new — you join
// a conversation already in progress and need to know what was said.
func TestObserveReplaysFullHistory(t *testing.T) {
	fleettest.Ring(t)
	st := testState()
	pinStore(t, st)
	long := strings.Repeat("a line of reasoning\n", 50)
	turn(t, st.ID, Event{Round: 1, Speaker: "claude-fable5", Kind: "turn", Text: long, TS: time.Now()})
	turn(t, st.ID, Event{Round: 1, Speaker: "codex-gpt-5.5", Kind: "turn", Text: "disagree", TS: time.Now()})

	var out, errW bytes.Buffer
	if err := observeMeeting(context.Background(), &out, &errW, st, observeOpts{follow: false}); err != nil {
		t.Fatal(err)
	}
	got := out.String()

	// Full text, not a preview: an observer attached to read the log wants the
	// log. All 50 lines, or the "entire history" claim is false.
	if n := strings.Count(got, "a line of reasoning"); n != 50 {
		t.Errorf("history was truncated: %d of 50 lines survived", n)
	}
	if !strings.Contains(got, "disagree") {
		t.Error("the second turn is missing")
	}
	if !strings.Contains(got, "claude-fable5 (") {
		t.Error("a seat should be shown with its human name beside the canonical one")
	}
}

// See pkg/bus TestPublishPrincipalIsRemoved: retired flags are removed, not
// aliased. -n is gone; the canonical spelling is --tail.
func TestObserveNShorthandIsRemoved(t *testing.T) {
	st := testState()
	pinStore(t, st)
	turn(t, st.ID, Event{Round: 1, Speaker: "claude-fable5", Kind: "turn", Text: "old", TS: time.Now()})

	cmd := newObserveCmd()
	if flag := cmd.Flags().Lookup("n"); flag != nil {
		t.Fatalf("-n must not be registered at all, got %#v", flag)
	}
	var out, errW bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errW)
	cmd.SetArgs([]string{st.ID, "--follow=false", "-n", "1"})
	if err := cmd.Execute(); err == nil {
		t.Fatal("-n must be rejected; the canonical spelling is --tail")
	}
}

// Following streams turns as they land. Granularity is one COMPLETED turn: the
// engine writes an event when an agent finishes, so this is what "live" means.
func TestObserveStreamsNewTurnsAndStopsWhenClosed(t *testing.T) {
	st := testState()
	pinStore(t, st)
	turn(t, st.ID, Event{Round: 1, Speaker: "claude-fable5", Kind: "turn", Text: "first", TS: time.Now()})

	var out, errW syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- observeMeeting(context.Background(), &out, &errW, st, observeOpts{follow: true})
	}()

	time.Sleep(2 * observePoll)
	turn(t, st.ID, Event{Round: 2, Speaker: "codex-gpt-5.5", Kind: "turn", Text: "second", TS: time.Now()})
	time.Sleep(2 * observePoll)

	// The closing turn is written BEFORE the status flips — an observer that
	// exited on the flag alone would truncate the meeting's last word.
	turn(t, st.ID, Event{Round: 2, Speaker: "claude", Kind: "decision", Text: "write-through", TS: time.Now()})
	closed := testState()
	closed.Status = "closed"
	if err := closed.save(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("observe did not stop when the meeting closed")
	}

	got := out.String()
	for _, want := range []string{"first", "second", "write-through"} {
		if !strings.Contains(got, want) {
			t.Errorf("stream lost %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "DECISION") {
		t.Error("a decision should be labelled, not shown as a speaker")
	}
	if !strings.Contains(errW.String(), "closed") {
		t.Error("the observer must say the meeting ended")
	}
}

// Detaching is not the meeting ending. A reader of the scrollback has to be
// able to tell the two apart.
func TestObserveDetachSaysSo(t *testing.T) {
	st := testState()
	pinStore(t, st)
	ctx, cancel := context.WithCancel(context.Background())

	var out, errW syncBuffer
	done := make(chan error, 1)
	go func() { done <- observeMeeting(ctx, &out, &errW, st, observeOpts{follow: true}) }()
	time.Sleep(2 * observePoll)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelling the context must detach the observer")
	}
	if !strings.Contains(errW.String(), "detached") {
		t.Errorf("detaching must be reported, not silent: %q", errW.String())
	}
	if strings.Contains(errW.String(), "meeting closed") {
		t.Error("detaching must not be reported as the meeting ending")
	}
}

// The writer appends whole events, but a reader can still catch one in flight.
// Half an event parsed as a whole one would be a silently corrupt turn in the
// observer's log — so a partial line is HELD, not guessed at.
func TestTailHoldsBackAPartialLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	tl := &lineTail{path: path}

	full, err := json.Marshal(Event{Round: 1, Speaker: "a", Kind: "turn", Text: "whole"})
	if err != nil {
		t.Fatal(err)
	}
	// A complete line, followed by half of the next one.
	torn := append(append([]byte{}, full...), '\n')
	torn = append(torn, []byte(`{"round":2,"speaker":"b","te`)...)
	if err := os.WriteFile(path, torn, 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := readEvents(tl)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "whole" {
		t.Fatalf("only the complete event may surface, got %+v", got)
	}

	// The rest of the torn line arrives; now — and only now — it is an event.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("xt\":\"rest\",\"kind\":\"turn\"}\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()

	got, err = readEvents(tl)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Text != "rest" || got[0].Speaker != "b" {
		t.Fatalf("the completed line must surface exactly once, got %+v", got)
	}
}

// Observing must never write. Any number of observers can attach, and attaching
// can never change what the meeting decides.
func TestObserveIsReadOnly(t *testing.T) {
	st := testState()
	path := pinStore(t, st)
	turn(t, st.ID, Event{Round: 1, Speaker: "claude-fable5", Kind: "turn", Text: "hi", TS: time.Now()})

	dir, err := storeDir(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sizeBefore := fileSize(t, path)

	var out, errW bytes.Buffer
	for range 3 { // three observers at once
		if err := observeMeeting(context.Background(), &out, &errW, st, observeOpts{follow: false}); err != nil {
			t.Fatal(err)
		}
	}

	after, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Errorf("observing created files: %d -> %d entries", len(before), len(after))
	}
	if got := fileSize(t, path); got != sizeBefore {
		t.Errorf("observing wrote to the transcript: %d -> %d bytes", sizeBefore, got)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return fi.Size()
}

// A typo'd --participant that silently matched nothing would leave the observer
// staring at a blank screen, concluding the meeting was quiet when it was in
// fact talking to somebody else. An empty view must be unreachable by mistake.
func TestObserveFilterRefusesAnUnseatedName(t *testing.T) {
	st := testState()
	if _, err := observeFilter(st, []string{"nobody"}, nil); err == nil {
		t.Fatal("filtering on a name that holds no seat must be refused, not silently empty")
	}
	if _, err := observeFilter(st, []string{"claude-fable5"}, nil); err != nil {
		t.Fatalf("a seated name must be accepted: %v", err)
	}
}

// The filter resolves aliases, because turns are RECORDED canonically: an
// observer filtering on `Sable` has to match turns recorded as `claude-fable5`.
func TestObserveFilterResolvesNicknames(t *testing.T) {
	nickOf := pinFleet(t)
	nick := nickOf("claude-fable5")

	st := testState()
	only, err := observeFilter(st, []string{nick}, nil)
	if err != nil {
		t.Fatalf("a nickname must name a seat: %v", err)
	}
	if !only("turn", "claude-fable5") {
		t.Errorf("filtering on %q must match the canonically-recorded seat", nick)
	}
	if only("turn", "codex-gpt-5.5") {
		t.Error("the filter must exclude other seats")
	}
}

func TestObserveKindFilter(t *testing.T) {
	st := testState()
	only, err := observeFilter(st, nil, []string{"decision"})
	if err != nil {
		t.Fatal(err)
	}
	if !only("decision", "") || only("turn", "") {
		t.Error("--kind must keep only the named kinds")
	}
}

// --- live streaming ------------------------------------------------------

// THE ASK: it should feel like attending, not like reading the minutes. An
// agent's answer appears LINE BY LINE as it writes it — not all at once,
// minutes later, when the turn completes.
func TestObserveStreamsLinesAsTheyAreWritten(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	var out, errW syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- observeMeeting(context.Background(), &out, &errW, st, observeOpts{follow: true})
	}()
	time.Sleep(2 * observePoll)

	// The agent takes the floor and writes, a chunk at a time, exactly as a
	// process flushes stdout — chunks that do NOT align with lines.
	w := newLiveWriter(st, "claude-fable5", "", "")
	w.Write([]byte("the cache should be write-through\nbecause a lost write is w"))
	time.Sleep(2 * observePoll)

	// Both COMPLETE lines are on screen already; the meeting is nowhere near done.
	mid := out.String()
	if !strings.Contains(mid, "the cache should be write-through") {
		t.Fatalf("a completed line must appear before the turn ends:\n%s", mid)
	}
	// ...but the half-line must NOT be, or the watcher reads a sentence the
	// agent never wrote.
	if strings.Contains(mid, "because a lost write is w") {
		t.Errorf("a partial line leaked to the watcher:\n%s", mid)
	}

	w.Write([]byte("orse than a slow one\n"))
	time.Sleep(2 * observePoll)
	if !strings.Contains(out.String(), "because a lost write is worse than a slow one") {
		t.Errorf("the line must appear once it is complete:\n%s", out.String())
	}

	// The turn completes: the whole thing lands in the transcript. It must NOT
	// be printed a second time — the watcher already saw it happen.
	w.close(statusOK)
	turn(t, st.ID, Event{
		Round: 1, Speaker: "claude-fable5", Kind: "turn", Status: statusOK, TS: time.Now(),
		Text: "the cache should be write-through\nbecause a lost write is worse than a slow one",
	})
	closed := testState()
	closed.Status = "closed"
	if err := closed.save(); err != nil {
		t.Fatal(err)
	}

	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("observe did not stop when the meeting closed")
	}

	if n := strings.Count(out.String(), "the cache should be write-through"); n != 1 {
		t.Errorf("a turn watched live must not be reprinted from the record (%d copies):\n%s",
			n, out.String())
	}
}

// An observer that attaches MID-turn missed the beginning, so it must take the
// whole turn from the record rather than show a truncated live view. Showing
// half a turn as if it were the turn is the failure this guards.
func TestObserveJoiningMidTurnTakesTheWholeTurnFromTheRecord(t *testing.T) {
	st := testState()
	pinStore(t, st)
	st.Round = 1

	// The agent is already speaking when the observer attaches.
	w := newLiveWriter(st, "claude-fable5", "", "")
	w.Write([]byte("first point\n"))

	var out, errW syncBuffer
	done := make(chan error, 1)
	go func() {
		done <- observeMeeting(context.Background(), &out, &errW, st, observeOpts{follow: true})
	}()
	time.Sleep(2 * observePoll)

	w.Write([]byte("second point\n"))
	w.close(statusOK)
	turn(t, st.ID, Event{
		Round: 1, Speaker: "claude-fable5", Kind: "turn", Status: statusOK, TS: time.Now(),
		Text: "first point\nsecond point",
	})
	closed := testState()
	closed.Status = "closed"
	if err := closed.save(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("observe did not stop")
	}

	// The part it missed must still reach it — from the transcript.
	got := out.String()
	if !strings.Contains(got, "first point") {
		t.Errorf("the part said before attaching must arrive from the record:\n%s", got)
	}
	if !strings.Contains(got, "second point") {
		t.Errorf("the rest of the turn is missing:\n%s", got)
	}
}

// The live channel is a VIEW; the transcript is the RECORD. Streaming must not
// change a single byte of what gets recorded — observing cannot perturb the
// meeting it observes.
func TestLiveWriterDoesNotAlterTheRecord(t *testing.T) {
	st := testState()
	pinStore(t, st)

	raw := "answer line one\nanswer line two\n"
	w := newLiveWriter(st, "claude-fable5", "", "")
	n, err := w.Write([]byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	// The tee must report the full write, or the agent's own stdout write
	// appears to fail and os/exec tears the process down.
	if n != len(raw) {
		t.Fatalf("the tee must consume the whole write: %d of %d", n, len(raw))
	}
	w.close(statusOK)

	// The live channel is a separate file. The transcript is untouched by it.
	if evs, err := readTranscript(st.ID); err != nil || len(evs) != 0 {
		t.Fatalf("streaming wrote to the transcript: %v %v", evs, err)
	}
}

// The live channel is sanitized exactly as the recorded turn is, so the watcher
// never sees something the transcript will not also contain — and ANSI escapes
// from a chatty CLI never garble the watching terminal.
func TestLiveLinesAreSanitizedLikeTheRecord(t *testing.T) {
	st := testState()
	pinStore(t, st)

	w := newLiveWriter(st, "claude-fable5", "", "")
	w.Write([]byte("\x1b[32mgreen\x1b[0m text\n"))
	w.close(statusOK)

	path, err := livePath(st.ID)
	if err != nil {
		t.Fatal(err)
	}
	lines, err := readLive(&lineTail{path: path})
	if err != nil {
		t.Fatal(err)
	}
	var saw bool
	for _, l := range lines {
		if l.Kind != liveLine {
			continue
		}
		saw = true
		if strings.Contains(l.Text, "\x1b") {
			t.Errorf("an ANSI escape reached the live channel: %q", l.Text)
		}
		if l.Text != "green text" {
			t.Errorf("live text = %q, want the sanitized %q", l.Text, "green text")
		}
	}
	if !saw {
		t.Error("no line reached the live channel")
	}
}

// syncBuffer is a bytes.Buffer safe to read while another goroutine writes it.
//
// The follow-mode tests run observeMeeting in a goroutine and then assert on
// what it has printed so far — that is the whole point of a streaming test. A
// plain bytes.Buffer makes every out.String() a data race against the streamer,
// which -race reports (observe_test.go read vs observe.go write) and which, race
// detector aside, can return a half-appended string and fail an assertion for
// reasons that have nothing to do with the behaviour under test.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}
