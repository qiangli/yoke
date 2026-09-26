package sched

import (
	"testing"
	"time"
)

func TestDeriveSessionID_StableAcrossTurns(t *testing.T) {
	// A real multi-turn conversation: clients re-send the full
	// history each turn. The session ID must be the same for
	// turn 1 (system+first-user) and turn 3 (system+first-user+
	// assistant+next-user). We hash only the prefix.
	turn1 := []byte(`{"model":"x","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hello"}
	]}`)
	turn3 := []byte(`{"model":"x","messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hello"},
		{"role":"assistant","content":"hi"},
		{"role":"user","content":"what's the time?"}
	]}`)
	g1 := DeriveSessionID(turn1)
	g3 := DeriveSessionID(turn3)
	if g1 == "" {
		t.Fatal("derive turn1 returned empty")
	}
	if g1 != g3 {
		t.Errorf("session ID drifted between turns: turn1=%q turn3=%q", g1, g3)
	}
}

func TestDeriveSessionID_DifferentConversationsDifferent(t *testing.T) {
	// Same system prompt, different first user message → different
	// session. Same system prompt, same first user message but
	// different second-turn user message → SAME session (because we
	// only hash the prefix).
	a := []byte(`{"messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hello A"}
	]}`)
	b := []byte(`{"messages":[
		{"role":"system","content":"be brief"},
		{"role":"user","content":"hello B"}
	]}`)
	ga := DeriveSessionID(a)
	gb := DeriveSessionID(b)
	if ga == "" || gb == "" {
		t.Fatal("got empty session IDs")
	}
	if ga == gb {
		t.Errorf("distinct first-user messages should give distinct IDs (got %q == %q)", ga, gb)
	}
}

func TestDeriveSessionID_NoMessages(t *testing.T) {
	// Missing or empty messages → empty session ID → caller will
	// skip the affinity lookup and fall through to capacity routing.
	for _, body := range [][]byte{
		[]byte(`{}`),
		[]byte(`{"messages":[]}`),
		[]byte(`not-json`),
	} {
		if got := DeriveSessionID(body); got != "" {
			t.Errorf("DeriveSessionID(%q) = %q, want empty", body, got)
		}
	}
}

func TestDeriveSessionID_VisionContent(t *testing.T) {
	// Vision payloads have content as an array; the parser must
	// preserve raw bytes so hashing still works without us decoding
	// the multimodal variant.
	body := []byte(`{"messages":[
		{"role":"user","content":[
			{"type":"text","text":"describe this"},
			{"type":"image_url","image_url":{"url":"data:image/png;base64,aaaa"}}
		]}
	]}`)
	got := DeriveSessionID(body)
	if got == "" {
		t.Errorf("vision body should produce a session ID, got empty")
	}
}

func TestAffinityCache_GetThenStickHitsSameBackend(t *testing.T) {
	cache := NewAffinityCache()
	cache.Stick("sess1", "backend-a", "llama")
	backend, model, ok := cache.Get("sess1")
	if !ok || backend != "backend-a" || model != "llama" {
		t.Errorf("Get after Stick: backend=%q model=%q ok=%v", backend, model, ok)
	}
}

func TestAffinityCache_TTLExpiry(t *testing.T) {
	cache := NewAffinityCache()
	cache.Stick("sess1", "backend-a", "llama")
	// Backdate the entry past the TTL.
	cache.mu.Lock()
	e := cache.bySessID["sess1"]
	e.lastUsed = time.Now().Add(-affinityTTL - time.Second)
	cache.bySessID["sess1"] = e
	cache.mu.Unlock()

	if _, _, ok := cache.Get("sess1"); ok {
		t.Error("expected expired entry to evict on Get")
	}
	// Confirm it was actually deleted, not just reported missing.
	cache.mu.Lock()
	_, present := cache.bySessID["sess1"]
	cache.mu.Unlock()
	if present {
		t.Error("expired entry should be removed from the map")
	}
}

func TestAffinityCache_EmptySessionIDIsNoOp(t *testing.T) {
	cache := NewAffinityCache()
	cache.Stick("", "backend-a", "llama")
	if _, _, ok := cache.Get(""); ok {
		t.Error("empty session ID should never hit the cache")
	}
	cache.mu.Lock()
	count := len(cache.bySessID)
	cache.mu.Unlock()
	if count != 0 {
		t.Errorf("empty session ID Stick() should not insert: got %d entries", count)
	}
}

func TestAffinityCache_StickOverwrites(t *testing.T) {
	// When the previously-sticky backend disappears from the candidate
	// set, the caller can pick a fresh backend and rebind it.
	cache := NewAffinityCache()
	cache.Stick("sess1", "backend-a", "llama")
	cache.Stick("sess1", "backend-b", "llama")
	backend, _, ok := cache.Get("sess1")
	if !ok || backend != "backend-b" {
		t.Errorf("after rebind: backend=%q ok=%v, want backend=backend-b", backend, ok)
	}
}
