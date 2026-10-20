package dagrun

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Files inside a workspace home directory owned by a Hub.
const (
	// logFileName is the append-only session output log.
	logFileName = "output.log"
	// recordFileName is the persisted session record beside the log.
	recordFileName = "session.json"
)

var (
	// ErrBadOffset is returned when a read or attach offset is negative or
	// past the end of the log.
	ErrBadOffset = errors.New("dagrun: bad byte offset")
	// ErrEvicted is returned by an attachment's Read after a different
	// client took over the session. Undelivered bytes are discarded; a new
	// attach with since=<offset> replays them from the log.
	ErrEvicted = errors.New("dagrun: attachment evicted by takeover")
	// ErrNoPTY is returned when input/resize/signal frames have nowhere to
	// go because the Hub was built without a PTY.
	ErrNoPTY = errors.New("dagrun: no pty attached")
	// ErrSessionClosed is returned when output is appended or a client
	// attaches after the session exited or was lost.
	ErrSessionClosed = errors.New("dagrun: session is closed")
	// ErrSessionLost is returned by an attachment's Read, after draining
	// surviving bytes, when the executor was lost. It is distinct from
	// io.EOF, which marks a recorded exit.
	ErrSessionLost = errors.New("dagrun: session lost")
	// ErrAttachmentClosed is returned by Read on an attachment that was
	// already closed.
	ErrAttachmentClosed = errors.New("dagrun: attachment closed")
	// ErrHubClosed is returned for operations on a closed Hub.
	ErrHubClosed = errors.New("dagrun: hub closed")
)

// SessionLog is an append-only byte log backed by one file in a caller-given
// directory (the workspace home). Offsets are byte counts from the start of
// the log; Append returns the offset its bytes start at, and ReadFrom replays
// from any earlier offset. It is safe for concurrent use.
type SessionLog struct {
	mu   sync.Mutex
	f    *os.File
	size int64
}

// OpenSessionLog opens (creating) the log in dir. Reopening an existing
// directory resumes at the previous size, so offsets stay stable.
func OpenSessionLog(dir string) (*SessionLog, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("dagrun: create log dir: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, logFileName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, fmt.Errorf("dagrun: open session log: %w", err)
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("dagrun: stat session log: %w", err)
	}
	return &SessionLog{f: f, size: st.Size()}, nil
}

// Append writes p at the end of the log and returns the byte offset p starts
// at. An empty append returns the current size without writing.
func (l *SessionLog) Append(p []byte) (int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return 0, fmt.Errorf("dagrun: append to closed session log")
	}
	off := l.size
	for len(p) > 0 {
		n, err := l.f.Write(p)
		l.size += int64(n)
		if err != nil {
			return off, fmt.Errorf("dagrun: append session log: %w", err)
		}
		p = p[n:]
	}
	return off, nil
}

// ReadFrom returns every byte from since to the end of the log plus the next
// offset to read from (the current size). It rejects out-of-range offsets.
func (l *SessionLog) ReadFrom(since int64) ([]byte, int64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil, 0, fmt.Errorf("dagrun: read closed session log")
	}
	if since < 0 || since > l.size {
		return nil, l.size, fmt.Errorf("%w: since=%d size=%d", ErrBadOffset, since, l.size)
	}
	buf := make([]byte, l.size-since)
	if len(buf) > 0 {
		if _, err := l.f.ReadAt(buf, since); err != nil {
			return nil, l.size, fmt.Errorf("dagrun: read session log: %w", err)
		}
	}
	return buf, l.size, nil
}

// Size returns the current end offset of the log.
func (l *SessionLog) Size() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.size
}

// Sync flushes buffered output to durable storage. The Hub calls it when an
// exit is recorded, so the bytes behind a reported exit code survive.
func (l *SessionLog) Sync() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return fmt.Errorf("dagrun: sync closed session log")
	}
	return l.f.Sync()
}

// Close closes the log file.
func (l *SessionLog) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// PTY is the minimal executor-side terminal control surface a Hub forwards
// input frames to. Resize/signal/input frames arriving from attached clients
// are forwarded here; output flows the other way, through the session log.
// It mirrors the control operations agentpty performs on its PTY without
// importing that package: pkg/dagrun stays dependency-free.
type PTY interface {
	// WriteInput delivers client keystrokes to the session's stdin.
	WriteInput(p []byte) (int, error)
	// Resize forwards a terminal geometry change.
	Resize(rows, cols uint16) error
	// Signal delivers a named signal such as SIGINT or SIGTERM.
	Signal(sig string) error
}

