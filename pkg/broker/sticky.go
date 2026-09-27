package broker

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Sticky bindings (Q10, design §10): the exact same model instance across
// requests. A key resolves ONCE through the normal routing and freezes an
// Identity; every later request under the key is served by that identity or
// refused — never rerouted to another model.

// StickyHeader carries a binding reference or an implicit create spec.
const StickyHeader = "X-Bashy-Sticky"

// Bind and reset modes.
const (
	BindIdentity = "identity"
	BindWorker   = "worker"
	ResetEach    = "each"
	ResetNone    = "none"
)

// DefaultStickyTTL is how long an idle binding lives.
const DefaultStickyTTL = 30 * time.Minute

// StickySpec is what a caller asks for.
type StickySpec struct {
	Key      string         `json:"key"`
	Model    string         `json:"model,omitempty"`
	Filter   string         `json:"filter,omitempty"`
	Uses     int            `json:"uses,omitempty"`
	Bind     string         `json:"bind,omitempty"`
	Reset    string         `json:"reset,omitempty"`
	Identity string         `json:"identity,omitempty"`
	TTL      string         `json:"ttl,omitempty"`
	Export   bool           `json:"export,omitempty"`
	Options  map[string]any `json:"options,omitempty"`
}

// Identity is what a binding freezes. Its digest is the proof, on every
// response and run record, that two calls were served by the same instance.
type Identity struct {
	Backend     string         `json:"backend"`  // ollama-local | cligw
	Location    string         `json:"location"` // local | peer:<name> | cloudbox
	Model       string         `json:"model"`    // engine model or registry model
	ModelDigest string         `json:"model_digest,omitempty"`
	Agent       string         `json:"agent,omitempty"` // fleet agent (tool:model binding)
	Tool        string         `json:"tool,omitempty"`
	ToolVersion string         `json:"tool_version,omitempty"`
	VendorModel string         `json:"vendor_model,omitempty"`
	Provider    string         `json:"provider,omitempty"`
	Launch      string         `json:"launch,omitempty"`  // launch fingerprint
	Account     string         `json:"account,omitempty"` // x_account where known
	Options     map[string]any `json:"options,omitempty"` // num_ctx, temperature, seed …
}

