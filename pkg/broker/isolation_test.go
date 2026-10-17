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

// A peer host holds no share of this door: until per-peer admission (Sprint
// 308 A5, 53fde6fa2d0d) exists, any bearer other than the owner's is refused
// on every surface before routing, and the refusal never echoes what the peer
// presented. toolcmd's --peer refusal relies on this door contract.
func TestPeerBearerIsNotAShare(t *testing.T) {
	h := newHarness(t, nil)
	const peerToken = "peer-seat-token-not-shared"
	for _, tc := range []struct{ path, header, value string }{
		{"/v1/chat/completions", "Authorization", "Bearer " + peerToken},
		{"/v1/messages", "x-api-key", peerToken},
		{"/k/" + peerToken + "/v1/chat/completions", "", ""},
		{"/cligw/health", "Authorization", "Bearer " + peerToken},
	} {
		r := httptest.NewRequest("POST", tc.path, strings.NewReader(`{"model":"claude:opus","messages":[]}`))
		if tc.header != "" {
			r.Header.Set(tc.header, tc.value)
		}
		w := httptest.NewRecorder()
		h.b.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("%s: peer bearer admitted: %d", tc.path, w.Code)
		}
		if strings.Contains(w.Body.String(), peerToken) {
			t.Fatalf("%s: refusal echoes the peer token: %s", tc.path, w.Body.String())
		}
	}
	h.eng.mu.Lock()
	defer h.eng.mu.Unlock()
	if h.eng.maxSeen.Load() != 0 || h.eng.lastBody != nil {
		t.Fatalf("peer refusal reached the engine: body %v", h.eng.lastBody)
	}
}

// Every request leaves one audit record carrying who, which session, which
// class, which model, the tokens and the wait — on success AND on refusal,
// and for cligw-served requests as much as for the local device.
func TestAuditRecordsCligwSuccessAndRefusal(t *testing.T) {
	h := newHarness(t, nil)
	hdr := map[string]string{SessionHeader: "s-audit", ClassHeader: "batch"}

	resp, out := h.do("POST", "/v1/chat/completions", chat("L4"), hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("cligw success: %d %v", resp.StatusCode, out)
	}
	ok := h.lastRecord(1)
	if ok.Principal != "owner" || ok.Session != "s-audit" || ok.Class != "batch" || ok.Backend != BackendCLI ||
		ok.Model != "L4" || ok.Status != 200 || ok.PromptTok != 2 || ok.OutputTok != 1 {
		t.Fatalf("cligw success record: %+v", ok)
	}

	resp, _ = h.do("POST", "/v1/chat/completions", chat("busy"), hdr)
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("cligw refusal: %d", resp.StatusCode)
	}
	busy := h.lastRecord(2)
	if busy.Principal != "owner" || busy.Session != "s-audit" || busy.Class != "batch" || busy.Backend != BackendCLI ||
		busy.Model != "busy" || busy.Status != http.StatusTooManyRequests || busy.Error == "" {
		t.Fatalf("cligw refusal record: %+v", busy)
	}

	// cligw's own surface through the door is audited too.
	resp, _ = h.do("GET", "/cligw/health", nil, hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("cligw health: %d", resp.StatusCode)
	}
	if r := h.lastRecord(3); r.Principal != "owner" || r.Session != "s-audit" || r.Path != "/cligw/health" || r.Status != 200 {
		t.Fatalf("cligw surface record: %+v", r)
	}

	// An unauthenticated caller is refused and still audited, without a
	// principal it never proved.
	resp, _ = h.do("POST", "/v1/chat/completions", chat("L4"), map[string]string{"Authorization": "Bearer nope"})
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: %d", resp.StatusCode)
	}
	if r := h.lastRecord(4); r.Status != http.StatusUnauthorized || r.Principal != "" || r.Error == "" {
		t.Fatalf("unauthenticated record: %+v", r)
	}
	if n := len(h.b.audit.snapshot()); n != 4 {
		t.Fatalf("records = %d, want one per request (4)", n)
	}
}

