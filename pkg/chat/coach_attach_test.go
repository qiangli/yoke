package chat

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/room"
)

// ctlSockServer is a minimal stand-in for a live coachee's control socket: it
// accepts the frames NewCtlSteerer writes (one connection per ESC/Say) and
// accumulates them so a test can assert a steer actually crossed the wire. It
// models "the coachee": leaving its listener open proves attach did not kill it.
type ctlSockServer struct {
	ln     net.Listener
	mu     bytesMutex
	path   string
	closed bool
}

// bytesMutex guards a byte buffer with a mutex.
type bytesMutex struct {
	mu sync.RWMutex
	b  []byte
}

func (bm *bytesMutex) add(p []byte) {
	bm.mu.Lock()
	bm.b = append(bm.b, p...)
	bm.mu.Unlock()
}

func (bm *bytesMutex) get() []byte {
	bm.mu.RLock()
	defer bm.mu.RUnlock()
	out := make([]byte, len(bm.b))
	copy(out, bm.b)
	return out
} // newCtlSockServer listens on a short unix socket path (the ~104-byte address
// cap rules out a long tempdir path) and serves frames until Close.
func newCtlSockServer(t *testing.T) *ctlSockServer {
	t.Helper()
	// os.TempDir() is short enough; a 12-char hash keeps the full path well under
	// the unix-socket address limit.
	name := "coach-test-" + shortHash(time.Now().Format(time.RFC3339Nano)) + ".sock"
	path := filepath.Join(os.TempDir(), name)
	// A leftover from a previous run blocks the bind.
	_ = os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("listen %s: %v", path, err)
	}
	s := &ctlSockServer{ln: ln}
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return // listener closed (Close, or the test ended)
			}
			data, _ := io.ReadAll(conn)
			conn.Close()
			s.mu.add(data)
		}
	}()
	// The socket file is the coachee's footprint; remove it when the test ends so
	// the temp dir stays clean. The listener itself is closed by Close().
	t.Cleanup(func() { s.Close(); _ = os.Remove(path) })
	s.path = path
	return s
}

func countEsc(b []byte) int { return bytes.Count(b, []byte("\x00R")) }

// settle waits for the server's recorded byte count to stop growing — i.e. for
// every connection the coach already opened to be drained by the server's reader
// goroutine. It returns once two samples taken settleWindow apart are equal (or
// the overall cap elapses). This removes a race where a frame sent just before
// detach is recorded by the server AFTER the coach has exited.
func settle(s *ctlSockServer) []byte {
	const (
		window = 120 * time.Millisecond
		cap    = 1500 * time.Millisecond
	)
	deadline := time.Now().Add(cap)
	prev := len(s.mu.get())
	for {
		time.Sleep(window)
		cur := s.mu.get()
		if len(cur) == prev || time.Now().After(deadline) {
			return cur
		}
		prev = len(cur)
	}
}

// dialAble reports whether the coachee's control socket still accepts
// connections — the observable sign the coachee is alive and was not killed.
func (s *ctlSockServer) dialAble() bool {
	c, err := net.DialTimeout("unix", s.path, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

func (s *ctlSockServer) Close() {
	s.mu.mu.Lock()
	if !s.closed {
		s.closed = true
		s.ln.Close()
	}
	s.mu.mu.Unlock()
}

// attachCard builds a room card for a fake coachee served by s, with a temp log
// the test writes a looping stream into.
func attachCard(t *testing.T, s *ctlSockServer, events bool) (room.Card, *os.File) {
	t.Helper()
	logF, err := os.CreateTemp("", "coach-attach-log-*.log")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { logF.Close(); os.Remove(logF.Name()) })
	card := room.Card{
		ID:      "attach-test-" + shortHash(s.path),
		Tool:    "agy",
		Binding: "agy:test-attach",
		Mode:    "weave",
		CtlSock: s.path,
		LogPath: logF.Name(),
		PID:     os.Getpid(), // alive: the test process
		Events:  events,
	}
	return card, logF
}

// appendLines writes the given lines (newline-terminated) to the coachee's log,
// simulating a live agent churning output a tailing coach will read.
func appendLines(t *testing.T, logF *os.File, lines []string) {
	t.Helper()
	var b strings.Builder
	for _, ln := range lines {
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	if _, err := logF.WriteString(b.String()); err != nil {
		t.Fatal(err)
	}
}

// churnLines is a rotating set of significant, distinct-looking lines that drives
// the pty novelty window below its floor (the same shape TestPtyDetectsChurnLoop
// uses) — i.e. a coachee stuck in a loop.
func churnLines(n int) []string {
	acts := []string{
		"run_tests on package ./attachsvc",
		"read_file internal/attachsvc/svc.go",
		"the tests are still failing here",
		"let me check the implementation again",
	}
	var out []string
	for i := 0; i < n; i++ {
		out = append(out, acts[i%len(acts)])
	}
	return out
}

// SUCCESS CRITERIA 2: a coach attached to a fake member with a working control
// socket, fed a looping line stream, must emit a steer THROUGH THAT SOCKET — ESC
// (the verbatim frame) first, then the spoken steer. This is the end-to-end
// proof that attach wires the room member → steerer → coach → socket.
func TestAttachCoachSteersThroughSocket(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false /* events: pty path */)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatalf("attachCoach: %v", err)
	}
	defer coach.Wait()

	// Feed the coachee's loop AFTER attaching: the watcher's file offset is at EOF,
	// so it picks these appends up on its next poll, just like a live run.
	appendLines(t, logF, churnLines(80))

	// The steer is async across a 500ms-tailed read + a socket dial. Poll for it.
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		got := srv.mu.get()
		if bytes.Contains(got, []byte("\x00R")) && bytes.Contains(got, []byte("STOP investigating")) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := srv.mu.get()
	if !bytes.Contains(got, []byte("\x00R")) {
		t.Fatalf("ESC frame never crossed the control socket; coachee would not be broken out of its loop. got=%q", got)
	}
	if !bytes.Contains(got, []byte("STOP investigating")) {
		t.Errorf("the spoken steer never crossed the control socket. got=%q", got)
	}
	if c := countEsc(got); c == 0 {
		t.Errorf("expected at least one ESC, got %d", c)
	}
	cancel() // detach
}

// SUCCESS CRITERIA 3a: attach REFUSES a member with an empty control socket —
// there is nothing to steer through, so it must not pretend to coach.
func TestAttachCoachRefusesEmptyCtlSock(t *testing.T) {
	isolateRoom(t)
	card := room.Card{ID: "no-sock", Binding: "agy:nosock", PID: os.Getpid(), LogPath: "/dev/null"}
	_, err := attachCoach(context.Background(), card, DefaultCoachPolicy(), false)
	if err == nil {
		t.Fatal("attach must refuse a member with no control socket")
	}
	if !strings.Contains(err.Error(), "control socket") {
		t.Errorf("refusal reason = %q, want it to name the missing control socket", err)
	}
}

