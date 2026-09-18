package foreman

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestControlStopDoesNotWaitForActiveTurnStateLock(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &Session{
		store:  NewStore(t.TempDir(), "locked-turn"),
		state:  State{ID: "locked-turn", Status: StatusWorking},
		stopCh: make(chan string, 1),
	}
	cancelled := make(chan struct{})
	finished := make(chan struct{})

	// Model Apply holding this lock for a long-running turn. Stop cancellation
	// must happen before the watcher needs the lock to persist terminal state.
	s.mu.Lock()
	go func() {
		s.watchControlLifetime(context.Background(), func() { close(cancelled) }, ln, make(chan struct{}), time.Time{}, "")
		close(finished)
	}()
	// Let at least one health tick pass first. The regression was the tick itself
	// trying to take s.mu and blocking the watcher before stop could be selected.
	time.Sleep(200 * time.Millisecond)
	s.requestStop("test stop")
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		s.mu.Unlock()
		t.Fatal("stop cancellation waited behind the active turn state lock")
	}
	s.mu.Unlock()
	select {
	case <-finished:
	case <-time.After(3 * time.Second):
		t.Fatal("control watcher did not finish after the turn released its lock")
	}
}

func TestTellReachesSessionOverUnixSocket(t *testing.T) {
	dir := t.TempDir()
	r := &stubRunner{out: "ack"}
	s, err := Start(context.Background(), Options{
		ID:     "sock",
		Goal:   "socket test",
		Agent:  "stub",
		Root:   dir,
		Runner: r,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	errc := make(chan error, 1)
	go func() { errc <- s.ServeControl(ctx, ready) }()
	select {
	case <-ready:
	case err := <-errc:
		t.Fatalf("ServeControl: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("control socket did not become ready")
	}

	if _, err := Tell(dir, "sock", "steer over socket"); err != nil {
		t.Fatalf("Tell: %v", err)
	}

	// THE ACK MEANS "ACCEPTED", NOT "FINISHED", and that is deliberate.
	//
	// A turn runs an LLM: it takes minutes. The ack has a 3-second deadline. The
	// old code applied the command inline and only then acked, which meant every
	// `foreman tell` against a real agent died on "i/o timeout" while the agent it
	// had just launched went on working — the command SUCCEEDED and reported
	// failure. It also blocked the listener for the whole turn, so the one moment
	// you most need to say "stop, wrong file" was the one moment the socket would
	// not take the call.
	//
	// So the outcome lands in state.json, which is where `foreman status` reads it
	// from and the only place that can honestly carry the result of a long turn.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if p := r.Prompts(); len(p) == 1 && strings.Contains(p[0], "steer over socket") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("runner prompts = %#v, want socket steering", r.Prompts())
		}
		time.Sleep(20 * time.Millisecond)
	}
	for {
		st, err := NewStore(dir, "sock").LoadState()
		if err != nil {
			t.Fatalf("LoadState: %v", err)
		}
		if st.Status == StatusIdle {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("status = %q, want idle", st.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-errc:
		if err != nil {
			t.Fatalf("ServeControl exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("control server did not stop")
	}
}

func TestServeControlHardRuntimeStopsSession(t *testing.T) {
	dir := t.TempDir()
	s, err := Start(context.Background(), Options{
		ID: "deadline", Goal: "bounded", Root: dir, MaxRuntime: 80 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ready := make(chan string, 1)
	errC := make(chan error, 1)
	go func() { errC <- s.ServeControl(context.Background(), ready) }()
	select {
	case <-ready:
	case err := <-errC:
		t.Fatalf("ServeControl before ready: %v", err)
	case <-time.After(time.Second):
		t.Fatal("control socket did not become ready")
	}
	select {
	case err := <-errC:
		if err != nil {
			t.Fatalf("ServeControl: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("max runtime did not stop control server")
	}
	st, err := NewStore(dir, "deadline").LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !st.Stopped || st.Status != StatusBlocked || !strings.Contains(st.StopReason, "max runtime 80ms exceeded") {
		t.Fatalf("expired state = %+v", st)
	}
}

func TestServeControlStopCommandReturns(t *testing.T) {
	dir := t.TempDir()
	s, err := Start(context.Background(), Options{
		ID: "stop", Goal: "stoppable", Root: dir, MaxRuntime: time.Minute,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ready := make(chan string, 1)
	errC := make(chan error, 1)
	go func() { errC <- s.ServeControl(context.Background(), ready) }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("control socket did not become ready")
	}
	if _, err := SendCommand(dir, "stop", Command{Verb: CommandStop}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case err := <-errC:
		if err != nil {
			t.Fatalf("ServeControl: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("stop command left control server running")
	}
}

type cancelRunner struct {
	started chan struct{}
	stopped chan struct{}
}

func (r *cancelRunner) Run(ctx context.Context, _ string, _ []string, _ string) (string, int, error) {
	close(r.started)
	<-ctx.Done()
	close(r.stopped)
	return "", 1, ctx.Err()
}

func TestServeControlStopCancelsActiveTurn(t *testing.T) {
	dir := t.TempDir()
	r := &cancelRunner{started: make(chan struct{}), stopped: make(chan struct{})}
	s, err := Start(context.Background(), Options{
		ID: "active-stop", Goal: "stoppable", Agent: "stub", Root: dir,
		MaxRuntime: time.Minute, Runner: r,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ready := make(chan string, 1)
	errC := make(chan error, 1)
	go func() { errC <- s.ServeControl(context.Background(), ready) }()
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("control socket did not become ready")
	}
	if _, err := SendCommand(dir, "active-stop", Command{Verb: CommandTell, Message: "work"}); err != nil {
		t.Fatalf("tell: %v", err)
	}
	select {
	case <-r.started:
	case <-time.After(time.Second):
		t.Fatal("turn did not start")
	}
	if _, err := SendCommand(dir, "active-stop", Command{Verb: CommandStop}); err != nil {
		t.Fatalf("stop: %v", err)
	}
	select {
	case <-r.stopped:
	case <-time.After(time.Second):
		t.Fatal("stop did not cancel active turn")
	}
	select {
	case err := <-errC:
		if err != nil {
			t.Fatalf("ServeControl: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control server did not stop")
	}
	st, err := NewStore(dir, "active-stop").LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !st.Stopped || st.Status != StatusDone || st.StopReason != "stopped by operator" {
		t.Fatalf("stopped state = %+v", st)
	}
}

func TestServeControlDeadlineCancelsActiveTurn(t *testing.T) {
	dir := t.TempDir()
	r := &cancelRunner{started: make(chan struct{}), stopped: make(chan struct{})}
	// This test covers expiration of an active turn. Starting an 80 ms
	// session lifetime before socket/agent startup can correctly expire before
	// the runner enters, which tests startup latency instead of cancellation.
	s, err := Start(context.Background(), Options{
		ID: "active-deadline", Goal: "bounded", Agent: "stub", Root: dir, Runner: r,
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	errC := make(chan error, 1)
	serveDone := make(chan struct{})
	var watched <-chan struct{}
	go func() { defer close(serveDone); errC <- s.ServeControl(ctx, ready) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-serveDone:
		case <-time.After(5 * time.Second):
			t.Error("control server leaked after test cleanup")
		}
		if watched != nil {
			select {
			case <-watched:
			case <-time.After(5 * time.Second):
				t.Error("deadline watcher leaked after test cleanup")
			}
		}
	})
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("control socket did not become ready")
	}
	if _, err := SendCommand(dir, "active-deadline", Command{Verb: CommandTell, Message: "work"}); err != nil {
		t.Fatalf("tell: %v", err)
	}
	select {
	case <-r.started:
	case <-time.After(5 * time.Second):
		t.Fatal("turn did not start")
	}
	select {
	case <-r.stopped:
		t.Fatal("runner stopped before deadline was armed")
	default:
	}
	// Arm the production lifetime watcher only after the real runner is active.
	// It shares the server's cancellation lifetime, so removing its cancellation
	// or failing to join the accepted turn still makes this test fail. Its own
	// listener is isolated from ServeControl's listener, which must close itself.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	watchExited := make(chan struct{})
	watched = watchExited
	go func() {
		defer close(watchExited)
		s.watchControlLifetime(ctx, cancel, ln, make(chan struct{}), time.Now().Add(80*time.Millisecond), "80ms")
	}()
	select {
	case <-r.stopped:
	case <-time.After(time.Second):
		t.Fatal("deadline did not cancel active turn")
	}
	select {
	case <-watched:
	case <-time.After(time.Second):
		t.Fatal("deadline watcher did not persist stopped state")
	}
	select {
	case err := <-errC:
		if err != nil {
			t.Fatalf("ServeControl: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("control server did not stop")
	}
	st, err := NewStore(dir, "active-deadline").LoadState()
	if err != nil {
		t.Fatalf("LoadState: %v", err)
	}
	if !st.Stopped || st.Status != StatusBlocked || st.StopReason != "max runtime 80ms exceeded" {
		t.Fatalf("expired state = %+v", st)
	}
}

// A runner may need time to finish its own cleanup after cancellation. Returning
// from ServeControl must join that work and the subsequent state persistence.
type cleanupRunner struct {
	started   chan struct{}
	cancelled chan struct{}
	release   chan struct{}
}

func (r *cleanupRunner) Run(ctx context.Context, _ string, _ []string, _ string) (string, int, error) {
	close(r.started)
	<-ctx.Done()
	close(r.cancelled)
	<-r.release
	return "", 1, ctx.Err()
}

func TestServeControlJoinsCancelledTurn(t *testing.T) {
	if !ControlSupported() {
		t.Skip("Unix control sockets are unsupported")
	}
	r := &cleanupRunner{started: make(chan struct{}), cancelled: make(chan struct{}), release: make(chan struct{})}
	s, err := Start(context.Background(), Options{
		ID: "join-turn", Goal: "finish cleanup", Agent: "stub", Root: t.TempDir(), Runner: r,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = s.ServeControl(ctx, ready)
		close(done)
	}()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(r.release) }) }
	t.Cleanup(func() {
		cancel()
		release()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("control server cleanup did not finish")
		}
	})
	select {
	case <-ready:
	case <-done:
		t.Fatalf("ServeControl before ready: %v", serveErr)
	case <-time.After(3 * time.Second):
		t.Fatal("control socket did not become ready")
	}
	if _, err := SendCommand(s.store.Root, s.store.ID, Command{Verb: CommandTell, Message: "work"}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.started:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not start")
	}
	cancel()
	select {
	case <-r.cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("turn did not receive cancellation")
	}
	select {
	case <-done:
		t.Error("ServeControl returned while its turn still owned session state")
	case <-time.After(100 * time.Millisecond):
	}
	release()
	select {
	case <-done:
		if serveErr != nil {
			t.Fatalf("ServeControl: %v", serveErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("control server did not join completed turn")
	}
}

func TestServeControlClosesIdleConnections(t *testing.T) {
	if !ControlSupported() {
		t.Skip("Unix control sockets are unsupported")
	}
	s, err := Start(context.Background(), Options{ID: "idle-conn", Goal: "close connections", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan string, 1)
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = s.ServeControl(ctx, ready)
		close(done)
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("control server cleanup did not finish")
		}
	})
	var path string
	select {
	case path = <-ready:
	case <-done:
		t.Fatalf("ServeControl before ready: %v", serveErr)
	case <-time.After(3 * time.Second):
		t.Fatal("control socket did not become ready")
	}
	conn, err := net.Dial("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// A complete exchange proves this socket was accepted, then leaves its
	// handler blocked waiting for another command on the same connection.
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("invalid-json\n")); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	if _, err := reader.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	select {
	case <-done:
		if serveErr != nil {
			t.Fatal(serveErr)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("control server did not stop")
	}
	if err := conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	if _, err := reader.ReadByte(); err == nil {
		t.Fatal("idle connection still readable after shutdown")
	} else if ne, ok := err.(net.Error); ok && ne.Timeout() {
		t.Fatal("idle connection outlived ServeControl")
	}
}

func TestApplyCancelledCommandPreservesTerminalState(t *testing.T) {
	s, err := Start(context.Background(), Options{ID: "cancelled-command", Goal: "stay stopped", Root: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	s.markStopped(StatusDone, "stopped by operator")
	before := s.State()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.Apply(ctx, Command{Verb: CommandTell, Message: "queued before shutdown"}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Apply = %v, want context cancellation", err)
	}
	if after := s.State(); CanonicalDigest(after) != CanonicalDigest(before) {
		t.Fatalf("cancelled command changed terminal state: before %+v, after %+v", before, after)
	}
}

func TestControlStopDoesNotWaitForUnreadAck(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close()
	s := &Session{stopCh: make(chan string, 1)}
	var workers sync.WaitGroup
	workers.Add(1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer workers.Done()
		s.handleControlConn(context.Background(), server, &workers)
	}()
	t.Cleanup(func() {
		_ = client.Close()
		<-done
	})
	if err := client.SetWriteDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Write([]byte("{\"verb\":\"stop\"}\n")); err != nil {
		t.Fatal(err)
	}
	// net.Pipe has no write buffer: the server's ACK cannot finish while the
	// client intentionally never reads, so only its write bound releases stop.
	select {
	case reason := <-s.stopCh:
		if reason != "stopped by operator" {
			t.Fatalf("stop reason = %q", reason)
		}
	case <-time.After(time.Second):
		t.Fatal("unread ACK prevented stop cancellation")
	}
	<-done
}