// A shared peer is a different principal on the one physical engine: the
// owner's resident models (and with them every prefix/KV cache) are evicted
// before the peer's work runs, and again before the owner's next turn.
func TestPeerPrincipalEvictsOwnerResidentModels(t *testing.T) {
	h, _, peers := peerHarness(t)
	cred, err := MintPeerCredential(peers, "codexhost", ShareAllow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	local := func(token string) int {
		req, _ := http.NewRequest("POST", h.server.URL+"/v1/chat/completions",
			strings.NewReader(`{"model":"llama3.2:3b","messages":[{"role":"user","content":"private prefix"}]}`))
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set(SessionHeader, "s-shared")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 || resp.Header.Get(BackendHeader) != BackendLocal {
			t.Fatalf("local request: %d backend=%q", resp.StatusCode, resp.Header.Get(BackendHeader))
		}
		return len(h.eng.unloadedList())
	}
	owner := local(testToken)
	again := local(testToken)
	if again != owner {
		t.Fatalf("same principal+session evicted its own cache: %d -> %d", owner, again)
	}
	peer := local(cred)
	if peer <= again {
		t.Fatal("peer:codexhost ran on the owner's resident model without an eviction")
	}
	if back := local(testToken); back <= peer {
		t.Fatal("owner ran on the peer's resident model without an eviction")
	}
	recs := h.b.audit.snapshot()
	if got := recs[len(recs)-2].Principal; got != "peer:codexhost" {
		t.Fatalf("peer turn audited as %q", got)
	}
}

// A peer naming the owner's sticky key in the owner's session reaches a
// binding of its own (or none) — never the owner's context, transcript or
// use counter.
func TestPeerCannotReachOwnerStickyContext(t *testing.T) {
	h, _, peers := peerHarness(t)
	cred, err := MintPeerCredential(peers, "codexhost", ShareAllow, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string]string{SessionHeader: "s-shared", StickyHeader: "context; uses=0"}
	resp, out := h.do("POST", "/v1/chat/completions", chat("llama3.2:3b"), hdr)
	if resp.StatusCode != 200 {
		t.Fatalf("owner sticky: %d %v", resp.StatusCode, out)
	}
	ownerBinding := h.b.sticky.get("owner", "s-shared", "context")
	if ownerBinding == nil || ownerBinding.Used != 1 {
		t.Fatalf("owner binding: %+v", ownerBinding)
	}

	// The key alone (URL form, no spec) must not resolve to the owner's binding.
	code, body := peerPost(t, h, "/s/s-shared/sticky/context/v1/chat/completions", cred, nil)
	if code != http.StatusNotFound || strings.Contains(body, ownerBinding.Digest) {
		t.Fatalf("peer resolved the owner's key: %d %s", code, body)
	}
	// With a spec the peer gets a binding of its own, in its own scope.
	if code, body = peerPost(t, h, "/v1/chat/completions", cred, map[string]string{SessionHeader: "s-shared", StickyHeader: "context; uses=0"}); code != 200 {
		t.Fatalf("peer's own sticky: %d %s", code, body)
	}
	if mine := h.b.sticky.get("peer:codexhost", "s-shared", "context"); mine == nil || mine.Principal != "peer:codexhost" || mine.Used != 1 {
		t.Fatalf("peer binding: %+v", mine)
	}
	if after := h.b.sticky.get("owner", "s-shared", "context"); after == nil || after.Used != 1 || len(after.Transcript) != len(ownerBinding.Transcript) {
		t.Fatalf("peer touched the owner's binding: %+v", after)
	}
	if h.b.sticky.findDigest("peer:codexhost", "s-shared", ownerBinding.Digest) != nil {
		t.Fatal("owner's context digest visible to the peer")
	}
}