// SUCCESS CRITERIA 3b: attach REFUSES a member whose pid is gone — a dead
// session. (The room prunes dead cards on read, but attach checks liveness
// itself so it never coaches a card a race just left behind.)
func TestAttachCoachRefusesDeadSession(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, _ := attachCard(t, srv, false)
	card.PID = 999999 // not running
	_, err := attachCoach(context.Background(), card, DefaultCoachPolicy(), false)
	if err == nil {
		t.Fatal("attach must refuse a dead session (pid gone)")
	}
	if !strings.Contains(err.Error(), "not running") {
		t.Errorf("refusal reason = %q, want it to say the session is not running", err)
	}
}

// SUCCESS CRITERIA 3b (the resolver side): the SAME resolver `chat steer` uses
// (findMember) reports an unknown session rather than guessing. attach routes its
// target through that resolver, so it inherits this refusal for free.
func TestAttachCoachResolverRefusesUnknownSession(t *testing.T) {
	isolateRoom(t) // empty room — nothing matches
	if _, err := findMember("no-such-session"); err == nil {
		t.Fatal("findMember must refuse an unknown session id rather than guess")
	}
}

// SUCCESS CRITERIA 3 (the discoverable prefix): a member resolves by an
// unambiguous id prefix, mirroring `chat steer` — so an operator types less.
func TestAttachResolvesUnambiguousPrefix(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, _ := attachCard(t, srv, false)
	if err := room.Join(card); err != nil {
		t.Fatal(err)
	}
	defer room.Leave(card.ID)

	// Resolve on a short unambiguous prefix of the id.
	got, ok, err := room.Find(card.ID[:12])
	if err != nil || !ok {
		t.Fatalf("room.Find(prefix) = %v,%v,%v; want the member resolved", got, ok, err)
	}
	if got.ID != card.ID {
		t.Errorf("resolved id = %q, want %q", got.ID, card.ID)
	}
}

// SUCCESS CRITERIA 4: detach leaves the coachee running. Cancelling the watch
// context (Ctrl-C / --timeout) ends the watcher; the coachee is neither killed
// nor pelted with an ESC storm as attach exits.
func TestAttachCoachDetachLeavesCoacheeRunning(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false)

	pol := DefaultCoachPolicy()
	ctx, cancel := context.WithCancel(context.Background())

	coach, err := attachCoach(ctx, card, pol, false)
	if err != nil {
		t.Fatalf("attachCoach: %v", err)
	}

	appendLines(t, logF, churnLines(120))

	// Let the coach read the loop and steer a bounded number of times.
	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if countEsc(srv.mu.get()) >= 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	cancel()     // detach (Ctrl-C / --timeout)
	coach.Wait() // watcher drains and exits

	// A frame the watcher sent just before exiting may still be in flight: the
	// sender has closed its side of the socket, but the server's reader goroutine
	// has not necessarily ReadAll'd+recorded it yet. settle() waits for the byte
	// count to stop growing, so atDetach is the true post-detach baseline rather
	// than a snapshot that a draining connection is about to bump.
	atDetach := settle(srv)
	escAtDetach := countEsc(atDetach)

	// Hold the watch dead for a while and assert NOTHING new crosses the socket —
	// no ESC storm on exit.
	time.Sleep(700 * time.Millisecond)
	after := settle(srv)
	if len(after) != len(atDetach) {
		t.Errorf("ESC storm on detach: %d bytes at detach, %d after — detach must not pelt the coachee", len(atDetach), len(after))
	}
	if countEsc(after) != escAtDetach {
		t.Errorf("ESC count rose after detach: %d -> %d — detach must leave the coachee alone", escAtDetach, countEsc(after))
	}
	// The coachee's control socket still accepts connections: attach did not kill
	// it. (attach never owns the coachee process.)
	if !srv.dialAble() {
		t.Errorf("coachee's control socket is gone after detach — attach must leave the coachee running")
	}
	// And the steers never exceeded the cap (no storm while running either).
	if max := countEsc(after); max > pol.MaxSteers {
		t.Errorf("ESC count %d exceeds MaxSteers %d — the cap must bound interventions", max, pol.MaxSteers)
	}
}

// --read-only is a watch with NO intervention: the coach sees the loop (it trips
// internally — the report records it) but sends NOTHING across the control
// socket. The invariant is that a coach is a report channel; read-only drops even
// ESC+Say, for an operator who wants to observe with zero chance of disturbance.
func TestAttachCoachReadOnlySendsNothing(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), true /* read-only */)
	if err != nil {
		t.Fatalf("attachCoach: %v", err)
	}

	appendLines(t, logF, churnLines(120))

	// Give the watcher plenty of time to read the loop and (internally) trip.
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		// read-only must send nothing, so the only exit is the cap being hit
		// internally OR the deadline; either way the socket stays empty.
		time.Sleep(100 * time.Millisecond)
	}
	cancel()
	coach.Wait()

	got := srv.mu.get()
	if len(got) != 0 {
		t.Errorf("read-only attach sent %d bytes across the control socket — it must watch without intervening: %q", len(got), got)
	}
	// But the coach DID watch: it recorded the loop as a trip in its report.
	rep := coach.Report()
	if len(rep.Steers) == 0 {
		t.Errorf("read-only attach recorded no trips — it must still WATCH (the report is the point); total=%d distinct=%d", rep.Total, rep.Distinct)
	}
	if rep.Total == 0 {
		t.Errorf("read-only attach saw no output lines — the watcher did not tail the log")
	}
}

// When the card's log file cannot be opened (the file is gone or the path is
// unreachable), attachCoach must return an error — not a coach that silently
// watches nothing. A coach whose watcher returns immediately (log is nil) is
// indistinguishable from one that legitimately saw zero output, which means an
// operator gets a report of "0 lines" with no sign that the attach was
// dead-on-arrival.
func TestAttachCoachErrorsWhenLogCannotBeOpened(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card := room.Card{
		ID:      "test-missing-log-" + shortHash(srv.path),
		Binding: "agy:test",
		CtlSock: srv.path,
		LogPath: "/tmp/definitely/does/not/exist/coach-attach-log-" + shortHash(srv.path) + ".log",
		PID:     os.Getpid(),
		Events:  false,
	}
	_, err := attachCoach(context.Background(), card, DefaultCoachPolicy(), false)
	if err == nil {
		t.Fatal("attachCoach must return an error when the log file cannot be opened — the coach would watch nothing, and the caller has no way to know")
	}
}

