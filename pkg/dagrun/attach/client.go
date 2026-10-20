// Reconnecting attach client: dials the attach endpoint, streams output,
// and redials from the last received byte offset after a dropped connection.

package attach

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qiangli/yoke/pkg/dagrun"
)

// ErrClosed is returned by Stream operations on a closed stream.
var ErrClosed = errors.New("attach: stream closed")

type clientConfig struct {
	viewer bool
}

// Option tunes Attach.
type Option func(*clientConfig)

// Viewer attaches read-only: the stream receives output but Write fails
// locally and the server ignores input frames from viewers.
func Viewer() Option {
	return func(c *clientConfig) { c.viewer = true }
}

// Attach dials endpoint (the ws:// or wss:// attach URL) as clientID,
// replaying from byte offset since. Takeover is the server default for
// non-viewers.
func Attach(ctx context.Context, endpoint, clientID string, since int64, opts ...Option) (*Stream, error) {
	var cfg clientConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	s := &Stream{
		endpoint: endpoint,
		clientID: clientID,
		viewer:   cfg.viewer,
		next:     since,
		waitCh:   make(chan struct{}),
	}
	if err := s.dial(ctx, since); err != nil {
		return nil, err
	}
	return s, nil
}

// Stream is one client's live view of a session. It implements io.Reader
// over the output bytes: Read returns io.EOF after a recorded exit drains,
// dagrun.ErrSessionLost after executor loss drains, and dagrun.ErrEvicted
// after a takeover. Wait reports the terminal outcome; Reconnect redials
// from the last received byte offset so no byte is duplicated or lost
// across a dropped connection. It is safe for concurrent use.
type Stream struct {
	mu       sync.Mutex
	cond     sync.Cond
	endpoint string
	clientID string
	viewer   bool

	conn   *websocket.Conn
	gen    int
	next   int64 // byte offset to resume from: since + bytes received.
	closed bool

	pending []byte // output bytes received but not yet read.
	connErr error  // transport died; cleared by Reconnect.

	exited     bool
	exitCode   int
	lost       bool
	evicted    bool
	waitCh     chan struct{}
	waitClosed bool
	waitErr    error
	lastErr    string // last {"type":"error"} frame from the server.
}

func (s *Stream) condInit() {
	if s.cond.L == nil {
		s.cond.L = &s.mu
	}
}

// dialURL builds the endpoint query for a resume from offset. The endpoint,
// client id and viewer flag are immutable after Attach, so this needs no lock.
func (s *Stream) dialURL(since int64) (string, error) {
	u, err := url.Parse(s.endpoint)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("client", s.clientID)
	q.Set("since", strconv.FormatInt(since, 10))
	if s.viewer {
		q.Set("viewer", "1")
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (s *Stream) dial(ctx context.Context, since int64) error {
	raw, err := s.dialURL(since)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, raw, nil)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.condInit()
	s.conn = ws
	s.gen++
	s.connErr = nil
	go s.readLoop(ws, s.gen)
	return nil
}

// Reconnect redials from the last received byte offset. Buffered bytes that
// arrived before the drop stay buffered, so the stream neither duplicates
// nor loses a byte. It fails on a terminated (exited, lost, evicted) or
// closed stream.
func (s *Stream) Reconnect(ctx context.Context) error {
	s.mu.Lock()
	s.condInit()
	if s.waitClosed {
		s.mu.Unlock()
		return errors.New("attach: stream terminated")
	}
	if s.closed {
		s.mu.Unlock()
		return ErrClosed
	}
	since := s.next
	old := s.conn
	s.conn = nil
	s.connErr = nil
	s.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	target, err := s.dialURL(since)
	if err != nil {
		return err
	}
	dialer := websocket.Dialer{HandshakeTimeout: 10 * time.Second}
	ws, _, err := dialer.DialContext(ctx, target, nil)
	if err != nil {
		s.mu.Lock()
		s.connErr = err
		s.cond.Broadcast()
		s.mu.Unlock()
		return err
	}
	s.mu.Lock()
	s.conn = ws
	s.gen++
	gen := s.gen
	s.mu.Unlock()
	// Spawn outside s.mu: readLoop takes it on arrival.
	go s.readLoop(ws, gen)
	return nil
}

// Close ends the stream and unblocks Read and Wait.
func (s *Stream) Close() error {
	s.mu.Lock()
	s.condInit()
	if !s.closed {
		s.closed = true
		if !s.waitClosed {
			if s.waitErr == nil && !s.exited && !s.lost && !s.evicted {
				s.waitErr = ErrClosed
			}
			close(s.waitCh)
			s.waitClosed = true
		}
		if s.conn != nil {
			_ = s.conn.Close()
		}
		s.cond.Broadcast()
	}
	s.mu.Unlock()
	return nil
}

// Read serves output bytes in log order.
func (s *Stream) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.condInit()
	for {
		if len(s.pending) > 0 {
			n := copy(p, s.pending)
			s.pending = s.pending[n:]
			return n, nil
		}
		if s.evicted {
			return 0, dagrun.ErrEvicted
		}
		if s.exited {
			return 0, io.EOF
		}
		if s.lost {
			return 0, dagrun.ErrSessionLost
		}
		if s.closed {
			return 0, ErrClosed
		}
		if s.connErr != nil {
			return 0, s.connErr
		}
		s.cond.Wait()
	}
}