type attachConfig struct {
	readOnly bool
}

// AttachOption tunes Hub.Attach.
type AttachOption func(*attachConfig)

// ReadOnly attaches a viewer: it replays and streams output but never holds
// the session and never evicts (or gets evicted by) takeovers.
func ReadOnly() AttachOption {
	return func(c *attachConfig) { c.readOnly = true }
}

// Hub owns one session's record, its append-only log, and the live
// attachments streaming it. Attach replays from a byte offset then streams
// live appends; a second non-viewer attach by a different client takes over
// and the previous holder's stream fails with ErrEvicted. It is safe for
// concurrent use. Lock order is always Hub -> attachment -> log.
type Hub struct {
	mu      sync.Mutex
	dir     string
	log     *SessionLog
	pty     PTY
	session Session
	holder  *Attachment
	viewers map[*Attachment]struct{}
	closed  bool
}

// NewHub opens the session home in dir (a caller-given workspace home
// directory) for sess. pty may be nil, in which case input frames fail with
// ErrNoPTY. When dir already holds a persisted record for the same session
// id, the stored record (with its exit or loss) wins over sess, so reopening
// a workspace home resumes the recorded outcome.
func NewHub(dir string, sess Session, pty PTY) (*Hub, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("dagrun: create session home: %w", err)
	}
	if raw, err := os.ReadFile(filepath.Join(dir, recordFileName)); err == nil {
		var stored Session
		if err := json.Unmarshal(raw, &stored); err != nil {
			return nil, fmt.Errorf("dagrun: decode session record: %w", err)
		}
		if stored.ID != sess.ID {
			return nil, fmt.Errorf("dagrun: session id mismatch: stored %q, got %q", stored.ID, sess.ID)
		}
		sess = stored
	} else if !os.IsNotExist(err) {
		return nil, fmt.Errorf("dagrun: read session record: %w", err)
	}
	log, err := OpenSessionLog(dir)
	if err != nil {
		return nil, err
	}
	h := &Hub{
		dir:     dir,
		log:     log,
		pty:     pty,
		session: copySession(sess),
		viewers: make(map[*Attachment]struct{}),
	}
	if err := h.saveLocked(); err != nil {
		_ = log.Close()
		return nil, err
	}
	return h, nil
}

// saveLocked persists the session record beside the log. Callers hold h.mu.
func (h *Hub) saveLocked() error {
	raw, err := json.Marshal(h.session)
	if err != nil {
		return fmt.Errorf("dagrun: encode session record: %w", err)
	}
	tmp := filepath.Join(h.dir, recordFileName+".tmp")
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return fmt.Errorf("dagrun: write session record: %w", err)
	}
	if err := os.Rename(tmp, filepath.Join(h.dir, recordFileName)); err != nil {
		return fmt.Errorf("dagrun: persist session record: %w", err)
	}
	return nil
}

// Session returns a snapshot copy of the session record. The exit-code
// pointer is deep-copied, so mutating the snapshot cannot affect the Hub.
func (h *Hub) Session() Session {
	h.mu.Lock()
	defer h.mu.Unlock()
	return copySession(h.session)
}

// liveLocked collects the attachments currently streaming. Callers hold h.mu;
// pushes happen after h.mu is released, so a slow reader never blocks the Hub.
func (h *Hub) liveLocked() []*Attachment {
	out := make([]*Attachment, 0, len(h.viewers)+1)
	if h.holder != nil {
		out = append(out, h.holder)
	}
	for a := range h.viewers {
		out = append(out, a)
	}
	return out
}

// Append appends output to the log and streams it to live attachments,
// returning the offset the bytes start at. It fails on exited/lost sessions.
func (h *Hub) Append(p []byte) (int64, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return 0, ErrHubClosed
	}
	if h.session.State.Terminal() {
		h.mu.Unlock()
		return 0, fmt.Errorf("%w: cannot append to %s session", ErrSessionClosed, h.session.State)
	}
	off, err := h.log.Append(p)
	live := h.liveLocked()
	h.mu.Unlock()
	if err != nil {
		return off, err
	}
	for _, a := range live {
		a.push(p)
	}
	return off, nil
}