// Digest is the sha256 of the identity's canonical JSON (map keys sorted by
// encoding/json).
func (id Identity) Digest() string {
	data, _ := json.Marshal(id)
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// ShortDigest is the first 12 hex digits, for headers and humans.
func ShortDigest(d string) string {
	d = strings.TrimPrefix(d, "sha256:")
	if len(d) > 12 {
		return d[:12]
	}
	return d
}

// Binding is a live sticky key.
type Binding struct {
	scoped
	Spec     StickySpec `json:"spec"`
	Identity Identity   `json:"identity"`
	Digest   string     `json:"digest"`
	Used     int        `json:"used"`
	LastUsed time.Time  `json:"last_used"`
	// Transcript is the rolling hash of each message the last reset=none
	// request sent; the next one must extend it.
	Transcript []string `json:"transcript,omitempty"`
}

func (b *Binding) ttl() time.Duration {
	if b.Spec.TTL == "" {
		return DefaultStickyTTL
	}
	d, err := time.ParseDuration(b.Spec.TTL)
	if err != nil {
		return DefaultStickyTTL
	}
	return d // 0 = never expires
}

// Remaining uses, or -1 for unbounded.
func (b *Binding) Remaining() int {
	if b.Spec.Uses <= 0 {
		return -1
	}
	return max(b.Spec.Uses-b.Used, 0)
}

// Sticky errors carry the HTTP status the broker answers with.
type StickyError struct {
	Status int
	Msg    string
}

func (e *StickyError) Error() string { return e.Msg }

func stickyErr(status int, format string, a ...any) error {
	return &StickyError{Status: status, Msg: fmt.Sprintf(format, a...)}
}

// ParseStickyHeader reads "key; uses=50; bind=identity; reset=each;
// identity=sha256:…; ttl=30m; export; model=…; filter=…; num_ctx=…;
// temperature=…; seed=…".
func ParseStickyHeader(v string) (StickySpec, error) {
	var spec StickySpec
	parts := strings.Split(v, ";")
	spec.Key = strings.TrimSpace(parts[0])
	for _, p := range parts[1:] {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		k, val, hasVal := strings.Cut(p, "=")
		k, val = strings.ToLower(strings.TrimSpace(k)), strings.TrimSpace(val)
		switch k {
		case "uses":
			n, err := strconv.Atoi(val)
			if err != nil || n < 0 {
				return spec, fmt.Errorf("sticky: uses=%q is not a count", val)
			}
			spec.Uses = n
		case "bind":
			spec.Bind = val
		case "reset":
			spec.Reset = val
		case "identity":
			spec.Identity = val
		case "ttl":
			spec.TTL = val
		case "export":
			spec.Export = !hasVal || val == "true" || val == "1"
		case "model":
			spec.Model = val
		case "filter":
			spec.Filter = val
		case "num_ctx", "temperature", "seed", "top_p", "top_k", "num_predict":
			if spec.Options == nil {
				spec.Options = map[string]any{}
			}
			spec.Options[k] = numberOrString(val)
		default:
			return spec, fmt.Errorf("sticky: unknown attribute %q (want uses, bind, reset, identity, ttl, export, model, filter, num_ctx, temperature, seed, top_p, top_k, num_predict)", k)
		}
	}
	return spec, spec.validate()
}

func numberOrString(s string) any {
	if n, err := strconv.ParseInt(s, 10, 64); err == nil {
		return n
	}
	if f, err := strconv.ParseFloat(s, 64); err == nil {
		return f
	}
	return s
}

func (s *StickySpec) validate() error {
	if err := validateKey(s.Key); err != nil {
		return err
	}
	switch s.Bind {
	case "":
		s.Bind = BindIdentity
	case BindIdentity, BindWorker:
	default:
		return fmt.Errorf("sticky: bind=%q (want identity or worker)", s.Bind)
	}
	switch s.Reset {
	case "":
		s.Reset = ResetEach
	case ResetEach, ResetNone:
	default:
		return fmt.Errorf("sticky: reset=%q (want each or none)", s.Reset)
	}
	if s.Reset == ResetNone && s.Bind != BindWorker {
		return errors.New("sticky: reset=none keeps one conversation on one worker; it needs bind=worker")
	}
	if s.TTL != "" {
		if _, err := time.ParseDuration(s.TTL); err != nil {
			return fmt.Errorf("sticky: ttl=%q: %v", s.TTL, err)
		}
	}
	// Options are normalised to JSON numbers so a digest does not depend on
	// whether they came from a header (int64) or a JSON body (float64).
	for k, v := range s.Options {
		if n, ok := v.(int64); ok {
			s.Options[k] = float64(n)
		}
		if n, ok := v.(int); ok {
			s.Options[k] = float64(n)
		}
	}
	return nil
}

func validateKey(k string) error {
	if k == "" {
		return errors.New("sticky: a key is required")
	}
	if len(k) > 128 {
		return errors.New("sticky: key longer than 128 characters")
	}
	for _, r := range k {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("sticky: key %q may contain only letters, digits, - _ .", k)
		}
	}
	return nil
}

// stickyStore holds the bindings, keyed by principal then key, and persists
// them so a broker restart does not silently re-resolve a running benchmark.
type stickyStore struct {
	mu   sync.Mutex
	m    map[string]*Binding // principal + "\x00" + key
	path string
	now  func() time.Time
}

func newStickyStore(path string, now func() time.Time) *stickyStore {
	s := &stickyStore{m: map[string]*Binding{}, path: path, now: now}
	s.load()
	return s
}

func storeKey(principal, key string) string { return principal + "\x00" + key }

func (s *stickyStore) load() {
	if s.path == "" {
		return
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		return
	}
	var list []*Binding
	if json.Unmarshal(data, &list) != nil {
		return
	}
	for _, b := range list {
		s.m[storeKey(b.Principal, b.Spec.Key)] = b
	}
}

