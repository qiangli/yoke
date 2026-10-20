package attach

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qiangli/yoke/pkg/dagrun"
)

// fakePTY implements dagrun.PTY without a real terminal.
type fakePTY struct {
	mu      sync.Mutex
	input   []byte
	resizes [][2]uint16
	signals []string
}

func (f *fakePTY) WriteInput(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.input = append(f.input, p...)
	return len(p), nil
}

func (f *fakePTY) Resize(rows, cols uint16) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resizes = append(f.resizes, [2]uint16{rows, cols})
	return nil
}

func (f *fakePTY) Signal(sig string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.signals = append(f.signals, sig)
	return nil
}

func (f *fakePTY) tookInput() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]byte(nil), f.input...)
}

func attachIsolateEnv(t *testing.T) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("BASHY_HOME", tmp+"/bashy-home")
	return tmp
}

func attachHub(t *testing.T, pty dagrun.PTY, holder string) (*dagrun.Hub, time.Time) {
	t.Helper()
	tmp := attachIsolateEnv(t)
	now := time.Now().Truncate(time.Millisecond)
	sess, err := dagrun.NewSession("sess-1", "ws-1", "run-1",
		dagrun.KindInteractive, "exec-1", "linux", "amd64", holder, now)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	hub, err := dagrun.NewHub(tmp+"/home", sess, pty)
	if err != nil {
		t.Fatalf("NewHub: %v", err)
	}
	t.Cleanup(func() { _ = hub.Close() })
	return hub, now
}

func attachServer(t *testing.T, hub *dagrun.Hub) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(Handler(hub))
	t.Cleanup(srv.Close)
	return srv
}

func attachWSURL(srv *httptest.Server) string {
	return "ws://" + strings.TrimPrefix(srv.URL, "http://") + "/attach"
}

func attachCtx(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 10*time.Second)
}