// A card written BEFORE cards advertised their own EventsPath still resolves,
// by reconstructing the old binding+pid key.
//
// This used to be the only path, and the comment here used to say the two
// functions "must agree" — which was the hazard, not the guarantee: one hash
// spelled out in two places, forever. A card now publishes EventsPath and
// readers use it; what survives is this fallback, and it is correct precisely
// because it reproduces the behaviour such a card had when it was written.
func TestCardEventsFileFallsBackToBindingAndPid(t *testing.T) {
	binding := "ycode:glm-5.2"
	pid := os.Getpid()
	card := room.Card{Binding: binding, PID: pid, Events: true} // no EventsPath: legacy
	got := cardEventsFile(card)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".bashy", "events", shortHash(binding+"\x00"+strconv.Itoa(pid))+".ndjson")
	if got != want {
		t.Errorf("cardEventsFile = %q\nwant the legacy binding+pid path %q", got, want)
	}
	if !strings.HasSuffix(got, ".ndjson") {
		t.Errorf("cardEventsFile = %q, want an .ndjson path", got)
	}
}

// The events watcher calls drain() (which advances offset to after the last \n),
// then skipToEnd() (which sets offset to EOF). Content written BETWEEN those two
// calls — or a partial line whose bytes sit between drain's offset and the EOF —
// is permanently lost: the live watcher starts reading from skipToEnd's offset
// and never revisits the bytes behind it.
//
// Reproducer: an events file with a trailing partial line (no closing \n).
// drain() consumes the complete lines, advances offset past the last \n, and
// skipToEnd() leaps over the partial bytes. When the writer later appends \n to
// finish the line, the watcher reads the lone \n — but the payload of the line
// (the name, the input) sits before its offset and is forever invisible.
func TestAttachCoachEventsLosesPartialLineContent(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)

	binding := "ycode:test-partial-loss"
	evPath := cardEventsFile(room.Card{Binding: binding, PID: os.Getpid(), Events: true})
	os.MkdirAll(filepath.Dir(evPath), 0o700)

	// Two complete tool calls, plus a third whose trailing \n is deliberately
	// absent — the file shape a live writer produces between the write(2) of the
	// JSON bytes and the write(2) of the newline.
	data := `{"type":"tool.call","data":{"name":"prior_a","input":{}}}` + "\n" +
		`{"type":"tool.call","data":{"name":"prior_b","input":{}}}` + "\n" +
		`{"type":"tool.call","data":{"name":"lost","input":{}}}`
	os.WriteFile(evPath, []byte(data), 0o600)
	defer os.Remove(evPath)

	card := room.Card{
		ID:      "test-partial-loss-" + shortHash(srv.path),
		Binding: binding,
		CtlSock: srv.path,
		LogPath: "/dev/null",
		PID:     os.Getpid(),
		Events:  true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}

	// The writer finishes the partial line (appends a closing \n) AND writes one
	// new complete event — the watcher should observe BOTH. With the bug, the \n
	// is observed alone and the "lost" payload is behind skipToEnd's offset.
	f, err := os.OpenFile(evPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("\n"))
	f.Write([]byte(`{"type":"tool.call","data":{"name":"live_new","input":{}}}` + "\n"))
	f.Close()

	// Wait for the watcher's tick (300ms) to drain new content.
	time.Sleep(time.Second)
	cancel()
	coach.Wait()

	rep := coach.Report()
	t.Logf("total=%d distinct=%d steers=%d", rep.Total, rep.Distinct, len(rep.Steers))

	// The bug: skipToEnd leapt over "lost". Without it, all four distinct tool
	// calls (prior_a, prior_b, lost, live_new) should be visible. With the bug,
	// only prior_a, prior_b, and live_new are — distinct=3 instead of 4.
	if rep.Distinct < 4 {
		t.Errorf("expected at least 4 distinct tool calls (prior_a, prior_b, lost, live_new), got %d — the partial-line payload was lost", rep.Distinct)
	}
	if rep.Total < 4 {
		t.Errorf("expected at least 4 total tool calls, got %d — the partial-line event is missing from the report", rep.Total)
	}
}

// The events watcher starts its eventTail at offset 0 — it replays the ENTIRE
// events-file history on the first drain(), including tool calls the coachee made
// BEFORE the coach attached. An attach to a session that has been running for 20
// minutes trips on its past — a coach must tail from the CURRENT position, not
// from the session's birth.
func TestAttachCoachEventsModeReplaysPastHistory(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)

	binding := "ycode:test-events-past"
	evPath := cardEventsFile(room.Card{Binding: binding, PID: os.Getpid(), Events: true})
	os.MkdirAll(filepath.Dir(evPath), 0o700)

	// A coachee that has been running for a while: 5 identical tool.call events,
	// well past the RepeatThreshold of 3. A coach that attaches now must NOT trip
	// on work that was already done.
	var b strings.Builder
	for i := 0; i < 5; i++ {
		b.WriteString(`{"type":"tool.call","timestamp":"2025-07-27T00:00:00Z","data":{"name":"read_file","input":{"path":"foo.go"}}}` + "\n")
	}
	os.WriteFile(evPath, []byte(b.String()), 0o600)
	defer os.Remove(evPath)

	card := room.Card{
		ID:      "test-events-past",
		Binding: binding,
		CtlSock: srv.path,
		LogPath: "/dev/null",
		PID:     os.Getpid(),
		Events:  true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(time.Second) // let the watcher drain the file
	cancel()
	coach.Wait()

	rep := coach.Report()
	if rep.Total == 0 {
		t.Fatal("events watcher read zero tool calls — the watcher did not tail the events file at all")
	}
	if len(rep.Steers) > 0 {
		t.Fatalf("coach tripped on %d historical steers from past tool calls: the events watcher must tail from the CURRENT position, not replay the session's entire history. total=%d distinct=%d steers=%v",
			len(rep.Steers), rep.Total, rep.Distinct, rep.Steers)
	}
}

// The pty watcher (watchAttachedLog) opens the log and reads from byte 0 — no
// seek-to-end, no skip. A coach attached to a coachee that has been running for
// 20 minutes replays the ENTIRE log history through feedPty, and when that
// history contains a looping pattern the coach trips on work done before it
// arrived. The events path correctly preserves history for the report via
// recordPriorToolCalls+skipToEnd; the pty path must do the same but does not.
func TestAttachCoachPtyModeReplaysPastHistory(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false /* pty */)

	// A coachee that has been running for a while: 120 lines of churn — a
	// rotating set of 4 distinct patterns repeated 30 times each. In a fresh
	// 40-line sliding window, 4 distinct / 40 windowed = 0.10, well below the
	// PtyNoveltyFloor of 0.35. The coach WILL trip on this loop.
	appendLines(t, logF, churnLines(120))

	pol := DefaultCoachPolicy()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coach, err := attachCoach(ctx, card, pol, false)
	if err != nil {
		t.Fatal(err)
	}

	// Give the watcher time to read history. It reads from byte 0, so it
	// processes all 120 pre-written lines.
	time.Sleep(2 * time.Second)
	cancel()
	coach.Wait()

	rep := coach.Report()
	if rep.Total == 0 {
		t.Fatal("pty watcher read zero lines — the watcher did not tail the log at all")
	}
	if len(rep.Steers) > 0 {
		t.Fatalf("pty coach tripped on %d historical steers from past output: the pty watcher must tail from the CURRENT position, not replay the session's entire history. total=%d distinct=%d steers=%v",
			len(rep.Steers), rep.Total, rep.Distinct, rep.Steers)
	}
}

