package dagrun

import (
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"
)

// slogIsolateEnv keeps the test off the real HOME/BASHY_HOME: all Hub
// directories come from t.TempDir and the environment is repointed there.
func slogIsolateEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("BASHY_HOME", filepath.Join(tmp, "bashy-home"))
	return tmp
}

func newSlogHub(t *testing.T, holder string) (*Hub, time.Time) {
	t.Helper()
	tmp := slogIsolateEnv(t)
	now := sessClock()
	sess, err := NewSession(
		sessFixtureID, sessFixtureWS, sessFixtureRun,
		KindInteractive, "exec-1", "linux", "amd64",
		holder, now,
	)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	hub, err := NewHub(filepath.Join(tmp, "home"), sess, nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub, now
}

// slogReadN reads exactly n bytes or fails the test; the timeout turns a
// stuck stream into a failure instead of hanging the suite.
func slogReadN(t *testing.T, r io.Reader, n int, what string) []byte {
	t.Helper()
	buf := make([]byte, n)
	type outcome struct {
		m   int
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		m, err := io.ReadFull(r, buf)
		done <- outcome{m, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("read %s: %v", what, res.err)
		}
		return buf[:res.m]
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out reading %s (%d bytes)", what, n)
		return nil
	}
}

// slogReadErr waits for the next Read error (eviction, EOF, loss).
func slogReadErr(t *testing.T, r io.Reader, what string) error {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		_, err := r.Read(make([]byte, 4096))
		done <- err
	}()
	select {
	case err := <-done:
		return err
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out waiting for read error on %s", what)
		return nil
	}
}

// TestSessionReattachSecondClientSeesOutputAndExit is the walking-skeleton
// slice: A attaches, output is appended, A vanishes; B attaches with since=0
// and sees every byte plus the original nonzero exit, recorded exactly once.
func TestSessionReattachSecondClientSeesOutputAndExit(t *testing.T) {
	hub, now := newSlogHub(t, "client-a")

	attA, err := hub.Attach("client-a", 0, now)
	if err != nil {
		t.Fatalf("A attach: %v", err)
	}

	var off int64
	for _, line := range []string{"tick 1\n", "tick 2\n", "tick 3\n"} {
		off, err = hub.Append([]byte(line))
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
	want := []byte("tick 1\ntick 2\ntick 3\n")
	if off != int64(len("tick 1\ntick 2\n")) {
		t.Fatalf("last append offset = %d, want %d", off, len("tick 1\ntick 2\n"))
	}

	// A detaches/vanishes.
	if err := hub.Detach("client-a", now.Add(time.Second)); err != nil {
		t.Fatalf("A detach: %v", err)
	}
	_ = attA.Close()
	if got := hub.Session().State; got != StateDetached {
		t.Fatalf("state after A detach = %q, want detached", got)
	}

	// B attaches with since=0 and replays everything.
	attB, err := hub.Attach("client-b", 0, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("B attach: %v", err)
	}
	defer attB.Close()
	if got := slogReadN(t, attB, len(want), "B replay"); string(got) != string(want) {
		t.Fatalf("B replay = %q, want %q", got, want)
	}

	// The run exits 7 while detached: detached -> exited, recorded once.
	if err := hub.RecordExit(7, now.Add(3*time.Second)); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}
	if err := hub.RecordExit(7, now.Add(4*time.Second)); !errors.Is(err, ErrSecondExit) {
		t.Fatalf("second RecordExit: got %v, want ErrSecondExit", err)
	}
	snap := hub.Session()
	if snap.State != StateExited {
		t.Fatalf("state = %q, want exited", snap.State)
	}
	if snap.ExitCode == nil || *snap.ExitCode != 7 {
		t.Fatalf("ExitCode = %v, want 7", snap.ExitCode)
	}

	// B drains to end-of-stream after the exit.
	if err := slogReadErr(t, attB, "B post-exit"); !errors.Is(err, io.EOF) {
		t.Fatalf("B post-exit read = %v, want EOF", err)
	}
}