// readAllSMS reads until the stream ends, with a timeout per Read.
func readAllSMS(t *testing.T, r io.Reader) ([]byte, error) {
	t.Helper()
	var out []byte
	for {
		buf := make([]byte, 4096)
		type outcome struct {
			n   int
			err error
		}
		done := make(chan outcome, 1)
		go func() {
			n, err := r.Read(buf)
			done <- outcome{n, err}
		}()
		select {
		case res := <-done:
			if res.n > 0 {
				out = append(out, buf[:res.n]...)
			}
			if res.err != nil {
				return out, res.err
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("timed out reading stream")
			return out, nil
		}
	}
}

// TestAttachSecondClientReplaysAndSeesExit is the slice-2 acceptance test:
// client A attaches, output is appended, A's connection drops; client B
// attaches since=0 and receives every byte plus the exit frame carrying the
// original nonzero code.
func TestAttachSecondClientReplaysAndSeesExit(t *testing.T) {
	pty := &fakePTY{}
	hub, now := attachHub(t, pty, "")
	srv := attachServer(t, hub)
	ctx, cancel := attachCtx(t)
	defer cancel()

	a, err := Attach(ctx, attachWSURL(srv), "a", 0)
	if err != nil {
		t.Fatalf("Attach A: %v", err)
	}
	if _, err := hub.Append([]byte("hello ")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if _, err := hub.Append([]byte("world")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// A sees the live bytes, then drops.
	got, err := readNFrom(t, a, len("hello world"))
	if err != nil {
		t.Fatalf("A read: %v", err)
	}
	if string(got) != "hello world" {
		t.Fatalf("A got %q, want %q", got, "hello world")
	}
	if err := a.Close(); err != nil {
		t.Fatalf("A close: %v", err)
	}
	if err := hub.RecordExit(3, now); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}

	b, err := Attach(ctx, attachWSURL(srv), "b", 0)
	if err != nil {
		t.Fatalf("Attach B: %v", err)
	}
	defer b.Close()
	body, err := readAllSMS(t, b)
	if err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("B read: %v", err)
	}
	if string(body) != "hello world" {
		t.Fatalf("B got %q, want %q", body, "hello world")
	}
	code, lost, err := b.Wait()
	if err != nil {
		t.Fatalf("B Wait: %v", err)
	}
	if lost {
		t.Fatal("B Wait: lost=true, want exit")
	}
	if code != 3 {
		t.Fatalf("B Wait code = %d, want 3", code)
	}
}

// TestAttachExitFrameShape pins the terminal replay on the wire: replay
// bytes, then {"type":"offset"}, then {"type":"exit","exit_code"} and a
// normal close.
func TestAttachExitFrameShape(t *testing.T) {
	pty := &fakePTY{}
	hub, now := attachHub(t, pty, "")
	srv := attachServer(t, hub)
	if _, err := hub.Append([]byte("ab")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.RecordExit(3, now); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.Dial(attachWSURL(srv)+"?client=raw&since=0", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	var sawBin []byte
	var sawOffset *int64
	var sawExit *int
	deadline := time.Now().Add(10 * time.Second)
	for sawExit == nil {
		_ = ws.SetReadDeadline(deadline)
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (bin=%q off=%v)", err, sawBin, sawOffset)
		}
		if typ == websocket.BinaryMessage {
			sawBin = append(sawBin, msg...)
			continue
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(msg, &m); err != nil {
			t.Fatalf("control not JSON: %q", msg)
		}
		var ctype string
		if err := json.Unmarshal(m["type"], &ctype); err != nil {
			t.Fatalf("control without type: %q", msg)
		}
		switch ctype {
		case "offset":
			var off int64
			if err := json.Unmarshal(m["offset"], &off); err != nil {
				t.Fatalf("offset without number: %q", msg)
			}
			sawOffset = &off
		case "exit":
			var code int
			if err := json.Unmarshal(m["exit_code"], &code); err != nil {
				t.Fatalf("exit without exit_code: %q", msg)
			}
			sawExit = &code
		default:
			t.Fatalf("unexpected control frame %q", msg)
		}
	}
	if string(sawBin) != "ab" {
		t.Fatalf("binary = %q, want %q", sawBin, "ab")
	}
	if sawOffset == nil || *sawOffset != 2 {
		t.Fatalf("offset = %v, want 2", sawOffset)
	}
	if sawExit == nil || *sawExit != 3 {
		t.Fatalf("exit = %v, want 3", sawExit)
	}
	// Terminal frames end with a normal close.
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := ws.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseNormalClosure) {
		t.Fatalf("close = %v, want normal closure", err)
	}
}

// killTransport severs the stream's socket without a close frame, like a
// dropped connection, leaving the stream itself open for Reconnect.
func killTransport(s *Stream) {
	s.mu.Lock()
	ws := s.conn
	s.mu.Unlock()
	if ws != nil {
		_ = ws.UnderlyingConn().Close()
	}
}

func readNFrom(t *testing.T, r io.Reader, n int) ([]byte, error) {
	t.Helper()
	buf := make([]byte, n)
	done := make(chan error, 1)
	go func() {
		_, err := io.ReadFull(r, buf)
		done <- err
	}()
	select {
	case err := <-done:
		return buf, err
	case <-time.After(10 * time.Second):
		t.Fatalf("timed out reading %d bytes", n)
		return nil, errors.New("timeout")
	}
}

// TestAttachTakeoverEvictsPreviousHolder: a second non-viewer attach takes
// over; the previous holder sees eviction and the new holder keeps working.
func TestAttachTakeoverEvictsPreviousHolder(t *testing.T) {
	pty := &fakePTY{}
	hub, _ := attachHub(t, pty, "")
	srv := attachServer(t, hub)
	ctx, cancel := attachCtx(t)
	defer cancel()

	a, err := Attach(ctx, attachWSURL(srv), "a", 0)
	if err != nil {
		t.Fatalf("Attach A: %v", err)
	}
	defer a.Close()
	b, err := Attach(ctx, attachWSURL(srv), "b", 0)
	if err != nil {
		t.Fatalf("Attach B: %v", err)
	}
	defer b.Close()

	if _, err := readAllSMS(t, a); !errors.Is(err, dagrun.ErrEvicted) {
		t.Fatalf("A read err = %v, want ErrEvicted", err)
	}
	if _, err := hub.Append([]byte("for-b")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := readNFrom(t, b, len("for-b"))
	if err != nil {
		t.Fatalf("B read: %v", err)
	}
	if string(got) != "for-b" {
		t.Fatalf("B got %q, want %q", got, "for-b")
	}
}

// TestAttachViewerCannotWriteInput: a viewer never takes over, receives
// output, and its keystrokes never reach the PTY.
func TestAttachViewerCannotWriteInput(t *testing.T) {
	pty := &fakePTY{}
	hub, _ := attachHub(t, pty, "")
	srv := attachServer(t, hub)
	ctx, cancel := attachCtx(t)
	defer cancel()

	a, err := Attach(ctx, attachWSURL(srv), "a", 0)
	if err != nil {
		t.Fatalf("Attach A: %v", err)
	}
	defer a.Close()
	v, err := Attach(ctx, attachWSURL(srv), "v", 0, Viewer())
	if err != nil {
		t.Fatalf("Attach viewer: %v", err)
	}
	defer v.Close()

	if err := v.Write([]byte("x")); err == nil {
		t.Fatal("viewer Write succeeded, want read-only error")
	}
	if _, err := hub.Append([]byte("hi")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	// Holder survives the viewer attach and still streams.
	got, err := readNFrom(t, a, 2)
	if err != nil {
		t.Fatalf("A read: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("A got %q, want %q", got, "hi")
	}
	// Viewer sees output too.
	got, err = readNFrom(t, v, 2)
	if err != nil {
		t.Fatalf("viewer read: %v", err)
	}
	if string(got) != "hi" {
		t.Fatalf("viewer got %q, want %q", got, "hi")
	}
	if in := pty.tookInput(); len(in) != 0 {
		t.Fatalf("PTY got input %q from viewer, want none", in)
	}
}

// TestAttachViewerRawWriteGetsErrorFrame: a raw viewer binary frame is
// ignored and answered with an error control frame.
func TestAttachViewerRawWriteGetsErrorFrame(t *testing.T) {
	pty := &fakePTY{}
	hub, _ := attachHub(t, pty, "")
	srv := attachServer(t, hub)

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.Dial(attachWSURL(srv)+"?client=v&since=0&viewer=1", nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ws.Close()
	// Drain the offset frame.
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, _, err := ws.ReadMessage(); err != nil {
		t.Fatalf("read offset: %v", err)
	}
	if err := ws.WriteMessage(websocket.BinaryMessage, []byte("x")); err != nil {
		t.Fatalf("write: %v", err)
	}
	_ = ws.SetReadDeadline(time.Now().Add(10 * time.Second))
	typ, msg, err := ws.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if typ != websocket.TextMessage {
		t.Fatalf("frame type = %d, want text", typ)
	}
	var m struct {
		Type string `json:"type"`
		Err  string `json:"error"`
	}
	if err := json.Unmarshal(msg, &m); err != nil || m.Type != "error" || m.Err == "" {
		t.Fatalf("frame = %q, want {\"type\":\"error\",...}", msg)
	}
	if in := pty.tookInput(); len(in) != 0 {
		t.Fatalf("PTY got input %q from viewer, want none", in)
	}
}

// TestAttachInputResizeSignal: holder binary frames reach the PTY as input,
// and resize/signal control frames are forwarded.
func TestAttachInputResizeSignal(t *testing.T) {
	pty := &fakePTY{}
	hub, _ := attachHub(t, pty, "")
	srv := attachServer(t, hub)
	ctx, cancel := attachCtx(t)
	defer cancel()

	a, err := Attach(ctx, attachWSURL(srv), "a", 0)
	if err != nil {
		t.Fatalf("Attach A: %v", err)
	}
	defer a.Close()
	if err := a.Write([]byte("ls\n")); err != nil {
		t.Fatalf("Write: %v", err)
	}
	if err := a.Resize(80, 24); err != nil {
		t.Fatalf("Resize: %v", err)
	}
	if err := a.Signal("INT"); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		pty.mu.Lock()
		gotIn := string(pty.input)
		gotSig := len(pty.signals) > 0
		gotRs := len(pty.resizes) > 0
		pty.mu.Unlock()
		if gotIn == "ls\n" && gotSig && gotRs {
			pty.mu.Lock()
			rs := pty.resizes[0]
			pty.mu.Unlock()
			if rs[0] != 24 || rs[1] != 80 {
				t.Fatalf("resize = %v, want [24 80]", rs)
			}
			if pty.signals[0] != "INT" {
				t.Fatalf("signal = %q, want INT", pty.signals[0])
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("PTY saw no input/resize/signal within deadline")
}

// TestClientReconnectResumesFromOffsetWithoutDuplicates: after the transport
// dies mid-stream, Reconnect redials from the last received byte offset —
// every byte arrives exactly once.
func TestClientReconnectResumesFromOffsetWithoutDuplicates(t *testing.T) {
	pty := &fakePTY{}
	hub, now := attachHub(t, pty, "")
	srv := attachServer(t, hub)
	ctx, cancel := attachCtx(t)
	defer cancel()

	s, err := Attach(ctx, attachWSURL(srv), "a", 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer s.Close()
	if _, err := hub.Append([]byte("one")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	got, err := readNFrom(t, s, 3)
	if err != nil {
		t.Fatalf("read one: %v", err)
	}
	// Kill the transport without a close frame, like a dropped connection.
	killTransport(s)
	if _, err := hub.Append([]byte("two")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := s.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	got2, err := readNFrom(t, s, 3)
	if err != nil {
		t.Fatalf("read two: %v", err)
	}
	if string(got)+string(got2) != "onetwo" {
		t.Fatalf("stream = %q, want %q (duplication or loss)", string(got)+string(got2), "onetwo")
	}
	if err := hub.RecordExit(0, now); err != nil {
		t.Fatalf("RecordExit: %v", err)
	}
	code, lost, err := s.Wait()
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if lost || code != 0 {
		t.Fatalf("Wait = (%d,%v), want (0,false)", code, lost)
	}
}

// TestAttachLostIsTerminalAndDistinctFromExit: executor loss ends the stream
// with a lost frame, never an exit code.
func TestAttachLostIsTerminalAndDistinctFromExit(t *testing.T) {
	pty := &fakePTY{}
	hub, now := attachHub(t, pty, "")
	srv := attachServer(t, hub)
	ctx, cancel := attachCtx(t)
	defer cancel()

	s, err := Attach(ctx, attachWSURL(srv), "a", 0)
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer s.Close()
	if _, err := hub.Append([]byte("last")); err != nil {
		t.Fatalf("Append: %v", err)
	}
	if err := hub.MarkLost(now); err != nil {
		t.Fatalf("MarkLost: %v", err)
	}
	body, err := readAllSMS(t, s)
	if !errors.Is(err, dagrun.ErrSessionLost) {
		t.Fatalf("read err = %v (%q), want ErrSessionLost", err, body)
	}
	if string(body) != "last" {
		t.Fatalf("body = %q, want %q", body, "last")
	}
	code, lost, werr := s.Wait()
	if werr != nil {
		t.Fatalf("Wait: %v", werr)
	}
	if !lost {
		t.Fatal("Wait: lost=false, want true")
	}
	if code != 0 {
		t.Fatalf("Wait code = %d, lost sessions carry no exit code", code)
	}
}

// TestAttachRejectsCrossOrigin: a browser Origin that does not match the
// server is refused; a matching Origin is accepted.
func TestAttachRejectsCrossOrigin(t *testing.T) {
	pty := &fakePTY{}
	hub, _ := attachHub(t, pty, "")
	srv := attachServer(t, hub)
	host := strings.TrimPrefix(srv.URL, "http://")

	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	evil := http.Header{"Origin": []string{"https://evil.example"}}
	if _, _, err := dialer.Dial(attachWSURL(srv)+"?client=a&since=0", evil); err == nil {
		t.Fatal("cross-origin dial succeeded, want rejection")
	}
	good := http.Header{"Origin": []string{"http://" + host}}
	ws, _, err := dialer.Dial(attachWSURL(srv)+"?client=a&since=0", good)
	if err != nil {
		t.Fatalf("same-origin dial: %v", err)
	}
	ws.Close()
}

// TestAttachBadOffset: since past the end of the log is a 400, not an upgrade.
func TestAttachBadOffset(t *testing.T) {
	pty := &fakePTY{}
	hub, _ := attachHub(t, pty, "")
	srv := attachServer(t, hub)

	resp, err := http.Get(srv.URL + "/attach?client=a&since=99")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

// TestAttachHeadersRideEveryDial pins the Headers option: the first dial and
// a reconnect each re-run the header func, so a refreshed credential reaches
// the redial, and a header error fails the dial before the network.
func TestAttachHeadersRideEveryDial(t *testing.T) {
	hub, _ := attachHub(t, &fakePTY{}, "")
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Header.Get("X-Test-Auth"))
		mu.Unlock()
		if r.Header.Get("X-Test-Auth") == "" {
			http.Error(w, "no auth", http.StatusUnauthorized)
			return
		}
		Handler(hub).ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	ctx, cancel := attachCtx(t)
	defer cancel()

	n := 0
	headers := func() (http.Header, error) {
		n++
		h := http.Header{}
		h.Set("X-Test-Auth", "gen-"+string(rune('0'+n)))
		return h, nil
	}
	s, err := Attach(ctx, attachWSURL(srv), "a", 0, Headers(headers))
	if err != nil {
		t.Fatalf("Attach: %v", err)
	}
	defer s.Close()
	if err := s.Reconnect(ctx); err != nil {
		t.Fatalf("Reconnect: %v", err)
	}
	mu.Lock()
	got := append([]string(nil), seen...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "gen-1" || got[1] != "gen-2" {
		t.Fatalf("handshake headers = %q, want [gen-1 gen-2]", got)
	}

	boom := errors.New("no credential")
	if _, err := Attach(ctx, attachWSURL(srv), "b", 0, Headers(func() (http.Header, error) { return nil, boom })); !errors.Is(err, boom) {
		t.Fatalf("Attach with failing headers = %v, want %v", err, boom)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 {
		t.Fatalf("a failing header func reached the server: %q", seen)
	}
}