// Write sends PTY input. Viewers are read-only and fail locally.
func (s *Stream) Write(p []byte) error {
	s.mu.Lock()
	if s.viewer {
		s.mu.Unlock()
		return errors.New("attach: viewer is read-only")
	}
	ws := s.conn
	s.mu.Unlock()
	if ws == nil {
		return errors.New("attach: not connected")
	}
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if err := ws.WriteMessage(websocket.BinaryMessage, p); err != nil {
		return err
	}
	return nil
}

// Resize forwards terminal geometry. The frame shape matches webterm's.
func (s *Stream) Resize(cols, rows uint16) error {
	return s.writeJSON(clientControl{Type: frameResize, Cols: cols, Rows: rows})
}

// Signal delivers a named signal such as INT, TERM or HUP.
func (s *Stream) Signal(sig string) error {
	if sig == "" {
		return errors.New("attach: empty signal")
	}
	return s.writeJSON(clientControl{Type: frameSignal, Signal: sig})
}

func (s *Stream) writeJSON(m clientControl) error {
	raw, err := json.Marshal(m)
	if err != nil {
		return err
	}
	s.mu.Lock()
	ws := s.conn
	s.mu.Unlock()
	if ws == nil {
		return errors.New("attach: not connected")
	}
	_ = ws.SetWriteDeadline(time.Now().Add(10 * time.Second))
	return ws.WriteMessage(websocket.TextMessage, raw)
}

// Wait blocks until the session exits, is lost, or the stream ends. It
// returns the exit code for an exit, lost=true for executor loss (which
// carries no exit code), and a non-nil error only when the stream ends
// without a terminal frame (eviction, close, transport failure).
func (s *Stream) Wait() (int, bool, error) {
	<-s.waitCh
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.lost:
		return 0, true, nil
	case s.exited:
		return s.exitCode, false, nil
	case s.evicted:
		return 0, false, dagrun.ErrEvicted
	default:
		if s.waitErr != nil {
			return 0, false, s.waitErr
		}
		return 0, false, io.EOF
	}
}

// LastError returns the last error control frame the server sent, if any.
func (s *Stream) LastError() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// Offset returns the byte offset a Reconnect would resume from.
func (s *Stream) Offset() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.next
}

// finishTerminal records a terminal frame under lock and wakes Wait/Read.
// Callers hold s.mu.
func (s *Stream) finishTerminal(code int, lost, evicted bool) {
	if s.waitClosed {
		return
	}
	s.exitCode = code
	s.lost = lost
	s.evicted = evicted
	s.exited = !lost && !evicted
	if evicted && s.waitErr == nil {
		s.waitErr = dagrun.ErrEvicted
	}
	close(s.waitCh)
	s.waitClosed = true
	s.cond.Broadcast()
}

// readLoop demultiplexes one connection: binary frames append to the output
// buffer (advancing the resume offset); control frames end the stream.
func (s *Stream) readLoop(ws *websocket.Conn, gen int) {
	current := func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return gen == s.gen && s.conn == ws
	}
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		// Only a stale loop leaves quietly; the live loop reports the
		// transport break so Read fails fast until Reconnect redials.
		if gen == s.gen && s.conn == ws {
			s.conn = nil
			if !(s.exited || s.lost || s.evicted || s.closed) && s.connErr == nil {
				s.connErr = errors.New("attach: connection lost")
			}
			s.cond.Broadcast()
		}
	}()
	for {
		if !current() {
			return
		}
		typ, msg, err := ws.ReadMessage()
		if err != nil {
			return
		}
		s.mu.Lock()
		if gen != s.gen || s.conn != ws {
			s.mu.Unlock()
			return
		}
		if typ == websocket.BinaryMessage {
			s.pending = append(s.pending, msg...)
			s.next += int64(len(msg))
			s.cond.Broadcast()
			s.mu.Unlock()
			continue
		}
		if typ != websocket.TextMessage {
			s.mu.Unlock()
			continue
		}
		var m serverControl
		if err := json.Unmarshal(msg, &m); err != nil {
			s.mu.Unlock()
			continue
		}
		switch m.Type {
		case frameExit:
			code := 0
			if m.ExitCode != nil {
				code = *m.ExitCode
			}
			s.finishTerminal(code, false, false)
		case frameLost:
			s.finishTerminal(0, true, false)
		case frameEvicted:
			s.finishTerminal(0, false, true)
		case frameError:
			s.lastErr = m.Error
		case frameOffset:
			// Informational: ordering on one connection already
			// guarantees the byte count matches.
		}
		done := s.exited || s.lost || s.evicted
		s.mu.Unlock()
		if done {
			return
		}
	}
}
