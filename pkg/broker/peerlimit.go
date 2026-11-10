package broker

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// Per-peer limits (Sprint 602 A4). The owner sets caps on a peer's record in
// peers.json; the door enforces them in ServeHTTP right after the peer is
// authenticated and its route admitted — the only point that still knows the
// peer (delegateCLI swaps the door token for cligw's own). Counters are kept
// in memory, seeded from the audit log at start, and advanced as each
// request finishes. Admission compares what is ALREADY spent against the cap,
// so requests in flight when the cap is crossed may overshoot it once; there
// are no reservations. The owner is never limited.

// PeerLimits caps one peer. A zero field is unlimited. Tokens are prompt +
// completion tokens as the audit record counts them; USD is the audit
// record's cost_usd (an unpriced model costs nothing against the cap).
type PeerLimits struct {
	TokensDay uint64  `json:"tokens_day,omitempty"`
	USDMonth  float64 `json:"usd_month,omitempty"`
	RPM       int     `json:"rpm,omitempty"`
}

// IsZero reports whether no cap is set.
func (l PeerLimits) IsZero() bool { return l.TokensDay == 0 && l.USDMonth == 0 && l.RPM == 0 }

// String renders the caps for listings ("-" when none).
func (l PeerLimits) String() string {
	if l.IsZero() {
		return "-"
	}
	s := ""
	add := func(part string) {
		if s != "" {
			s += " "
		}
		s += part
	}
	if l.TokensDay > 0 {
		add(fmt.Sprintf("tokens/day=%d", l.TokensDay))
	}
	if l.USDMonth > 0 {
		add("usd/month=" + strconv.FormatFloat(l.USDMonth, 'f', -1, 64))
	}
	if l.RPM > 0 {
		add(fmt.Sprintf("rpm=%d", l.RPM))
	}
	return s
}

// SetPeerLimits replaces an existing peer's caps; all-zero clears them.
func SetPeerLimits(path, name string, lim PeerLimits, now time.Time) error {
	if lim.USDMonth < 0 || math.IsNaN(lim.USDMonth) || math.IsInf(lim.USDMonth, 0) || lim.RPM < 0 {
		return errors.New("broker: limits must be finite and not negative")
	}
	ps, err := LoadPeerShares(path)
	if err != nil {
		return err
	}
	rec, ok := ps.Peers[name]
	if !ok {
		return fmt.Errorf("broker: no peer %q (mint one with `llm peer add %s`)", name, name)
	}
	if lim.IsZero() {
		rec.Limits = nil
	} else {
		rec.Limits = &lim
	}
	rec.Updated = now
	ps.Peers[name] = rec
	return savePeerShares(path, ps)
}

// Limit names, as reported in a refusal.
const (
	LimitTokensDay = "tokens_day"
	LimitUSDMonth  = "usd_month"
	LimitRPM       = "rpm"
)

// limitRefusal is why a peer request was refused and when it may retry.
type limitRefusal struct {
	Principal string
	Limit     string
	Used      float64
	Cap       float64
	Reset     time.Time
}

func (r *limitRefusal) message() string {
	return fmt.Sprintf("%s is over its %s limit (used %s of %s); resets %s",
		r.Principal, r.Limit, fmtAmount(r.Used), fmtAmount(r.Cap), r.Reset.UTC().Format(time.RFC3339))
}

func fmtAmount(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// peerMeter holds every peer's running usage for the current UTC day
// (tokens), UTC month (USD) and trailing minute (admitted requests).
type peerMeter struct {
	mu     sync.Mutex
	day    time.Time
	month  time.Time
	tokens map[string]uint64
	usd    map[string]float64
	recent map[string][]time.Time
}

func newPeerMeter() *peerMeter {
	return &peerMeter{tokens: map[string]uint64{}, usd: map[string]float64{}, recent: map[string][]time.Time{}}
}

func dayStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
}

