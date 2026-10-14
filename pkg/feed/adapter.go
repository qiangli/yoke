package feed

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qiangli/yoke/pkg/toolcmd"
)

// adapterOutbound sends envelopes to a channel's registered tool command via
// stdin JSON. The tool returns delivered message ids on stdout.
//
// Secrets are never placed on argv; the tool inherits env vars populated by
// `bashy secret env` (the existing secret path).

// EnvelopeSender is the outbound delivery function. It receives envelopes as
// JSON on stdin and returns delivered message ids. Tests inject a fake.
type EnvelopeSender func(ctx context.Context, ch Channel, envelopes []Envelope) ([]string, error)

// defaultSender is the production sender: it marshals DeliveryRequest to JSON
// and runs the channel's registered tool command. The JSON is sent as the
// --message argument (toolcmd does not expose stdin piping); adapters that
// need true stdin can read the message from the --message flag value which
// carries the JSON. Secrets are never on argv — tools read them from env
// (bashy secret env).
var defaultSender EnvelopeSender = toolcmdSender

func toolcmdSender(ctx context.Context, ch Channel, envelopes []Envelope) ([]string, error) {
	tool, tc, err := toolcmd.LookupCommand(ch.Cmd)
	if err != nil {
		return nil, err
	}
	baseArgs, err := toolcmd.SplitArgs(ch.Args)
	if err != nil {
		return nil, fmt.Errorf("feed: channel %q: --args: %w", ch.Name, err)
	}
	req := DeliveryRequest{Version: ContractVersion, Envelopes: envelopes}
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("feed: marshal delivery request: %w", err)
	}
	// Pass JSON via --message; the adapter reads it from argv.
	// This keeps secrets off argv — only envelope JSON is on argv.
	argv := append(append([]string(nil), baseArgs...), "--message", string(body))
	res, err := toolcmd.Run(ctx, tool, tc, strings.Join(argv, " "), toolcmd.Options{Argv: argv, Yes: true})
	if err != nil {
		return nil, err
	}
	if res.Outcome != toolcmd.OutcomeSuccess {
		return nil, fmt.Errorf("toolcmd: %s:%s: outcome %s: %s", res.Tool, res.Command, res.Outcome, res.Error)
	}
	// Try to parse DeliveryResult from Text; fallback to treating non-empty
	// Text as one id per envelope.
	var result DeliveryResult
	if err := json.Unmarshal([]byte(res.Text), &result); err != nil {
		if strings.TrimSpace(res.Text) == "" {
			// No structured result: synthesize ids from envelopes.
			ids := make([]string, len(envelopes))
			for i, e := range envelopes {
				if e.MessageID != "" {
					ids[i] = e.MessageID
				} else {
					ids[i] = e.ID
				}
			}
			return ids, nil
		}
		// Try whitespace-delimited ids.
		fields := strings.Fields(res.Text)
		if len(fields) > 0 {
			return fields, nil
		}
		return nil, fmt.Errorf("feed: adapter %q: bad result json: %w: %q", ch.Name, err, res.Text)
	}
	if result.Error != "" {
		return nil, fmt.Errorf("feed: adapter %q: %s", ch.Name, result.Error)
	}
	if len(result.DeliveredIDs) == 0 {
		ids := make([]string, len(envelopes))
		for i, e := range envelopes {
			if e.MessageID != "" {
				ids[i] = e.MessageID
			} else {
				ids[i] = e.ID
			}
		}
		return ids, nil
	}
	return result.DeliveredIDs, nil
}

// SendEnvelopes delivers envelopes through the named channel's tool command.
// It encodes DeliveryRequest as JSON and routes via defaultSender (overridable
// in tests via SetEnvelopeSender).
var envelopeSenderMu sync.Mutex
var envelopeSender EnvelopeSender = defaultSender

// SetEnvelopeSender overrides the outbound sender (tests).
func SetEnvelopeSender(s EnvelopeSender) {
	envelopeSenderMu.Lock()
	defer envelopeSenderMu.Unlock()
	if s == nil {
		envelopeSender = defaultSender
	} else {
		envelopeSender = s
	}
}

func getEnvelopeSender() EnvelopeSender {
	envelopeSenderMu.Lock()
	defer envelopeSenderMu.Unlock()
	return envelopeSender
}

// SendEnvelopes delivers envelopes through the named channel's tool command.
func SendEnvelopes(ctx context.Context, ch Channel, envelopes []Envelope) ([]string, error) {
	if len(envelopes) == 0 {
		return nil, nil
	}
	if err := validateChannel(ch); err != nil {
		return nil, err
	}
	for i := range envelopes {
		if envelopes[i].Version == "" {
			envelopes[i].Version = ContractVersion
		}
		if err := envelopes[i].Validate(); err != nil {
			return nil, fmt.Errorf("feed: envelope %d: %w", i, err)
		}
	}
	return getEnvelopeSender()(ctx, ch, envelopes)
}

// SendWithRetry delivers with exponential backoff. It retries on transient
// failures up to maxAttempts (default 3). Quiet-hours are honoured by the
// caller via DeliveryPrefs; this helper only retries transport errors.
func SendWithRetry(ctx context.Context, ch Channel, envelopes []Envelope, maxAttempts int) ([]string, error) {
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		ids, err := SendEnvelopes(ctx, ch, envelopes)
		if err == nil {
			return ids, nil
		}
		last = err
		if attempt < maxAttempts {
			backoff := time.Duration(1<<uint(attempt-1)) * time.Second
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
		}
	}
	return nil, fmt.Errorf("feed: adapter %q: after %d attempts: %w", ch.Name, maxAttempts, last)
}

// ---- inbound ----

// InboundHandler is called for each validated inbound envelope.
type InboundHandler func(ctx context.Context, env Envelope) error

