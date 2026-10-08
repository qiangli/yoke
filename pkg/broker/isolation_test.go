package broker

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestForeignUnixPeerCannotUseOwnerToken(t *testing.T) {
	h := newHarness(t, nil)
	r := httptest.NewRequest("GET", "/health", nil)
	r.Header.Set("Authorization", "Bearer "+testToken)
	r = r.WithContext(context.WithValue(r.Context(), unixOwnerKey, false))
	w := httptest.NewRecorder()
	h.b.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("foreign peer accepted: %d", w.Code)
	}
}

func TestRejectedRequestHasAuditRecord(t *testing.T) {
	h := newHarness(t, nil)
	r := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader("invalid"))
	r.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.b.ServeHTTP(w, r)
	records := h.b.audit.snapshot()
	if len(records) != 1 || records[0].Status != 400 || records[0].Principal == "" {
		t.Fatalf("missing refusal audit: %+v", records)
	}
}

func TestPrincipalSwitchFailsClosed(t *testing.T) {
	h := newHarness(t, nil)
	release, _, err := h.b.device.Acquire(context.Background(), ClassInteractive, "alice")
	if err != nil {
		t.Fatal(err)
	}
	release()
	h.eng.server.Close()
	for i := 0; i < 2; i++ {
		release, _, err = h.b.device.Acquire(context.Background(), ClassInteractive, "bob")
		if release != nil {
			release()
		}
		if err == nil {
			t.Fatal("admitted bob when alice's cache could not be evicted")
		}
	}
}

func TestStickyContextCannotCrossSession(t *testing.T) {
	s := newStickyStore("", time.Now)
	binding := &Binding{scoped: scoped{Principal: "alice", Session: "s-parent", Exported: true, Created: time.Now()}, Spec: StickySpec{Key: "context", Reset: ResetNone, Bind: BindWorker}, Digest: "abcdef1234567890"}
	if _, err := s.put(binding); err != nil {
		t.Fatal(err)
	}
	if s.get("alice", "s-parent", "context") == nil {
		t.Fatal("owner lost context")
	}
	for _, caller := range []struct{ principal, session string }{{"bob", "s-parent"}, {"alice", "s-parent~child"}, {"alice", CloneSession("s-parent", time.Now())}} {
		if s.get(caller.principal, caller.session, "context") != nil {
			t.Errorf("context exposed to %+v", caller)
		}
		if s.findDigest(caller.principal, caller.session, binding.Digest) != nil {
			t.Errorf("context digest exposed to %+v", caller)
		}
	}
}

func TestExpiredContextDigestCannotBeRecovered(t *testing.T) {
	now := time.Now()
	s := newStickyStore("", func() time.Time { return now })
	b := &Binding{scoped: scoped{Principal: "alice"}, Spec: StickySpec{Key: "context", TTL: "1s"}, Digest: "abcdef1234567890"}
	if _, err := s.put(b); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	if s.findDigest("alice", "", b.Digest) != nil {
		t.Fatal("expired context recovered by digest")
	}
}

// The fake runner reports a hit exactly when its previous prompt survives.
// Identical bytes must miss after a principal OR session switch.
func TestPrefixCacheIsolation(t *testing.T) {
	cached := false
	engine := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags", "/api/ps":
			io.WriteString(w, `{"models":[{"name":"test:latest","digest":"d"}]}`)
		case "/api/generate":
			var p map[string]any
			json.NewDecoder(r.Body).Decode(&p)
			if p["keep_alive"] == float64(0) {
				cached = false
				io.WriteString(w, `{"done":true}`)
				return
			}
			json.NewEncoder(w).Encode(map[string]any{"hit": cached, "done": true})
			cached = true
		default:
			http.NotFound(w, r)
		}
	}))
	defer engine.Close()
	b, err := New(context.Background(), Options{Token: "token", Engine: StaticEngine(engine.URL)})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		principal, session string
		hit                bool
	}{{"alice", "s-one", false}, {"alice", "s-one", true}, {"bob", "s-one", false}, {"alice", "s-one", false}, {"alice", "s-two", false}} {
		payload := map[string]json.RawMessage{"model": json.RawMessage(`"test"`), "prompt": json.RawMessage(`"private prefix"`)}
		r := httptest.NewRequest("POST", "/api/generate", nil)
		w := httptest.NewRecorder()
		ri := &reqInfo{principal: tc.principal, session: tc.session, class: ClassInteractive, started: time.Now()}
		b.serveLocal(w, r, ri, payload, "test", nil, true)
		var result struct{ Hit bool }
		if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil {
			t.Fatalf("response: %d %s", w.Code, w.Body.String())
		}
		if result.Hit != tc.hit {
			t.Fatalf("%s/%s cache hit=%v want %v", tc.principal, tc.session, result.Hit, tc.hit)
		}
	}
}

func TestUnknownToolVersionIsRetried(t *testing.T) {
	probes := 0
	v := &toolVersions{probe: func(string) []string { probes++; return []string{filepath.Join(t.TempDir(), "missing")} }}
	for i := 0; i < 2; i++ {
		if got := v.get("new-tool"); got != "unknown" {
			t.Fatalf("version=%q", got)
		}
	}
	if probes != 2 {
		t.Fatalf("cached failed catalog lookup; probes=%d", probes)
	}
}

func TestRestoredStickyContextIsOwnerSessionOnly(t *testing.T) {
	now := time.Now()
	path := filepath.Join(t.TempDir(), "sticky.json")
	s := newStickyStore(path, func() time.Time { return now })
	b := &Binding{scoped: scoped{Principal: "alice", Session: "s-parent", Exported: true, Created: now},
		Spec: StickySpec{Key: "context", Reset: ResetNone, Bind: BindWorker, TTL: "1s"}, Digest: "abcdef1234567890"}
	if _, err := s.put(b); err != nil {
		t.Fatal(err)
	}
	messages := []json.RawMessage{json.RawMessage(`{"role":"user","content":"private context"}`)}
	if _, err := s.use("alice", "s-parent", "context", messages); err != nil {
		t.Fatal(err)
	}
	s = newStickyStore(path, func() time.Time { return now })
	if restored := s.get("alice", "s-parent", "context"); restored == nil || len(restored.Transcript) != 1 {
		t.Fatal("owner session failed to restore its transcript")
	}
	for _, caller := range []struct{ principal, session string }{
		{"bob", "s-parent"}, {"alice", "s-other"}, {"alice", "s-parent~child"}, {"alice", CloneSession("s-parent", now)},
	} {
		if s.get(caller.principal, caller.session, "context") != nil || s.findDigest(caller.principal, caller.session, b.Digest) != nil {
			t.Errorf("restored context visible to %+v", caller)
		}
		if _, err := s.use(caller.principal, caller.session, "context", messages); err == nil {
			t.Errorf("restored context used by %+v", caller)
		}
		if s.delete(caller.principal, caller.session, "context") {
			t.Errorf("restored context deleted by %+v", caller)
		}
	}
	now = now.Add(2 * time.Second)
	s = newStickyStore(path, func() time.Time { return now })
	if s.get("alice", "s-parent", "context") != nil || s.findDigest("alice", "s-parent", b.Digest) != nil {
		t.Fatal("expired context recovered after restart")
	}
}