func monthStart(t time.Time) time.Time {
	t = t.UTC()
	return time.Date(t.Year(), t.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// roll starts fresh day/month buckets once now has left them. Caller holds mu.
func (m *peerMeter) roll(now time.Time) {
	if d := dayStart(now); !d.Equal(m.day) {
		m.day, m.tokens = d, map[string]uint64{}
	}
	if mo := monthStart(now); !mo.Equal(m.month) {
		m.month, m.usd = mo, map[string]float64{}
	}
}

// seed loads the current day's tokens and month's cost per peer from the
// audit log. A missing log is an empty history.
func (m *peerMeter) seed(auditPath string, now time.Time) error {
	if auditPath == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll(now)
	month, err := rollupFile(auditPath, m.month)
	if err != nil {
		return err
	}
	day, err := rollupFile(auditPath, m.day)
	if err != nil {
		return err
	}
	for _, row := range month {
		m.usd[row.Principal] = row.CostUSD
	}
	for _, row := range day {
		m.tokens[row.Principal] = row.PromptTokens + row.CompletionTokens
	}
	return nil
}

func rollupFile(path string, since time.Time) ([]PrincipalUsage, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return RollupUsage(f, UsageRollupOptions{Since: since})
}

// admit checks principal against lim at now; a nil result admits the
// request and counts it toward the RPM window.
func (m *peerMeter) admit(principal string, lim PeerLimits, now time.Time) *limitRefusal {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll(now)
	if lim.TokensDay > 0 && m.tokens[principal] >= lim.TokensDay {
		return &limitRefusal{Principal: principal, Limit: LimitTokensDay, Used: float64(m.tokens[principal]), Cap: float64(lim.TokensDay), Reset: m.day.AddDate(0, 0, 1)}
	}
	if lim.USDMonth > 0 && m.usd[principal] >= lim.USDMonth {
		return &limitRefusal{Principal: principal, Limit: LimitUSDMonth, Used: m.usd[principal], Cap: lim.USDMonth, Reset: m.month.AddDate(0, 1, 0)}
	}
	win := m.recent[principal]
	cut := 0
	for cut < len(win) && !win[cut].After(now.Add(-time.Minute)) {
		cut++
	}
	win = win[cut:]
	if lim.RPM > 0 && len(win) >= lim.RPM {
		m.recent[principal] = win
		return &limitRefusal{Principal: principal, Limit: LimitRPM, Used: float64(len(win)), Cap: float64(lim.RPM), Reset: win[0].Add(time.Minute)}
	}
	if lim.RPM > 0 {
		win = append(win, now)
	}
	if len(win) == 0 {
		delete(m.recent, principal)
	} else {
		m.recent[principal] = win
	}
	return nil
}

// observe adds a finished request's usage to principal's buckets.
func (m *peerMeter) observe(principal string, tokens uint64, usd float64, now time.Time) {
	if tokens == 0 && usd == 0 {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.roll(now)
	m.tokens[principal] += tokens
	m.usd[principal] += usd
}

// recordUsage reads a record's tokens and cost through the same JSON fields
// RollupUsage reads from audit.jsonl, so live counters and seeded counters
// can never disagree about what a record spent.
func recordUsage(rec Record) (tokens uint64, usd float64) {
	data, err := json.Marshal(rec)
	if err != nil {
		return 0, 0
	}
	rows, err := RollupUsage(bytes.NewReader(data), UsageRollupOptions{})
	if err != nil || len(rows) != 1 {
		return 0, 0
	}
	return rows[0].PromptTokens + rows[0].CompletionTokens, rows[0].CostUSD
}

// seedPeerMeter seeds b.meter from the audit log; a failure (a malformed
// line, an unreadable file) is logged and the door starts with empty
// counters rather than refusing to serve.
func (b *Broker) seedPeerMeter(auditPath string) {
	if err := b.meter.seed(auditPath, b.opts.Now()); err != nil {
		log.Printf("broker: peer limits: seeding usage from %s failed, starting with empty counters: %v", auditPath, err)
		b.meter = newPeerMeter()
	}
}

// admitPeer enforces lim for a peer request and answers the refusal itself;
// it reports whether the request may proceed.
func (b *Broker) admitPeer(w http.ResponseWriter, ri *reqInfo, lim *PeerLimits) bool {
	if lim == nil || lim.IsZero() {
		return true
	}
	now := b.opts.Now()
	ref := b.meter.admit(ri.principal, *lim, now)
	if ref == nil {
		return true
	}
	retry := int(math.Ceil(ref.Reset.Sub(now).Seconds()))
	if retry < 1 {
		retry = 1
	}
	msg := ref.message()
	b.finish(ri, http.StatusTooManyRequests, 0, msg)
	w.Header().Set("Retry-After", strconv.Itoa(retry))
	w.Header().Set("X-Bashy-Refusal", "peer-limit")
	writeJSON(w, http.StatusTooManyRequests, map[string]any{
		"type":          "peer_limit",
		"principal":     ref.Principal,
		"limit":         ref.Limit,
		"used":          ref.Used,
		"cap":           ref.Cap,
		"reset":         ref.Reset.UTC().Format(time.RFC3339),
		"retry_after_s": retry,
		"error":         map[string]any{"message": msg, "type": "peer_limit", "code": http.StatusTooManyRequests},
	})
	return false
}