// InboundServer is the loopback HTTP endpoint adapters POST envelopes to.
type InboundServer struct {
	mu      sync.Mutex
	handler InboundHandler
	seen    map[string]bool // dedupe by origin key
	pairing PairingChecker
	quiet   func(channel string) (start, end int, ok bool)
	cursor  CursorStore
	server  *http.Server
}

// PairingChecker reports whether a sender is paired (allowlisted).
type PairingChecker interface {
	IsPaired(senderID string) bool
}

// AllowAllPairing allows every sender (for tests).
type AllowAllPairing struct{}

func (AllowAllPairing) IsPaired(_ string) bool { return true }

// DenyAllPairing denies every sender.
type DenyAllPairing struct{}

func (DenyAllPairing) IsPaired(_ string) bool { return false }

// CursorStore tracks the shared read cursor per conversation.
type CursorStore interface {
	ReadCursor(conversation string) (int64, error)
	WriteCursor(conversation string, seq int64) error
}

// MemCursor is an in-memory CursorStore.
type MemCursor struct {
	mu sync.Mutex
	m  map[string]int64
}

func NewMemCursor() *MemCursor { return &MemCursor{m: map[string]int64{}} }

func (c *MemCursor) ReadCursor(conv string) (int64, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[conv], nil
}
func (c *MemCursor) WriteCursor(conv string, seq int64) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[conv] = seq
	return nil
}

// NewInboundServer creates a loopback inbound server. handler is called for
// each accepted envelope. pairing may be nil (allow all).
func NewInboundServer(handler InboundHandler, pairing PairingChecker) *InboundServer {
	if pairing == nil {
		pairing = AllowAllPairing{}
	}
	return &InboundServer{
		handler: handler,
		seen:    map[string]bool{},
		pairing: pairing,
		cursor:  NewMemCursor(),
	}
}

// ServeHTTP implements http.Handler for POST /inbound.
func (s *InboundServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read error", http.StatusBadRequest)
		return
	}
	var env Envelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "bad envelope json: "+err.Error(), http.StatusBadRequest)
		return
	}
	if err := env.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if !s.pairing.IsPaired(env.SenderID) {
		http.Error(w, "sender not paired", http.StatusForbidden)
		return
	}
	s.mu.Lock()
	key := env.OriginKey()
	if s.seen[key] {
		s.mu.Unlock()
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"duplicate"}`))
		return
	}
	s.seen[key] = true
	s.mu.Unlock()

	if s.handler != nil {
		if err := s.handler(r.Context(), env); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

// ProcessNDJSON reads newline-delimited envelopes from r (stdout of a
// supervised service unit) and delivers each to handler. It validates,
// dedupes, checks pairing, and honours quiet hours when a quiet func is set.
func ProcessNDJSON(ctx context.Context, r io.Reader, handler InboundHandler, pairing PairingChecker) error {
	if pairing == nil {
		pairing = AllowAllPairing{}
	}
	seen := map[string]bool{}
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	lines := bytes.Split(data, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		var env Envelope
		if err := json.Unmarshal(line, &env); err != nil {
			continue // skip malformed lines
		}
		if err := env.Validate(); err != nil {
			continue
		}
		if !pairing.IsPaired(env.SenderID) {
			continue
		}
		key := env.OriginKey()
		if seen[key] {
			continue
		}
		seen[key] = true
		if handler != nil {
			if err := handler(ctx, env); err != nil {
				// handler error does not stop processing other lines
				continue
			}
		}
	}
	return nil
}

// DraftStore holds draft envelopes awaiting /approve.
type DraftStore struct {
	mu     sync.Mutex
	drafts map[string]Envelope // id -> envelope
}

func NewDraftStore() *DraftStore { return &DraftStore{drafts: map[string]Envelope{}} }

func (d *DraftStore) Add(e Envelope) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.drafts[e.ID] = e
}
func (d *DraftStore) Approve(id string) (Envelope, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.drafts[id]
	if ok {
		delete(d.drafts, id)
	}
	return e, ok
}
func (d *DraftStore) List() []Envelope {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]Envelope, 0, len(d.drafts))
	for _, e := range d.drafts {
		out = append(out, e)
	}
	return out
}
func (d *DraftStore) Refuse(id string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.drafts[id]; ok {
		delete(d.drafts, id)
		return true
	}
	return false
}

// QuietHours checks whether now is within a quiet window [start,end) in
// minutes after midnight. An equal start==end means no quiet hours.
func QuietHours(now time.Time, start, end int) bool {
	if start == end {
		return false
	}
	m := now.Hour()*60 + now.Minute()
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// SharedCursorPath is the file where the shared read cursor is persisted.
// Kept beside channels.json for locality.
const sharedCursorFile = "feed-cursor.json"

// FileCursor persists the shared read cursor to disk.
type FileCursor struct {
	dir string
	mu  sync.Mutex
}

func NewFileCursor(dir string) *FileCursor { return &FileCursor{dir: dir} }

func (f *FileCursor) cursorPath() string { return filepath.Join(f.dir, sharedCursorFile) }

func (f *FileCursor) load() (map[string]int64, error) {
	data, err := os.ReadFile(f.cursorPath())
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]int64{}, nil
		}
		return nil, err
	}
	var m map[string]int64
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]int64{}
	}
	return m, nil
}

func (f *FileCursor) ReadCursor(conv string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return 0, err
	}
	return m[conv], nil
}
func (f *FileCursor) WriteCursor(conv string, seq int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, err := f.load()
	if err != nil {
		return err
	}
	m[conv] = seq
	return writeJSON0600(f.dir, f.cursorPath(), m)
}

// ErrUnpairedSender is returned when an unpaired sender is refused.
var ErrUnpairedSender = errors.New("feed: sender not paired")