// ReadFrom replays the log from a byte offset. See SessionLog.ReadFrom.
func (h *Hub) ReadFrom(since int64) ([]byte, int64, error) {
	return h.log.ReadFrom(since)
}

// Attach subscribes clientID at byte offset since: the returned Attachment
// first replays the log from since, then streams live appends. A non-viewer
// attach by a different client than the current holder takes over: the
// previous holder's Read fails with ErrEvicted. Read-only viewers never take
// over and are never evicted by takeovers. Attaching to an exited or lost
// session fails.
func (h *Hub) Attach(clientID string, since int64, now time.Time, opts ...AttachOption) (*Attachment, error) {
	var cfg attachConfig
	for _, opt := range opts {
		opt(&cfg)
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, ErrHubClosed
	}
	if clientID == "" {
		h.mu.Unlock()
		return nil, ErrEmptyClientID
	}
	size := h.log.Size()
	if since < 0 || since > size {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w: since=%d size=%d", ErrBadOffset, since, size)
	}
	if h.session.State.Terminal() {
		h.mu.Unlock()
		return nil, fmt.Errorf("%w: cannot attach to %s session", ErrSessionClosed, h.session.State)
	}
	replay, _, err := h.log.ReadFrom(since)
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	a := newAttachment(h, clientID, cfg.readOnly, replay)
	var evict *Attachment
	if cfg.readOnly {
		h.viewers[a] = struct{}{}
	} else {
		next, err := h.session.Attach(clientID, now)
		if err != nil {
			h.mu.Unlock()
			return nil, err
		}
		h.session = next
		if h.holder != nil {
			evict = h.holder
		}
		h.holder = a
		if err := h.saveLocked(); err != nil {
			h.mu.Unlock()
			return nil, err
		}
	}
	h.mu.Unlock()
	if evict != nil {
		evict.evict()
	}
	return a, nil
}

// Detach records a client disconnect and drops its live stream. The session
// keeps running holderless (detached).
func (h *Hub) Detach(clientID string, now time.Time) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrHubClosed
	}
	next, err := h.session.Detach(clientID, now)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	h.session = next
	var drop *Attachment
	if h.holder != nil && h.holder.clientID == clientID {
		drop = h.holder
		h.holder = nil
	}
	if err := h.saveLocked(); err != nil {
		h.mu.Unlock()
		return err
	}
	h.mu.Unlock()
	if drop != nil {
		drop.shutdown()
	}
	return nil
}

// RecordExit records the exit code exactly once, fsyncs the log so the bytes
// behind the code are durable, and ends live streams (readers drain, then see
// io.EOF). A second exit fails with ErrSecondExit.
func (h *Hub) RecordExit(code int, now time.Time) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrHubClosed
	}
	prev := h.session
	next, err := prev.RecordExit(code, now)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	h.session = next
	if err := h.saveLocked(); err != nil {
		h.session = prev
		h.mu.Unlock()
		return err
	}
	live := h.liveLocked()
	h.mu.Unlock()
	if err := h.log.Sync(); err != nil {
		return fmt.Errorf("dagrun: fsync session log on exit: %w", err)
	}
	for _, a := range live {
		a.finish(false)
	}
	return nil
}

// MarkLost records executor loss and ends live streams (readers drain
// surviving bytes, then see ErrSessionLost). The session never exits after.
func (h *Hub) MarkLost(now time.Time) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return ErrHubClosed
	}
	prev := h.session
	next, err := prev.MarkLost(now)
	if err != nil {
		h.mu.Unlock()
		return err
	}
	h.session = next
	if err := h.saveLocked(); err != nil {
		h.session = prev
		h.mu.Unlock()
		return err
	}
	live := h.liveLocked()
	h.mu.Unlock()
	for _, a := range live {
		a.finish(true)
	}
	return nil
}