func waitForAttachReport(t *testing.T, coach *Coach, wantTotal int) CoachReport {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		rep := coach.Report()
		if rep.Total >= wantTotal {
			return rep
		}
		time.Sleep(25 * time.Millisecond)
	}
	rep := coach.Report()
	t.Fatalf("coach report never reached %d total entries: total=%d distinct=%d", wantTotal, rep.Total, rep.Distinct)
	return CoachReport{}
}

func toolCallLine(name string) string {
	return `{"type":"tool.call","data":{"name":"` + name + `","input":{}}}`
}

func TestAttachCoachPtyMidLineDeliveredOnceWhole(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false)
	if _, err := logF.WriteString("prior_complete\nboundary"); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := logF.WriteString("_completed\nlive_new\n"); err != nil {
		t.Fatal(err)
	}
	rep := waitForAttachReport(t, coach, 3)
	cancel()
	coach.Wait()

	if rep.Total != 3 || rep.Distinct != 3 {
		t.Fatalf("partial PTY line must be delivered once and whole: total=%d distinct=%d, want 3/3", rep.Total, rep.Distinct)
	}
}

func TestAttachCoachEventsMidLineDeliveredOnceWhole(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	binding := "ycode:test-partial-once"
	evPath := cardEventsFile(room.Card{Binding: binding, PID: os.Getpid(), Events: true})
	if err := os.MkdirAll(filepath.Dir(evPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath, []byte(toolCallLine("prior")+"\n"+toolCallLine("boundary")), 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(evPath)

	card := room.Card{
		ID: "events-partial-once", Binding: binding, CtlSock: srv.path,
		LogPath: "/dev/null", PID: os.Getpid(), Events: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(evPath, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("\n" + toolCallLine("live_new") + "\n"); err != nil {
		f.Close()
		t.Fatal(err)
	}
	f.Close()
	rep := waitForAttachReport(t, coach, 3)
	cancel()
	coach.Wait()

	if rep.Total != 3 || rep.Distinct != 3 {
		t.Fatalf("partial event must be delivered once and whole: total=%d distinct=%d, want 3/3", rep.Total, rep.Distinct)
	}
}

func TestAttachCoachErrorsWhenEventsAndLogCannotBeOpened(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	binding := "ycode:test-no-signal-" + shortHash(srv.path)
	evPath := cardEventsFile(room.Card{Binding: binding, PID: os.Getpid(), Events: true})
	_ = os.Remove(evPath)
	card := room.Card{
		ID: "events-no-signal", Binding: binding, CtlSock: srv.path,
		LogPath: filepath.Join(t.TempDir(), "missing", "capture.log"),
		PID:     os.Getpid(), Events: true,
	}
	_, err := attachCoach(context.Background(), card, DefaultCoachPolicy(), false)
	if err == nil {
		t.Fatal("attachCoach succeeded with neither a readable events source nor a readable PTY source")
	}
	if !strings.Contains(err.Error(), "nothing to watch") {
		t.Fatalf("error %q must clearly say there is nothing to watch", err)
	}
}

func TestAttachCoachEventsEmptyAtAttach(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	binding := "ycode:test-empty-at-attach"
	evPath := cardEventsFile(room.Card{Binding: binding, PID: os.Getpid(), Events: true})
	if err := os.MkdirAll(filepath.Dir(evPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(evPath)
	card := room.Card{
		ID: "events-empty", Binding: binding, CtlSock: srv.path,
		LogPath: "/dev/null", PID: os.Getpid(), Events: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath, []byte(toolCallLine("after_empty")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := waitForAttachReport(t, coach, 1)
	cancel()
	coach.Wait()
	if rep.Total != 1 || rep.Distinct != 1 {
		t.Fatalf("empty events source did not begin tailing at byte zero: total=%d distinct=%d", rep.Total, rep.Distinct)
	}
}

func TestAttachCoachPtyRecoversWhenSourceDisappears(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false)
	ctx, cancel := context.WithCancel(context.Background())
	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(card.LogPath); err != nil {
		t.Fatal(err)
	}
	time.Sleep(650 * time.Millisecond)
	if err := os.WriteFile(card.LogPath, []byte("after_recreate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := waitForAttachReport(t, coach, 1)
	cancel()
	coach.Wait()
	logF.Close()
	if rep.Total != 1 || rep.Distinct != 1 {
		t.Fatalf("PTY tail did not recover after its path reappeared: total=%d distinct=%d", rep.Total, rep.Distinct)
	}
}

func TestAttachCoachEventsRecoversWhenSourceDisappears(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	binding := "ycode:test-events-reappear"
	evPath := cardEventsFile(room.Card{Binding: binding, PID: os.Getpid(), Events: true})
	if err := os.MkdirAll(filepath.Dir(evPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(evPath)
	card := room.Card{
		ID: "events-reappear", Binding: binding, CtlSock: srv.path,
		LogPath: "/dev/null", PID: os.Getpid(), Events: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(evPath); err != nil {
		t.Fatal(err)
	}
	time.Sleep(400 * time.Millisecond)
	if err := os.WriteFile(evPath, []byte(toolCallLine("after_recreate")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := waitForAttachReport(t, coach, 1)
	cancel()
	coach.Wait()
	if rep.Total != 1 || rep.Distinct != 1 {
		t.Fatalf("events tail did not recover after its path reappeared: total=%d distinct=%d", rep.Total, rep.Distinct)
	}
}

func TestAttachCoachPtyFollowsTruncationAndRotation(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false)
	ctx, cancel := context.WithCancel(context.Background())
	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}
	appendLines(t, logF, []string{"first_generation_line"})
	waitForAttachReport(t, coach, 1)

	if err := os.Truncate(card.LogPath, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(card.LogPath, []byte("second_after_truncate\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForAttachReport(t, coach, 2)

	rotated := card.LogPath + ".old"
	if err := os.Rename(card.LogPath, rotated); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(rotated)
	if err := os.WriteFile(card.LogPath, []byte("third_after_rotation\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := waitForAttachReport(t, coach, 3)
	cancel()
	coach.Wait()
	if rep.Total != 3 || rep.Distinct != 3 {
		t.Fatalf("PTY tail lost or duplicated a generation: total=%d distinct=%d, want 3/3", rep.Total, rep.Distinct)
	}
}

func TestAttachCoachEventsFollowsTruncationAndRotation(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	binding := "ycode:test-events-generations"
	evPath := cardEventsFile(room.Card{Binding: binding, PID: os.Getpid(), Events: true})
	if err := os.MkdirAll(filepath.Dir(evPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(evPath)
	card := room.Card{
		ID: "events-generations", Binding: binding, CtlSock: srv.path,
		LogPath: "/dev/null", PID: os.Getpid(), Events: true,
	}
	ctx, cancel := context.WithCancel(context.Background())
	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath, []byte(toolCallLine("first")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForAttachReport(t, coach, 1)

	if err := os.Truncate(evPath, 0); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath, []byte(toolCallLine("second_after_truncate")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitForAttachReport(t, coach, 2)

	rotated := evPath + ".old"
	if err := os.Rename(evPath, rotated); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(rotated)
	if err := os.WriteFile(evPath, []byte(toolCallLine("third_after_rotation")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	rep := waitForAttachReport(t, coach, 3)
	cancel()
	coach.Wait()
	if rep.Total != 3 || rep.Distinct != 3 {
		t.Fatalf("events tail lost or duplicated a generation: total=%d distinct=%d, want 3/3", rep.Total, rep.Distinct)
	}
}

// When a same-inode log file is truncated to a smaller size while a coach is
// attached, the tail resets to offset 0 and re-reads the truncated content as
// live data. Lines that were already recorded as prior BEFORE the truncation
// are counted a second time as NEW live data — inflating the report total.
//
// This is a real defect: skipToEnd correctly positions at EOF and recordPriorPty
// preserves prior content, but the same-inode size-shrink path in next() resets
// t.off to 0 without accounting for already-seen content. A truncation that
// leaves a complete prior line intact replays that line through feedPty, which
// counts it in c.total. Since recordPriorPty does not set c.ptyLast, the
// consecutive-duplicate gate in feedPty does not catch it either.
func TestAttachCoachPtyReplaysContentAfterSameInodeTruncation(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false)

	// Write two lines BEFORE attaching — these become prior content.
	preData := "abcdefgh_content_alpha\nabcdefgh_content_beta\n"
	if _, err := logF.WriteString(preData); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}

	// skipToEnd read both lines, recordPriorPty recorded them: priorTotal=2.
	// Truncate the file to include only the first line (same inode, smaller size).
	firstLineLen := int64(len("abcdefgh_content_alpha\n"))
	if err := os.Truncate(card.LogPath, firstLineLen); err != nil {
		t.Fatal(err)
	}

	// Wait for the watcher's first poll (500ms). The size-shrink resets t.off to 0,
	// re-reads the truncated prefix, and feeds the first line through feedPty as if
	// it were new live data.
	time.Sleep(700 * time.Millisecond)

	cancel()
	coach.Wait()

	rep := coach.Report()
	// Without the defect: total=2 (prior only — a truncation is less data,
	// not new data). Distinct=2 (both unique lines from prior).
	// With the defect: total=3 (priorTotal=2 + 1 replayed live line).
	t.Logf("total=%d distinct=%d steers=%d", rep.Total, rep.Distinct, len(rep.Steers))
	if rep.Total != 2 {
		t.Errorf("same-inode truncation replayed prior content as live: total=%d distinct=%d, want total=2 (prior only, no live data from truncation)", rep.Total, rep.Distinct)
	}
}

// Events path: same-inode truncation of the NDJSON events file replays prior
// tool.call events through decide, inflating both total and — critically —
// seeding the live detector with already-completed calls. A truncated events file
// whose prefix is a single prior call (once, never seen by the live detector) will
// not trip; but if the prefix contains repeated calls, a coach that attached after
// a long run will spuriously intervene on work done before it arrived.
func TestAttachCoachEventsReplaysContentAfterSameInodeTruncation(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)

	binding := "ycode:test-events-trunc-replay"
	evPath := cardEventsFile(room.Card{Binding: binding, PID: os.Getpid(), Events: true})
	os.MkdirAll(filepath.Dir(evPath), 0o700)

	data := toolCallLine("event_alpha") + "\n" + toolCallLine("event_beta") + "\n"
	os.WriteFile(evPath, []byte(data), 0o600)
	defer os.Remove(evPath)

	firstLineLen := int64(len(toolCallLine("event_alpha") + "\n"))

	card := room.Card{
		ID:      "test-events-trunc-replay-" + shortHash(srv.path),
		Binding: binding,
		CtlSock: srv.path,
		LogPath: "/dev/null",
		PID:     os.Getpid(),
		Events:  true,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	coach, err := attachCoach(ctx, card, DefaultCoachPolicy(), false)
	if err != nil {
		t.Fatal(err)
	}

	os.Truncate(evPath, firstLineLen)

	time.Sleep(700 * time.Millisecond)
	cancel()
	coach.Wait()

	rep := coach.Report()
	t.Logf("events: total=%d distinct=%d steers=%d", rep.Total, rep.Distinct, len(rep.Steers))
	if rep.Total != 2 {
		t.Errorf("events: same-inode truncation replayed prior event as live: total=%d distinct=%d, want total=2 (prior only)", rep.Total, rep.Distinct)
	}
}

// ── ROLES ──────────────────────────────────────────────────────────────────
//
// The role is an ENFORCED EFFECT CAP, so every role test asserts the cap at the
// wire — what did or did not cross the coachee's control socket — rather than
// asserting that some flag was set. The capability table under test:
//
//	role         observe   say   interrupt (ESC)
//	observer     yes       no    no
//	advisor      yes       yes   no
//	supervisor   yes       yes   yes

// writeChurn feeds a looping stream into a coachee's log from a goroutine (where
// t.Fatal is not allowed), so the tail picks it up as LIVE output.
func writeChurn(w io.Writer, n int) {
	var b strings.Builder
	for _, ln := range churnLines(n) {
		b.WriteString(ln)
		b.WriteByte('\n')
	}
	_, _ = io.WriteString(w, b.String())
}

// runAttachRole attaches in the given role to a fake coachee, drives it into a
// loop, and waits until the reflex policy has actually TRIPPED (or the deadline
// passes). Returning only after a trip is what makes the per-role assertions
// meaningful: "observer sent nothing" proves a cap only if there was something to
// send.
func runAttachRole(t *testing.T, role AttachRole) (*ctlSockServer, CoachReport, *roleSteerer) {
	t.Helper()
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false /* pty path */)

	ctx, cancel := context.WithCancel(context.Background())
	coach, steer, err := attachCoachAs(ctx, card, DefaultCoachPolicy(), role)
	if err != nil {
		cancel()
		t.Fatalf("attachCoachAs(%s): %v", role, err)
	}

	// Feed the loop AFTER attaching: the tail sits at EOF, so this arrives as live
	// output, exactly as a running agent would produce it.
	writeChurn(logF, 120)

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if len(coach.Report().Steers) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	// The Say follows the ESC after a 150ms settle inside intervene; let the frames
	// that a trip produces finish crossing before the socket is inspected.
	settle(srv)
	cancel()
	coach.Wait()

	rep := coach.Report()
	if len(rep.Steers) == 0 {
		t.Fatalf("role %s: the reflex policy never tripped, so this test proves nothing about its cap: total=%d distinct=%d", role, rep.Total, rep.Distinct)
	}
	return srv, rep, steer
}

// SUCCESS CRITERIA 2 (observer): an observer emits NOTHING even when the policy
// trips. Not a quieter steer, not a Say without the ESC — zero bytes. The trip is
// still detected and recorded, which is the "demote, never drop" half: the
// operator learns the agent is looping, the agent is not touched.
func TestAttachRoleObserverEmitsNothingWhenPolicyTrips(t *testing.T) {
	srv, rep, steer := runAttachRole(t, RoleObserver)

	got := settle(srv)
	if len(got) != 0 {
		t.Errorf("observer emitted %d bytes across the control socket — an observer must not be able to affect the coachee at all: %q", len(got), got)
	}
	// It observed: the trip is in the report even though nothing was sent.
	if rep.Total == 0 {
		t.Errorf("observer saw no output — an observer must still WATCH (the report is the whole point)")
	}
	// And the prevented effects were counted, not silently discarded.
	if n := steer.Demotions(); n < len(rep.Steers) {
		t.Errorf("observer demotions = %d for %d trips — every capped effect must be recorded, not dropped", n, len(rep.Steers))
	}
}

// SUCCESS CRITERIA 2 (advisor): an advisor MAY say a sentence and may NEVER send
// the raw escape byte. This is the load-bearing distinction of the whole feature:
// a Say is queued and read at a turn boundary, so an advisor can change where the
// agent goes next; only ESC breaks into the turn already running.
func TestAttachRoleAdvisorSaysButNeverSendsEsc(t *testing.T) {
	srv, rep, steer := runAttachRole(t, RoleAdvisor)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(srv.mu.get(), []byte("STOP investigating")) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := settle(srv)
	if !bytes.Contains(got, []byte("STOP investigating")) {
		t.Errorf("advisor's steer never crossed the control socket — an advisor MAY say a sentence. got=%q", got)
	}
	// VerbatimFrame("\x1b") is encoded as the "\x00R" frame prefix; a TextFrame Say
	// never contains it. Its absence is the proof no ESC was sent.
	if n := countEsc(got); n != 0 {
		t.Errorf("advisor sent %d ESC frame(s) — an advisor that can reach Interrupt IS a supervisor. got=%q", n, got)
	}
	if bytes.Contains(got, []byte{0x1b}) {
		t.Errorf("advisor put a raw escape byte on the wire: %q", got)
	}
	// The ESC the policy asked for was demoted, once per trip, not dropped.
	if n := steer.Demotions(); n < len(rep.Steers) {
		t.Errorf("advisor demotions = %d for %d trips — a capped ESC must be recorded as demoted", n, len(rep.Steers))
	}
}

// SUCCESS CRITERIA 2 (supervisor): a supervisor does BOTH — the shipped behavior,
// unchanged by the introduction of roles.
func TestAttachRoleSupervisorSaysAndInterrupts(t *testing.T) {
	srv, _, steer := runAttachRole(t, RoleSupervisor)

	deadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(deadline) {
		got := srv.mu.get()
		if countEsc(got) > 0 && bytes.Contains(got, []byte("STOP investigating")) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	got := settle(srv)
	if countEsc(got) == 0 {
		t.Errorf("supervisor sent no ESC frame — breaking into a running turn is the power that defines the role. got=%q", got)
	}
	if !bytes.Contains(got, []byte("STOP investigating")) {
		t.Errorf("supervisor's steer never crossed the control socket. got=%q", got)
	}
	if n := steer.Demotions(); n != 0 {
		t.Errorf("supervisor had %d effect(s) demoted — a supervisor is capped by nothing", n)
	}
}

// The capability table itself, asserted directly. The wire tests above prove the
// caps are enforced; this pins the table they are enforcing, so a future role
// cannot quietly acquire ESC by being added to the wrong branch.
func TestAttachRoleCapabilityTable(t *testing.T) {
	for _, tc := range []struct {
		role                        AttachRole
		observe, say, interruptable bool
	}{
		{RoleObserver, true, false, false},
		{RoleAdvisor, true, true, false},
		{RoleSupervisor, true, true, true},
	} {
		if got := tc.role.CanObserve(); got != tc.observe {
			t.Errorf("%s.CanObserve() = %v, want %v", tc.role, got, tc.observe)
		}
		if got := tc.role.CanSay(); got != tc.say {
			t.Errorf("%s.CanSay() = %v, want %v", tc.role, got, tc.say)
		}
		if got := tc.role.CanInterrupt(); got != tc.interruptable {
			t.Errorf("%s.CanInterrupt() = %v, want %v", tc.role, got, tc.interruptable)
		}
	}
	// Least-powerful first: the ladder order is what help texts and errors show.
	want := []AttachRole{RoleObserver, RoleAdvisor, RoleSupervisor}
	got := AttachRoles()
	if len(got) != len(want) {
		t.Fatalf("AttachRoles() = %v, want the three P1 roles %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("AttachRoles() = %v, want least-to-most power %v", got, want)
		}
	}
}

// SUCCESS CRITERIA 3: `coach attach` still means --as supervisor. It is
// documented and in use; adding a role flag must not change what an existing
// invocation does. Asserted end to end through the real cobra command against a
// real room member — the flag default AND the effect it produces.
func TestCoachAttachDefaultsToSupervisor(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, logF := attachCard(t, srv, false)
	if err := room.Join(card); err != nil {
		t.Fatal(err)
	}
	defer room.Leave(card.ID)

	cmd := newCoachAttachCmd()
	if def := cmd.Flags().Lookup("as").DefValue; def != string(RoleSupervisor) {
		t.Fatalf("`coach attach` --as default = %q, want %q — the shipped alias must keep its meaning", def, RoleSupervisor)
	}

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{card.ID, "--timeout", "3s"})

	go func() {
		time.Sleep(400 * time.Millisecond)
		writeChurn(logF, 120)
	}()
	if err := cmd.Execute(); err != nil {
		t.Fatalf("coach attach: %v", err)
	}

	got := settle(srv)
	if countEsc(got) == 0 {
		t.Errorf("`coach attach` with no --as sent no ESC — it must still behave as --as supervisor. got=%q", got)
	}
	if !bytes.Contains(got, []byte("STOP investigating")) {
		t.Errorf("`coach attach` with no --as spoke no steer. got=%q", got)
	}
	if !strings.Contains(out.String(), "as supervisor") {
		t.Errorf("`coach attach` did not report the role it joined with: %q", out.String())
	}
}

// The new front door defaults to the LEAST powerful role. `coach attach` keeps
// supervisor because that is what it already meant; a fresh verb has no such
// history, and "let me join this session" must not hand over the power to stop it.
func TestAttachCmdDefaultsToObserver(t *testing.T) {
	cmd := NewAttachCmd()
	if def := cmd.Flags().Lookup("as").DefValue; def != string(RoleObserver) {
		t.Fatalf("`attach` --as default = %q, want %q — a new front door must default to the least power", def, RoleObserver)
	}
}

// attachAudit collects the participation notes this feature emits on the room
// timeline, in order.
func attachAudit(t *testing.T, kind string) []room.Event {
	t.Helper()
	events, err := room.Timeline(0)
	if err != nil {
		t.Fatal(err)
	}
	var out []room.Event
	for _, e := range events {
		if e.Type == room.EventNote && strings.HasPrefix(e.Body, kind+" role=") {
			out = append(out, e)
		}
	}
	return out
}

// SUCCESS CRITERIA 4: attaching and detaching each leave a timeline record
// carrying actor, role, and target. A participant nobody can attribute is not a
// participant — the same rule bus.Publish enforces with its required Principal —
// and only the PAIR bounds the window in which the attachment could have acted.
func TestAttachEmitsRoomEventsOnAttachAndDetach(t *testing.T) {
	isolateRoom(t)
	t.Setenv("BASHY_PRINCIPAL", "audit-tester")
	srv := newCtlSockServer(t)
	card, _ := attachCard(t, srv, false)

	ctx, cancel := context.WithCancel(context.Background())
	coach, _, err := attachCoachAs(ctx, card, DefaultCoachPolicy(), RoleAdvisor)
	if err != nil {
		cancel()
		t.Fatal(err)
	}

	// The attach note is emitted synchronously, before the watcher starts, so it is
	// already on the timeline here — and the detach note is not.
	joins := attachAudit(t, "attach")
	if len(joins) != 1 {
		t.Fatalf("attach emitted %d timeline notes, want exactly 1", len(joins))
	}
	if joins[0].Actor != "audit-tester" || joins[0].Principal != "audit-tester" {
		t.Errorf("attach note actor=%q principal=%q, want the attributed principal", joins[0].Actor, joins[0].Principal)
	}
	if joins[0].Target != card.ID {
		t.Errorf("attach note target=%q, want the session %q", joins[0].Target, card.ID)
	}
	if !strings.Contains(joins[0].Body, "role=advisor") {
		t.Errorf("attach note body=%q, want it to carry the role it joined with", joins[0].Body)
	}
	if n := len(attachAudit(t, "detach")); n != 0 {
		t.Fatalf("%d detach notes before detaching — the pair must bound the actual window", n)
	}

	cancel()
	coach.Wait()

	leaves := attachAudit(t, "detach")
	if len(leaves) != 1 {
		t.Fatalf("detach emitted %d timeline notes, want exactly 1", len(leaves))
	}
	if leaves[0].Actor != "audit-tester" || leaves[0].Target != card.ID {
		t.Errorf("detach note actor=%q target=%q, want actor and target attributed", leaves[0].Actor, leaves[0].Target)
	}
	if !strings.Contains(leaves[0].Body, "role=advisor") {
		t.Errorf("detach note body=%q, want it to carry the role", leaves[0].Body)
	}
}

// A refused attach is NOT audited as participation: it never happened, so there
// is no window to bound and nothing it could have affected.
func TestAttachRefusalEmitsNoAudit(t *testing.T) {
	isolateRoom(t)
	card := room.Card{ID: "no-sock", Binding: "agy:nosock", PID: os.Getpid(), LogPath: "/dev/null"}
	if _, _, err := attachCoachAs(context.Background(), card, DefaultCoachPolicy(), RoleSupervisor); err == nil {
		t.Fatal("attach must refuse a member with no control socket")
	}
	if n := len(attachAudit(t, "attach")); n != 0 {
		t.Errorf("a refused attach wrote %d participation notes — nothing joined, so nothing to record", n)
	}
}

// SUCCESS CRITERIA 5: an unknown --as is REFUSED, with a message that lists the
// valid roles. Never defaulted — silently promoting a typo to a powerful role is
// exactly the failure roles exist to prevent, and silently demoting it would hide
// the typo behind an attachment that quietly does nothing.
func TestAttachRefusesUnknownRole(t *testing.T) {
	for _, bad := range []string{"root", "admin", "SUPERVISER", "", "obs"} {
		got, err := ParseAttachRole(bad)
		if err == nil {
			t.Fatalf("ParseAttachRole(%q) = %q with no error — an unknown role must be refused, never defaulted", bad, got)
		}
		for _, name := range AttachRoleNames() {
			if !strings.Contains(err.Error(), name) {
				t.Errorf("ParseAttachRole(%q) error %q does not list the valid role %q", bad, err, name)
			}
		}
	}
	// The P1 boundary is stated rather than left to read as a typo: judge and pair
	// author, and attaching an author needs an isolation story that does not exist.
	for _, notYet := range []string{"judge", "pair"} {
		_, err := ParseAttachRole(notYet)
		if err == nil {
			t.Fatalf("ParseAttachRole(%q) must be refused — it is not implemented", notYet)
		}
		if !strings.Contains(err.Error(), "not implemented") {
			t.Errorf("ParseAttachRole(%q) error %q should say it is not implemented, not that it is unknown", notYet, err)
		}
	}
	// Valid spellings still parse, case-insensitively and with surrounding space.
	for _, ok := range []string{"observer", "Advisor", " supervisor "} {
		if _, err := ParseAttachRole(ok); err != nil {
			t.Errorf("ParseAttachRole(%q) = %v, want it accepted", ok, err)
		}
	}
}

// The refusal reaches the operator through the command, before any session is
// resolved or watched — an attach with a bad role must not run at all.
func TestAttachCmdRefusesUnknownRole(t *testing.T) {
	isolateRoom(t)
	cmd := NewAttachCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetContext(context.Background())
	cmd.SetArgs([]string{"some-session", "--as", "root"})
	err := cmd.Execute()
	if err == nil {
		t.Fatal("`attach --as root` must fail — an unknown role is never silently defaulted")
	}
	for _, name := range AttachRoleNames() {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error %q does not list the valid role %q", err, name)
		}
	}
}

// attachCoachAs refuses a role it does not recognize even when called directly —
// the cap cannot be bypassed by constructing an AttachRole value in Go. Its zero
// value is deliberately not a role.
func TestAttachCoachAsRefusesUnknownRoleValue(t *testing.T) {
	isolateRoom(t)
	srv := newCtlSockServer(t)
	card, _ := attachCard(t, srv, false)
	if _, _, err := attachCoachAs(context.Background(), card, DefaultCoachPolicy(), AttachRole("root")); err == nil {
		t.Fatal("attachCoachAs must refuse an unrecognized role rather than treat it as observable")
	}
	if _, _, err := attachCoachAs(context.Background(), card, DefaultCoachPolicy(), AttachRole("")); err == nil {
		t.Fatal("the zero AttachRole must not be a valid role — a role is chosen, never inferred")
	}
}

// --read-only predates --as and means observer. When both are given they must
// agree: capping an explicit `--as supervisor` down would hide a contradiction the
// operator wrote on purpose, and honouring it would defeat --read-only.
func TestAttachReadOnlyAndRoleMustAgree(t *testing.T) {
	// --read-only alone still resolves to observer, whatever the command's default.
	cmd := newCoachAttachCmd()
	if err := cmd.Flags().Parse([]string{"--read-only"}); err != nil {
		t.Fatal(err)
	}
	role, err := resolveAttachRole(cmd, cmd.Flags().Lookup("as").Value.String(), true, RoleSupervisor)
	if err != nil {
		t.Fatalf("--read-only alone: %v", err)
	}
	if role != RoleObserver {
		t.Errorf("--read-only resolved to %q, want %q", role, RoleObserver)
	}

	// --read-only with an explicit contradicting --as is refused, naming both.
	cmd = newCoachAttachCmd()
	if err := cmd.Flags().Parse([]string{"--read-only", "--as", "supervisor"}); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveAttachRole(cmd, "supervisor", true, RoleSupervisor); err == nil {
		t.Fatal("`--read-only --as supervisor` must be refused — the two flags contradict")
	} else if !strings.Contains(err.Error(), "--read-only") || !strings.Contains(err.Error(), "supervisor") {
		t.Errorf("contradiction error %q must name both flags", err)
	}

	// --read-only with an explicit --as observer agrees, and is allowed.
	cmd = newCoachAttachCmd()
	if err := cmd.Flags().Parse([]string{"--read-only", "--as", "observer"}); err != nil {
		t.Fatal(err)
	}
	if role, err := resolveAttachRole(cmd, "observer", true, RoleSupervisor); err != nil || role != RoleObserver {
		t.Errorf("`--read-only --as observer` = %q,%v; the two agree and must be accepted", role, err)
	}
}

// The role cap sits on the Steerer — the only surface an attachment can emit
// through — so it holds for any caller, not just the attach path. Asserted
// without a socket: an observer's and an advisor's capped calls must not even
// reach the inner steerer.
func TestRoleSteererCapsAtTheSteerer(t *testing.T) {
	for _, tc := range []struct {
		role                 AttachRole
		wantSay, wantInterru bool
	}{
		{RoleObserver, false, false},
		{RoleAdvisor, true, false},
		{RoleSupervisor, true, true},
	} {
		inner := &recordingSteerer{}
		s := &roleSteerer{role: tc.role, inner: inner}
		if err := s.Interrupt(); err != nil {
			t.Fatalf("%s.Interrupt: %v", tc.role, err)
		}
		if err := s.Say("hello"); err != nil {
			t.Fatalf("%s.Say: %v", tc.role, err)
		}
		if got := inner.interrupts > 0; got != tc.wantInterru {
			t.Errorf("%s: reached inner Interrupt = %v, want %v", tc.role, got, tc.wantInterru)
		}
		if got := len(inner.said) > 0; got != tc.wantSay {
			t.Errorf("%s: reached inner Say = %v, want %v", tc.role, got, tc.wantSay)
		}
		wantDemotions := 0
		if !tc.wantInterru {
			wantDemotions++
		}
		if !tc.wantSay {
			wantDemotions++
		}
		if got := s.Demotions(); got != wantDemotions {
			t.Errorf("%s: demotions = %d, want %d — every capped effect is recorded, never dropped", tc.role, got, wantDemotions)
		}
	}
}

// recordingSteerer is a Steerer that records rather than sends, so the cap can be
// asserted without a control socket.
type recordingSteerer struct {
	interrupts int
	said       []string
}

func (r *recordingSteerer) Interrupt() error      { r.interrupts++; return nil }
func (r *recordingSteerer) Say(text string) error { r.said = append(r.said, text); return nil }

func TestAttachCoachDropsIncompleteLineWhenCoacheeDies(t *testing.T) {
	t.Run("pty", func(t *testing.T) {
		f, err := os.CreateTemp("", "coach-dead-partial-pty-*.log")
		if err != nil {
			t.Fatal(err)
		}
		path := f.Name()
		defer os.Remove(path)
		if _, err := f.WriteString("never_completed"); err != nil {
			t.Fatal(err)
		}
		f.Close()
		tail := &attachLineTail{path: path}
		if _, err := tail.skipToEnd(); err != nil {
			t.Fatal(err)
		}
		coach := NewLineCoach(DefaultCoachPolicy(), NewCtlSteerer(""))
		watchAttachedLog(context.Background(), coach, room.Card{PID: 999999}, tail)
		if rep := coach.Report(); rep.Total != 0 {
			t.Fatalf("incomplete PTY line from a dead coachee was treated as complete: total=%d", rep.Total)
		}
		if string(tail.rem) != "never_completed" {
			t.Fatalf("pending PTY remainder changed: %q", tail.rem)
		}
	})

	t.Run("events", func(t *testing.T) {
		f, err := os.CreateTemp("", "coach-dead-partial-events-*.ndjson")
		if err != nil {
			t.Fatal(err)
		}
		path := f.Name()
		defer os.Remove(path)
		if _, err := f.WriteString(toolCallLine("never_completed")); err != nil {
			t.Fatal(err)
		}
		f.Close()
		tail := &attachLineTail{path: path}
		if _, err := tail.skipToEnd(); err != nil {
			t.Fatal(err)
		}
		coach := newCoach(DefaultCoachPolicy())
		watchAttachedEvents(context.Background(), coach, tail, 999999)
		if rep := coach.Report(); rep.Total != 0 {
			t.Fatalf("incomplete event from a dead coachee was treated as complete: total=%d", rep.Total)
		}
		if string(tail.rem) != toolCallLine("never_completed") {
			t.Fatalf("pending event remainder changed: %q", tail.rem)
		}
	})
}