func (s *stickyStore) saveLocked() {
	if s.path == "" {
		return
	}
	list := make([]*Binding, 0, len(s.m))
	for _, b := range s.m {
		list = append(list, b)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Spec.Key < list[j].Spec.Key })
	data, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return
	}
	_ = os.MkdirAll(filepath.Dir(s.path), 0o700)
	tmp := s.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, s.path)
	}
}

func (s *stickyStore) sweepLocked() {
	now := s.now()
	changed := false
	for k, b := range s.m {
		if ttl := b.ttl(); ttl > 0 && now.Sub(b.LastUsed) > ttl {
			delete(s.m, k)
			changed = true
		}
	}
	if changed {
		s.saveLocked()
	}
}

// get returns the binding visible to (principal, session), or nil.
func (s *stickyStore) get(principal, session, key string) *Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	b := s.m[storeKey(principal, key)]
	if b == nil || !visible(b.scoped, principal, session) {
		return nil
	}
	cp := *b
	return &cp
}

// findDigest returns a visible binding with the given identity digest (or
// digest prefix), so another arm can bind to the same identity.
func (s *stickyStore) findDigest(principal, session, digest string) *Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	want := ShortDigest(digest)
	for _, b := range s.m {
		if visible(b.scoped, principal, session) && strings.HasPrefix(ShortDigest(b.Digest), want) {
			cp := *b
			return &cp
		}
	}
	return nil
}

// put stores a new binding; an existing key with a different identity is a
// conflict, the same identity is idempotent.
func (s *stickyStore) put(b *Binding) (*Binding, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	k := storeKey(b.Principal, b.Spec.Key)
	if old := s.m[k]; old != nil {
		if old.Digest != b.Digest {
			return nil, stickyErr(409, "sticky: key %q is already bound to identity %s (this spec resolves to %s); delete it first",
				b.Spec.Key, ShortDigest(old.Digest), ShortDigest(b.Digest))
		}
		cp := *old
		return &cp, nil
	}
	b.LastUsed = s.now()
	s.m[k] = b
	s.saveLocked()
	cp := *b
	return &cp, nil
}

// use consumes one use, checking the uses budget and, for reset=none, that
// messages extend the recorded transcript. It returns the use number.
func (s *stickyStore) use(principal, key string, messages []json.RawMessage) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b := s.m[storeKey(principal, key)]
	if b == nil {
		return 0, stickyErr(404, "sticky: key %q is gone (expired or deleted)", key)
	}
	if b.Spec.Uses > 0 && b.Used >= b.Spec.Uses {
		return 0, stickyErr(409, "sticky exhausted: key %q has used all %d uses", key, b.Spec.Uses)
	}
	if b.Spec.Reset == ResetNone && messages != nil {
		hashes := rollingHashes(messages)
		if len(hashes) < len(b.Transcript) || (len(b.Transcript) > 0 && hashes[len(b.Transcript)-1] != b.Transcript[len(b.Transcript)-1]) {
			return 0, stickyErr(409, "sticky transcript diverged: key %q holds a %d-message conversation this request does not extend", key, len(b.Transcript))
		}
		b.Transcript = hashes
	}
	b.Used++
	b.LastUsed = s.now()
	s.saveLocked()
	return b.Used, nil
}

func (s *stickyStore) delete(principal, session, key string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := storeKey(principal, key)
	b := s.m[k]
	if b == nil || !visible(b.scoped, principal, session) {
		return false
	}
	delete(s.m, k)
	s.saveLocked()
	return true
}

func (s *stickyStore) list(principal, session string) []Binding {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sweepLocked()
	var out []Binding
	for _, b := range s.m {
		if visible(b.scoped, principal, session) {
			out = append(out, *b)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Spec.Key < out[j].Spec.Key })
	return out
}

// rollingHashes returns h[i] = sha256(h[i-1] || canonical(messages[i])).
func rollingHashes(messages []json.RawMessage) []string {
	out := make([]string, len(messages))
	prev := ""
	for i, m := range messages {
		var v any
		canon := []byte(m)
		if json.Unmarshal(m, &v) == nil {
			if c, err := json.Marshal(v); err == nil {
				canon = c
			}
		}
		sum := sha256.Sum256(append([]byte(prev), canon...))
		prev = hex.EncodeToString(sum[:])
		out[i] = prev
	}
	return out
}