// Input forwards client keystrokes to the session's stdin.
func (h *Hub) Input(p []byte) (int, error) {
	h.mu.Lock()
	pty := h.pty
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return 0, ErrHubClosed
	}
	if pty == nil {
		return 0, ErrNoPTY
	}
	return pty.WriteInput(p)
}

// Resize forwards a terminal geometry change to the session's PTY.
func (h *Hub) Resize(rows, cols uint16) error {
	h.mu.Lock()
	pty := h.pty
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return ErrHubClosed
	}
	if pty == nil {
		return ErrNoPTY
	}
	return pty.Resize(rows, cols)
}

// SendSignal delivers a named signal to the session's process group.
func (h *Hub) SendSignal(sig string) error {
	h.mu.Lock()
	pty := h.pty
	closed := h.closed
	h.mu.Unlock()
	if closed {
		return ErrHubClosed
	}
	if pty == nil {
		return ErrNoPTY
	}
	return pty.Signal(sig)
}

// remove detaches a from the live set without touching session state:
// closing a stream never changes the record; only Detach does.
func (h *Hub) remove(a *Attachment) {
	h.mu.Lock()
	if h.holder == a {
		h.holder = nil
	}
	delete(h.viewers, a)
	h.mu.Unlock()
	a.shutdown()
}

// Close shuts the Hub down: live streams end and the log closes. The
// persisted record and log bytes stay on disk for reopening.
func (h *Hub) Close() error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	live := h.liveLocked()
	h.holder = nil
	h.viewers = make(map[*Attachment]struct{})
	h.mu.Unlock()
	for _, a := range live {
		a.shutdown()
	}
	return h.log.Close()
}

// Attachment is one client's live view of a session: first the replayed log
// bytes from the attach offset, then streamed live appends. It implements
// io.Reader. Read returns ErrEvicted after a takeover, ErrSessionLost after
// executor loss once surviving bytes drain, and io.EOF after a recorded exit
// once bytes drain. Use Close to stop streaming; closing never changes the
// session record.
type Attachment struct {
	clientID string
	readOnly bool
	hub      *Hub

	mu      sync.Mutex
	cond    sync.Cond
	pending []byte
	evicted bool
	closed  bool
	ended   bool
	lost    bool
}

func newAttachment(hub *Hub, clientID string, readOnly bool, replay []byte) *Attachment {
	a := &Attachment{
		clientID: clientID,
		readOnly: readOnly,
		hub:      hub,
		pending:  replay,
	}
	a.cond.L = &a.mu
	return a
}

// ClientID returns the attaching client's id.
func (a *Attachment) ClientID() string { return a.clientID }

// ReadOnly reports whether this attachment is a viewer.
func (a *Attachment) ReadOnly() bool { return a.readOnly }

// push queues live bytes for the reader.
func (a *Attachment) push(p []byte) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.evicted || a.closed || a.ended || len(p) == 0 {
		return
	}
	a.pending = append(a.pending, p...)
	a.cond.Broadcast()
}

// evict marks the attachment taken over: the next Read fails with
// ErrEvicted, discarding undelivered bytes.
func (a *Attachment) evict() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.evicted = true
	a.cond.Broadcast()
}

// finish ends the live stream: readers drain, then see io.EOF on exit or
// ErrSessionLost on executor loss.
func (a *Attachment) finish(lost bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.ended {
		return
	}
	a.ended = true
	a.lost = lost
	a.cond.Broadcast()
}

// shutdown closes the attachment locally without touching session state.
func (a *Attachment) shutdown() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closed = true
	a.cond.Broadcast()
}

// Read serves replayed bytes first, then live appends, blocking until bytes
// arrive or the stream ends.
func (a *Attachment) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	for {
		if a.evicted {
			return 0, ErrEvicted
		}
		if len(a.pending) > 0 {
			n := copy(p, a.pending)
			a.pending = a.pending[n:]
			return n, nil
		}
		if a.closed {
			return 0, ErrAttachmentClosed
		}
		if a.ended {
			if a.lost {
				return 0, ErrSessionLost
			}
			return 0, io.EOF
		}
		a.cond.Wait()
	}
}

// Close stops the stream and leaves the Hub's live set. It is idempotent and
// never changes the session record: call Hub.Detach to record a disconnect.
func (a *Attachment) Close() error {
	a.hub.remove(a)
	return nil
}