// TestSessionTakeoverEvictsPreviousHolder: a second non-viewer attach by a
// different client takes over; the previous holder's stream gets evicted.
func TestSessionTakeoverEvictsPreviousHolder(t *testing.T) {
	hub, now := newSlogHub(t, "client-a")

	if _, err := hub.Append([]byte("hello\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	attA, err := hub.Attach("client-a", 0, now)
	if err != nil {
		t.Fatalf("A attach: %v", err)
	}
	defer attA.Close()

	attB, err := hub.Attach("client-b", 0, now.Add(time.Second))
	if err != nil {
		t.Fatalf("B takeover attach: %v", err)
	}
	defer attB.Close()

	if got := hub.Session().Holder; got != "client-b" {
		t.Fatalf("holder = %q, want client-b after takeover", got)
	}
	if err := slogReadErr(t, attA, "evicted A"); !errors.Is(err, ErrEvicted) {
		t.Fatalf("evicted A read = %v, want ErrEvicted", err)
	}

	// The taker-over still gets the full replay from its own offset.
	if got := slogReadN(t, attB, len("hello\n"), "B replay"); string(got) != "hello\n" {
		t.Fatalf("B replay = %q, want %q", got, "hello\n")
	}

	// Live appends after takeover stream to the new holder.
	if _, err := hub.Append([]byte("after\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := slogReadN(t, attB, len("after\n"), "B live"); string(got) != "after\n" {
		t.Fatalf("B live = %q, want %q", got, "after\n")
	}
}

func TestSessionReadOnlyViewersDoNotTakeOver(t *testing.T) {
	hub, now := newSlogHub(t, "client-a")

	if _, err := hub.Append([]byte("data\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	attA, err := hub.Attach("client-a", 0, now)
	if err != nil {
		t.Fatalf("A attach: %v", err)
	}
	defer attA.Close()

	// A viewer attaches: no takeover, holder unchanged.
	viewer, err := hub.Attach("viewer-1", 0, now.Add(time.Second), ReadOnly())
	if err != nil {
		t.Fatalf("viewer attach: %v", err)
	}
	defer viewer.Close()
	if got := hub.Session().Holder; got != "client-a" {
		t.Fatalf("holder after viewer attach = %q, want client-a", got)
	}
	if got := slogReadN(t, viewer, len("data\n"), "viewer replay"); string(got) != "data\n" {
		t.Fatalf("viewer replay = %q, want %q", got, "data\n")
	}

	// A new holder takes over: the old holder is evicted, the viewer is not.
	attB, err := hub.Attach("client-b", 0, now.Add(2*time.Second))
	if err != nil {
		t.Fatalf("B takeover attach: %v", err)
	}
	defer attB.Close()
	if err := slogReadErr(t, attA, "evicted A"); !errors.Is(err, ErrEvicted) {
		t.Fatalf("evicted A read = %v, want ErrEvicted", err)
	}
	if _, err := hub.Append([]byte("live\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if got := slogReadN(t, viewer, len("live\n"), "viewer live"); string(got) != "live\n" {
		t.Fatalf("viewer live = %q, want %q (viewers survive takeovers)", got, "live\n")
	}
}

func TestSessionLogAppendReadOffsets(t *testing.T) {
	hub, _ := newSlogHub(t, "")

	off0, err := hub.Append([]byte("abc"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if off0 != 0 {
		t.Fatalf("first append offset = %d, want 0", off0)
	}
	off1, err := hub.Append([]byte("defgh"))
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if off1 != 3 {
		t.Fatalf("second append offset = %d, want 3", off1)
	}

	data, next, err := hub.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom(0): %v", err)
	}
	if string(data) != "abcdefgh" || next != 8 {
		t.Fatalf("ReadFrom(0) = %q,%d want %q,8", data, next, "abcdefgh")
	}
	data, next, err = hub.ReadFrom(3)
	if err != nil {
		t.Fatalf("ReadFrom(3): %v", err)
	}
	if string(data) != "defgh" || next != 8 {
		t.Fatalf("ReadFrom(3) = %q,%d want %q,8", data, next, "defgh")
	}
	data, next, err = hub.ReadFrom(8)
	if err != nil {
		t.Fatalf("ReadFrom(8): %v", err)
	}
	if len(data) != 0 || next != 8 {
		t.Fatalf("ReadFrom(size) = %q,%d want empty,8", data, next)
	}

	for _, bad := range []int64{-1, 9, 1 << 40} {
		if _, _, err := hub.ReadFrom(bad); !errors.Is(err, ErrBadOffset) {
			t.Fatalf("ReadFrom(%d): got %v, want ErrBadOffset", bad, err)
		}
	}
	if _, err := hub.Attach("client-a", 99, sessClock()); !errors.Is(err, ErrBadOffset) {
		t.Fatalf("Attach with bad offset: got %v, want ErrBadOffset", err)
	}
}

func TestSessionLogSurvivesReopen(t *testing.T) {
	tmp := slogIsolateEnv(t)
	now := sessClock()
	sess, err := NewSession(sessFixtureID, sessFixtureWS, sessFixtureRun,
		KindInteractive, "exec-1", "linux", "amd64", "client-a", now)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	dir := filepath.Join(tmp, "home")
	hub, err := NewHub(dir, sess, nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	if _, err := hub.Append([]byte("before restart\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.RecordExit(7, now.Add(time.Second)); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}
	if err := hub.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Reopen the same workspace home: log bytes and the recorded exit survive.
	hub2, err := NewHub(dir, sess, nil)
	if err != nil {
		t.Fatalf("reopen NewHub: %v", err)
	}
	defer hub2.Close()
	data, next, err := hub2.ReadFrom(0)
	if err != nil {
		t.Fatalf("ReadFrom after reopen: %v", err)
	}
	if string(data) != "before restart\n" || next != int64(len("before restart\n")) {
		t.Fatalf("reopened log = %q,%d", data, next)
	}
	snap := hub2.Session()
	if snap.State != StateExited || snap.ExitCode == nil || *snap.ExitCode != 7 {
		t.Fatalf("reopened session = %+v, want exited with code 7", snap)
	}
}

func TestSessionLostHubSemantics(t *testing.T) {
	hub, now := newSlogHub(t, "client-a")

	attA, err := hub.Attach("client-a", 0, now)
	if err != nil {
		t.Fatalf("A attach: %v", err)
	}
	defer attA.Close()
	if _, err := hub.Append([]byte("partial\n")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.MarkLost(now.Add(time.Second)); err != nil {
		t.Fatalf("MarkLost: %v", err)
	}

	snap := hub.Session()
	if snap.State != StateLost {
		t.Fatalf("state = %q, want lost", snap.State)
	}
	if snap.State == StateDetached {
		t.Fatal("lost session reports detached")
	}
	if snap.ExitCode != nil {
		t.Fatalf("lost ExitCode = %v, want nil", *snap.ExitCode)
	}

	// Readers drain what survived, then learn the session was lost (not EOF,
	// not an exit).
	if got := slogReadN(t, attA, len("partial\n"), "A drain"); string(got) != "partial\n" {
		t.Fatalf("A drain = %q, want %q", got, "partial\n")
	}
	if err := slogReadErr(t, attA, "A post-loss"); !errors.Is(err, ErrSessionLost) {
		t.Fatalf("A post-loss read = %v, want ErrSessionLost", err)
	}

	// The log stays readable, but the dead session takes no more output and
	// no late attachers.
	if _, err := hub.Append([]byte("more")); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("Append after loss: got %v, want ErrSessionClosed", err)
	}
	if _, err := hub.Attach("client-b", 0, now.Add(2*time.Second)); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("attach after loss: got %v, want ErrSessionClosed", err)
	}
	if _, err := hub.Attach("client-b", 0, now.Add(2*time.Second), ReadOnly()); !errors.Is(err, ErrSessionClosed) {
		t.Fatalf("viewer attach after loss: got %v, want ErrSessionClosed", err)
	}
}

// slogStubPTY records forwarded frames for the PTY-interface test.
type slogStubPTY struct {
	inputs  [][]byte
	resizes [][2]uint16
	signals []string
}

func (s *slogStubPTY) WriteInput(p []byte) (int, error) {
	cp := append([]byte(nil), p...)
	s.inputs = append(s.inputs, cp)
	return len(p), nil
}

func (s *slogStubPTY) Resize(rows, cols uint16) error {
	s.resizes = append(s.resizes, [2]uint16{rows, cols})
	return nil
}

func (s *slogStubPTY) Signal(sig string) error {
	s.signals = append(s.signals, sig)
	return nil
}

func TestSessionHubForwardsFramesToPTY(t *testing.T) {
	tmp := slogIsolateEnv(t)
	now := sessClock()
	sess, err := NewSession(sessFixtureID, sessFixtureWS, sessFixtureRun,
		KindInteractive, "exec-1", "linux", "amd64", "client-a", now)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	stub := &slogStubPTY{}
	hub, err := NewHub(filepath.Join(tmp, "home"), sess, stub)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	defer hub.Close()

	if _, err := hub.Input([]byte("ls\n")); err != nil {
		t.Fatalf("Input: %v", err)
	}
	if err := hub.Resize(24, 80); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := hub.SendSignal("SIGINT"); err != nil {
		t.Fatalf("SendSignal: %v", err)
	}
	if len(stub.inputs) != 1 || string(stub.inputs[0]) != "ls\n" {
		t.Fatalf("inputs = %q", stub.inputs)
	}
	if len(stub.resizes) != 1 || stub.resizes[0] != [2]uint16{24, 80} {
		t.Fatalf("resizes = %v", stub.resizes)
	}
	if len(stub.signals) != 1 || stub.signals[0] != "SIGINT" {
		t.Fatalf("signals = %v", stub.signals)
	}

	// Without a PTY, frames fail loudly instead of vanishing silently.
	hub2, err := NewHub(filepath.Join(tmp, "home2"), sess, nil)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	defer hub2.Close()
	if _, err := hub2.Input([]byte("x")); !errors.Is(err, ErrNoPTY) {
		t.Fatalf("Input without PTY: got %v, want ErrNoPTY", err)
	}
	if err := hub2.Resize(24, 80); !errors.Is(err, ErrNoPTY) {
		t.Fatalf("Resize without PTY: got %v, want ErrNoPTY", err)
	}
	if err := hub2.SendSignal("SIGINT"); !errors.Is(err, ErrNoPTY) {
		t.Fatalf("SendSignal without PTY: got %v, want ErrNoPTY", err)
	}
}

func TestSessionHubSnapshotIsolation(t *testing.T) {
	hub, now := newSlogHub(t, "client-a")
	if err := hub.RecordExit(7, now); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}
	snap := hub.Session()
	*snap.ExitCode = 99
	again := hub.Session()
	if *again.ExitCode != 7 {
		t.Fatalf("Session() aliases ExitCode: got %d, want 7", *again.ExitCode)
	}
}
