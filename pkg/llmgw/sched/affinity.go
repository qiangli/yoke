package sched

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"time"
)

// Session affinity for /v1/chat/completions routing.
//
// Without affinity, every multi-turn chat request gets independently
// routed to the lowest-load backend. That's correct for *load*, but
// catastrophic for *latency* on long contexts: every time the chat
// lands on a different backend, the new backend has to re-prefill the
// full message history from scratch (no KV-cache reuse). For a 30K-
// token context that's literal seconds of GPU time at the top of every
// turn.
//
// The fix is to pin a (conversation → backend) mapping for as long as
// the operator is plausibly mid-conversation. The next request whose
// derived session ID matches goes back to the same backend, keeping
// any prompt-prefix cache hot. Callers can fall through to their normal
// selection policy when the sticky backend is no longer eligible.
//
// Inspired by olol's session_map; the difference here is we derive the
// session ID from the message prefix rather than requiring the client
// to send an explicit conversation ID, so off-the-shelf OpenAI clients
// (which don't carry a stable conversation header) get the benefit
// transparently.

// affinityTTL is how long a sticky (session → backend) mapping survives
// without being re-used. Sized for "user is actively chatting" —
// 10 min covers thinking pauses, tool-execution gaps, doc-reading, but
// expires fast enough that an idle conversation doesn't pin a backend
// forever after the operator has moved on.
const affinityTTL = 10 * time.Minute

// affinityEntry records one sticky binding. Model is recorded so a
// mid-conversation model switch (rare, but happens when a client
// retries with a different size) invalidates the affinity and routes
// fresh — the previous backend's prefix cache is useless against a
// different model anyway.
type affinityEntry struct {
	backend  string
	model    string
	lastUsed time.Time
}

// AffinityCache is a (session ID → backend) store.
// Map + mutex is sufficient at our scale; switch to sync.Map only if
// profiling shows the cache lookup is the bottleneck (it won't —
// model routing is dominated by capacity probes).
type AffinityCache struct {
	mu       sync.Mutex
	bySessID map[string]affinityEntry
}

// NewAffinityCache returns an isolated affinity cache.
func NewAffinityCache() *AffinityCache {
	return &AffinityCache{bySessID: map[string]affinityEntry{}}
}

// Get returns the sticky backend for a session, if any, and refreshes
// lastUsed on hit (sliding TTL — actively chatting users keep their
// pin; idle conversations expire). Returns ("", "", false) when the
// session ID is empty, no entry exists, or the entry is past TTL.
func (c *AffinityCache) Get(sessionID string) (backend, model string, ok bool) {
	if sessionID == "" {
		return "", "", false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, hit := c.bySessID[sessionID]
	if !hit {
		return "", "", false
	}
	if time.Since(e.lastUsed) > affinityTTL {
		delete(c.bySessID, sessionID)
		return "", "", false
	}
	e.lastUsed = time.Now()
	c.bySessID[sessionID] = e
	return e.backend, e.model, true
}

// Stick records a (session, backend, model) binding. Called from the
// chat handler AFTER a successful route decision so we only record
// backends that actually received traffic.
//
// Replaces any existing entry under the same session ID — useful when
// the previously-sticky backend disappears: the next pickLeastLoaded
// produces a new backend, and Stick() rebinds without us needing a
// separate invalidation path.
func (c *AffinityCache) Stick(sessionID, backend, model string) {
	if sessionID == "" || backend == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.bySessID[sessionID] = affinityEntry{
		backend:  backend,
		model:    model,
		lastUsed: time.Now(),
	}
	// Cheap opportunistic GC. Bounded by the size of the cache at the
	// moment of a write; a full scan O(n) is fine at chat-traffic
	// scale (single-digit thousands of active sessions even at LLM-
	// pool production scale). If the cache ever grows to where this
	// hurts, switch to a separate background sweeper goroutine.
	if len(c.bySessID) > 32 {
		cutoff := time.Now().Add(-affinityTTL)
		for k, v := range c.bySessID {
			if v.lastUsed.Before(cutoff) {
				delete(c.bySessID, k)
			}
		}
	}
}

// chatMessage is the minimal slice of an OpenAI message we need to
// derive a stable session ID. Content can be a string OR an array of
// content parts (vision payloads, multi-modal). RawMessage preserves
// either shape for hashing without forcing us to parse the vision
// variant — bit-identical message prefix yields bit-identical hash.
type chatMessage struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// chatBodyForAffinity is just enough of the request to peel off the
// first two messages without decoding the full request shape.
type chatBodyForAffinity struct {
	Messages []chatMessage `json:"messages"`
}

// DeriveSessionID computes a stable conversation identifier from the
// first 1–2 messages of an OpenAI chat request. The hash includes
// role + raw content bytes; two requests in the same conversation
// will resubmit the same messages[0:N] prefix (clients send full
// history per turn), so the prefix hash is stable across turns.
//
// Strategy: take messages[0] (the system message in typical use), and
// messages[1] (the first user turn) when present. Skip the rest —
// they're the turn-by-turn additions that change every request.
//
// Returns "" when the body is shaped wrong, the messages array is
// empty, or both first messages are empty. An empty session ID
// causes the affinity cache to skip the lookup.
func DeriveSessionID(body []byte) string {
	var b chatBodyForAffinity
	if err := json.Unmarshal(body, &b); err != nil {
		return ""
	}
	if len(b.Messages) == 0 {
		return ""
	}
	// Bound how many messages we hash: 2 is plenty for a stable ID,
	// and avoids re-hashing growing histories on each turn.
	prefix := b.Messages
	if len(prefix) > 2 {
		prefix = prefix[:2]
	}
	h := sha256.New()
	for _, m := range prefix {
		role := strings.ToLower(strings.TrimSpace(m.Role))
		if role == "" && len(m.Content) == 0 {
			continue
		}
		h.Write([]byte(role))
		h.Write([]byte{0})
		h.Write(m.Content)
		h.Write([]byte{0})
	}
	sum := h.Sum(nil)
	if len(sum) == 0 {
		return ""
	}
	// 16 hex chars = 64 bits of namespace. Collision probability is
	// negligible compared to the user-facing harm of one (worst case:
	// two distinct conversations share a sticky backend).
	return hex.EncodeToString(sum[:8])
}
