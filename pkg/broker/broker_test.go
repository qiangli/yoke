package broker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "tok-test"

// fakeEngine imitates the Ollama API surface the broker uses.
type fakeEngine struct {
	mu       sync.Mutex
	models   map[string]engineModel
	inflight atomic.Int32
	maxSeen  atomic.Int32
	hold     chan struct{} // when non-nil, generations block until closed
	started  chan string   // receives the model of each generation as it starts
	created  []string
	unloaded []string
	lastBody map[string]any
	server   *httptest.Server
}

func newFakeEngine(t *testing.T) *fakeEngine {
	t.Helper()
	f := &fakeEngine{models: map[string]engineModel{
		"llama3.2:3b": {Name: "llama3.2:3b", Size: 2 << 30, Digest: "d-llama"},
		"qwen3:8b":    {Name: "qwen3:8b", Size: 5 << 30, Digest: "d-qwen"},
		"huge:1t":     {Name: "huge:1t", Size: 900 << 30, Digest: "d-huge"},
	}}
	f.server = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.server.Close)
	return f
}

func (f *fakeEngine) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/api/version":
		io.WriteString(w, `{"version":"fake"}`)
	case "/api/tags":
		f.mu.Lock()
		var list []engineModel
		for _, m := range f.models {
			list = append(list, m)
		}
		f.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]any{"models": list})
	case "/api/ps":
		json.NewEncoder(w).Encode(map[string]any{"models": []engineModel{{Name: "llama3.2:3b"}}})
	case "/api/create":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		name, _ := body["model"].(string)
		f.mu.Lock()
		f.created = append(f.created, name)
		f.models[name] = engineModel{Name: name, Digest: "d-derived"}
		f.mu.Unlock()
		io.WriteString(w, `{"status":"success"}`)
	case "/api/generate", "/api/chat", "/v1/chat/completions":
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if ka, ok := body["keep_alive"]; ok && ka == float64(0) {
			f.mu.Lock()
			f.unloaded = append(f.unloaded, body["model"].(string))
			f.mu.Unlock()
			io.WriteString(w, `{"done":true}`)
			return
		}
		f.mu.Lock()
		f.lastBody = body
		hold := f.hold
		f.mu.Unlock()
		n := f.inflight.Add(1)
		for {
			m := f.maxSeen.Load()
			if n <= m || f.maxSeen.CompareAndSwap(m, n) {
				break
			}
		}
		if f.started != nil {
			f.started <- body["model"].(string)
		}
		if hold != nil {
			<-hold
		}
		f.inflight.Add(-1)
		if r.URL.Path == "/v1/chat/completions" {
			io.WriteString(w, `{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
			return
		}
		io.WriteString(w, `{"response":"ok","done":true,"prompt_eval_count":11,"eval_count":5}`)
	default:
		http.NotFound(w, r)
	}
}

func (f *fakeEngine) body() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastBody
}

func (f *fakeEngine) createdList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.created...)
}

func (f *fakeEngine) unloadedList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.unloaded...)
}

// lastRecord waits for the n-th run record (records land just after the
// response has been written).
func (h *harness) lastRecord(n int) Record {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if recs := h.b.audit.snapshot(); len(recs) >= n {
			return recs[len(recs)-1]
		}
		time.Sleep(2 * time.Millisecond)
	}
	h.t.Fatalf("no run record #%d", n)
	return Record{}
}

// fakeCLI imitates cligw.
type fakeCLI struct {
	mu          sync.Mutex
	resolved    int
	lastAuth    string
	lastBody    map[string]any
	sticky      *fakeStickySession
	stickyAgent string
	stickyErr   error
	dials       int
}

func (c *fakeCLI) snap() (string, map[string]any, int) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastAuth, c.lastBody, c.resolved
}

func (c *fakeCLI) Token() string { return "cli-token" }

func (c *fakeCLI) ResolveAgent(_ context.Context, model, _ string) (AgentInfo, error) {
	c.mu.Lock()
	c.resolved++
	c.mu.Unlock()
	return AgentInfo{Name: "claude-opus5", Tool: "claude", Model: "opus5", VendorModel: "claude-opus-5", Provider: "anthropic", Warm: "stdin-stream-json", Band: 5}, nil
}

func (c *fakeCLI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	c.mu.Lock()
	c.lastAuth = r.Header.Get("Authorization")
	if r.Method == http.MethodPost {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		c.lastBody = body
	}
	c.mu.Unlock()
	switch r.URL.Path {
	case "/v1/models":
		io.WriteString(w, `{"object":"list","data":[{"id":"L4","object":"model"}]}`)
	case "/health":
		io.WriteString(w, `{"schema_version":"bashy-cligw-health-v1"}`)
	default:
		w.Header().Set("X-Bashy-Routed", "agent=claude-opus5, band=L5, reason=test")
		io.WriteString(w, `{"choices":[{"message":{"content":"cli"}}],"usage":{"prompt_tokens":2,"completion_tokens":1}}`)
	}
}

type harness struct {
	t      *testing.T
	b      *Broker
	eng    *fakeEngine
	cli    *fakeCLI
	server *httptest.Server
	tool   atomic.Value
}

func newHarness(t *testing.T, mod func(*Options)) *harness {
	t.Helper()
	h := &harness{t: t, eng: newFakeEngine(t), cli: &fakeCLI{}}
	h.tool.Store("2.1.0")
	opts := Options{
		Engine:      StaticEngine(h.eng.server.URL),
		CLI:         h.cli,
		Token:       testToken,
		MemoryBytes: 64 << 30,
		StateDir:    t.TempDir(),
		ToolVersion: func(string) string { return h.tool.Load().(string) },
	}
	if mod != nil {
		mod(&opts)
	}
	b, err := New(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	h.b = b
	h.server = httptest.NewServer(b)
	t.Cleanup(h.server.Close)
	return h
}

func (h *harness) do(method, path string, body any, hdr map[string]string) (*http.Response, map[string]any) {
	h.t.Helper()
	var rd io.Reader
	if body != nil {
		data, _ := json.Marshal(body)
		rd = bytes.NewReader(data)
	}
	req, _ := http.NewRequest(method, h.server.URL+path, rd)
	req.Header.Set("Authorization", "Bearer "+testToken)
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		h.t.Fatal(err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	var out map[string]any
	_ = json.Unmarshal(data, &out)
	return resp, out
}

func chat(model string) map[string]any {
	return map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": "hi"}}}
}

func TestAuth(t *testing.T) {
	h := newHarness(t, nil)
	resp, _ := h.do("GET", "/v1/models", nil, map[string]string{"Authorization": ""})
	if resp.StatusCode != 401 {
		t.Fatalf("no token: %d", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/v1/models", nil, map[string]string{"Authorization": "Bearer wrong"})
	if resp.StatusCode != 401 {
		t.Fatalf("bad token: %d", resp.StatusCode)
	}
	resp, _ = h.do("GET", "/k/"+testToken+"/v1/models", nil, map[string]string{"Authorization": ""})
	if resp.StatusCode != 200 {
		t.Fatalf("path token: %d", resp.StatusCode)
	}
}

func TestUnixSocketIsOwner(t *testing.T) {
	h := newHarness(t, nil)
	sock := filepath.Join(t.TempDir(), "b.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skip("unix sockets unavailable:", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.b.Serve(ctx, ln)
	client := http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "unix", sock)
	}}}
	resp, err := client.Get("http://door/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("socket without token: %d", resp.StatusCode)
	}
}

func TestLocalChatRecordsModelDigestAndTokens(t *testing.T) {
	h := newHarness(t, nil)
	resp, out := h.do("POST", "/api/chat", chat("llama3.2:3b"), nil)
	if resp.StatusCode != 200 || out["response"] != "ok" {
		t.Fatalf("status %d body %v", resp.StatusCode, out)
	}
	if resp.Header.Get(BackendHeader) != BackendLocal {
		t.Fatalf("backend header %q", resp.Header.Get(BackendHeader))
	}
	last := h.lastRecord(1)
	if last.ModelDigest != "d-llama" || last.PromptTok != 11 || last.OutputTok != 5 || last.Class != "interactive" {
		t.Fatalf("record %+v", last)
	}
}

func TestDeviceIsExclusive(t *testing.T) {
	h := newHarness(t, nil)
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.do("POST", "/v1/chat/completions", chat("llama3.2:3b"), nil)
		}()
	}
	wg.Wait()
	if got := h.eng.maxSeen.Load(); got != 1 {
		t.Fatalf("engine saw %d concurrent generations, want 1", got)
	}
}

func TestPriorityClassesAndQueueFull(t *testing.T) {
	h := newHarness(t, func(o *Options) { o.QueueLimit = 2 })
	h.eng.hold = make(chan struct{})
	h.eng.started = make(chan string, 8)
	order := make(chan string, 4)
	send := func(class, tag string) {
		body := chat("llama3.2:3b")
		body["messages"] = []map[string]string{{"role": "user", "content": tag}}
		resp, _ := h.do("POST", "/api/chat", body, map[string]string{ClassHeader: class})
		order <- tag + ":" + resp.Status[:3]
	}
	go send("batch", "first")
	<-h.eng.started // device busy
	go send("batch", "batch")
	waitQueued(t, h.b.device, 1)
	go send("interactive", "interactive")
	waitQueued(t, h.b.device, 2)
	resp, out := h.do("POST", "/api/chat", chat("llama3.2:3b"), nil)
	if resp.StatusCode != 503 || resp.Header.Get("X-Bashy-Refusal") != "queue-full" {
		t.Fatalf("queue full: %d %v", resp.StatusCode, out)
	}
	close(h.eng.hold)
	got := []string{<-order, <-order, <-order}
	if got[0] != "first:200" || got[1] != "interactive:200" || got[2] != "batch:200" {
		t.Fatalf("served order %v", got)
	}
}

func waitQueued(t *testing.T, d *Device, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		s := d.Stats()
		total := 0
		for _, v := range s.Queued {
			total += v
		}
		if total >= n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("queue never reached %d", n)
}

func TestMemoryRefusal(t *testing.T) {
	h := newHarness(t, nil)
	resp, out := h.do("POST", "/api/generate", map[string]any{"model": "huge:1t", "prompt": "x"}, nil)
	if resp.StatusCode != 503 || resp.Header.Get("X-Bashy-Refusal") != "memory" {
		t.Fatalf("got %d %v", resp.StatusCode, out)
	}
	if !strings.Contains(out["error"].(string), "insufficient memory") {
		t.Fatalf("reason %v", out)
	}
}

func TestNonLocalModelGoesToCLIWithItsToken(t *testing.T) {
	h := newHarness(t, nil)
	resp, out := h.do("POST", "/v1/chat/completions", chat("L4"), nil)
	if resp.StatusCode != 200 || resp.Header.Get("X-Bashy-Routed") == "" {
		t.Fatalf("cli: %d %v", resp.StatusCode, out)
	}
	if auth, _, _ := h.cli.snap(); auth != "Bearer cli-token" {
		t.Fatalf("cli saw auth %q", auth)
	}
	last := h.lastRecord(1)
	if last.Backend != BackendCLI || !strings.Contains(last.Routed, "claude-opus5") {
		t.Fatalf("record %+v", last)
	}
	// Native API never falls through to a CLI.
	resp, _ = h.do("POST", "/api/chat", chat("L4"), nil)
	if resp.StatusCode != 404 {
		t.Fatalf("native non-local: %d", resp.StatusCode)
	}
}

func TestModelsMergesLocalAndCLI(t *testing.T) {
	h := newHarness(t, nil)
	_, out := h.do("GET", "/v1/models", nil, nil)
	data := out["data"].([]any)
	ids := map[string]bool{}
	for _, d := range data {
		ids[d.(map[string]any)["id"].(string)] = true
	}
	if !ids["llama3.2:3b"] || !ids["L4"] {
		t.Fatalf("ids %v", ids)
	}
}

func TestStickyUsesAndIdentityHeader(t *testing.T) {
	h := newHarness(t, nil)
	resp, out := h.do("POST", "/v1/sticky", StickySpec{Key: "run1", Model: "llama3.2:3b", Uses: 2,
		Options: map[string]any{"temperature": 0, "seed": 1}}, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("create %d %v", resp.StatusCode, out)
	}
	digest := out["digest"].(string)
	for i := 1; i <= 2; i++ {
		resp, _ := h.do("POST", "/sticky/run1/v1/chat/completions", chat("ignored"), nil)
		if resp.StatusCode != 200 {
			t.Fatalf("use %d: %d", i, resp.StatusCode)
		}
		if resp.Header.Get(IdentityHeader) != digest {
			t.Fatalf("identity header %q want %q", resp.Header.Get(IdentityHeader), digest)
		}
		if !strings.Contains(resp.Header.Get(StickyHeader), "use="+string(rune('0'+i))+"/2") {
			t.Fatalf("sticky header %q", resp.Header.Get(StickyHeader))
		}
	}
	if h.eng.body()["model"] != "llama3.2:3b" || h.eng.body()["temperature"] != float64(0) {
		t.Fatalf("engine saw %v", h.eng.lastBody)
	}
	resp, out = h.do("POST", "/sticky/run1/v1/chat/completions", chat("x"), nil)
	if resp.StatusCode != 409 || !strings.Contains(out["error"].(map[string]any)["message"].(string), "exhausted") {
		t.Fatalf("exhausted: %d %v", resp.StatusCode, out)
	}
}

func TestStickyNumCtxUsesDerivedModel(t *testing.T) {
	h := newHarness(t, nil)
	h.do("POST", "/v1/sticky", StickySpec{Key: "ctx", Model: "qwen3:8b", Options: map[string]any{"num_ctx": 65536}}, nil)
	for i := 0; i < 2; i++ {
		resp, _ := h.do("POST", "/sticky/ctx/v1/chat/completions", chat("x"), nil)
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
	}
	if len(h.eng.createdList()) != 1 || !strings.HasPrefix(h.eng.createdList()[0], "qwen3:8b-bashy-") {
		t.Fatalf("created %v", h.eng.createdList())
	}
	if h.eng.body()["model"] != h.eng.createdList()[0] {
		t.Fatalf("engine model %v", h.eng.body()["model"])
	}
	// Native API passes num_ctx as an option instead.
	h.do("POST", "/sticky/ctx/api/chat", chat("x"), nil)
	opts := h.eng.body()["options"].(map[string]any)
	if opts["num_ctx"] != float64(65536) || h.eng.body()["model"] != "qwen3:8b" {
		t.Fatalf("native body %v", h.eng.lastBody)
	}
	// Derived models stay out of the listing.
	_, out := h.do("GET", "/v1/models", nil, nil)
	for _, d := range out["data"].([]any) {
		if isDerived(d.(map[string]any)["id"].(string)) {
			t.Fatalf("derived model listed: %v", d)
		}
	}
}

func TestStickyCLIFreezesAgentAndRefusesDrift(t *testing.T) {
	h := newHarness(t, nil)
	resp, out := h.do("POST", "/v1/chat/completions", chat("L5"),
		map[string]string{StickyHeader: "arm-a; uses=0"})
	if resp.StatusCode != 200 {
		t.Fatalf("implicit create: %d %v", resp.StatusCode, out)
	}
	digest := resp.Header.Get(IdentityHeader)
	if _, body, _ := h.cli.snap(); body["model"] != "claude-opus5" {
		t.Fatalf("cli model %v", body["model"])
	}
	// Second arm binds to the same identity by digest alone.
	resp, out = h.do("POST", "/v1/sticky", StickySpec{Key: "arm-b", Identity: ShortDigest(digest)}, nil)
	if resp.StatusCode != 200 || out["digest"] != digest {
		t.Fatalf("arm-b: %d %v", resp.StatusCode, out)
	}
	if _, _, n := h.cli.snap(); n != 1 {
		t.Fatalf("resolved %d times, want once", n)
	}
	// A spec whose identity does not match what the model resolves to.
	resp, _ = h.do("POST", "/v1/sticky", StickySpec{Key: "arm-c", Model: "L5", Identity: "sha256:deadbeef0000"}, nil)
	if resp.StatusCode != 409 {
		t.Fatalf("mismatch: %d", resp.StatusCode)
	}
	// The CLI upgrades: the binding cannot be served exactly any more.
	h.tool.Store("2.2.0")
	resp, _ = h.do("POST", "/sticky/arm-b/v1/chat/completions", chat("x"), nil)
	if resp.StatusCode != 409 {
		t.Fatalf("drift: %d", resp.StatusCode)
	}
	// reset=none on a CLI agent holds one warm worker for the binding: the
	// implicit worker binding serves its turn instead of answering 501.
	h.tool.Store("2.1.0")
	resp, out = h.do("POST", "/v1/chat/completions", chat("L5"), map[string]string{StickyHeader: "mt; bind=worker; reset=none"})
	if resp.StatusCode != 200 {
		t.Fatalf("cli reset=none: %d %v", resp.StatusCode, out)
	}
}

func TestStickyLocalDigestChangeRefused(t *testing.T) {
	h := newHarness(t, nil)
	h.do("POST", "/v1/sticky", StickySpec{Key: "k", Model: "llama3.2:3b"}, nil)
	h.eng.mu.Lock()
	h.eng.models["llama3.2:3b"] = engineModel{Name: "llama3.2:3b", Size: 2 << 30, Digest: "d-new"}
	h.eng.mu.Unlock()
	h.b.refreshTags(context.Background(), true)
	resp, _ := h.do("POST", "/sticky/k/v1/chat/completions", chat("x"), nil)
	if resp.StatusCode != 409 {
		t.Fatalf("got %d", resp.StatusCode)
	}
}

func TestStickyResetNoneTranscript(t *testing.T) {
	h := newHarness(t, nil)
	h.do("POST", "/v1/sticky", StickySpec{Key: "conv", Model: "llama3.2:3b", Bind: BindWorker, Reset: ResetNone}, nil)
	turn := func(msgs ...string) int {
		var m []map[string]string
		for i, s := range msgs {
			role := "user"
			if i%2 == 1 {
				role = "assistant"
			}
			m = append(m, map[string]string{"role": role, "content": s})
		}
		resp, _ := h.do("POST", "/sticky/conv/v1/chat/completions", map[string]any{"model": "x", "messages": m}, nil)
		return resp.StatusCode
	}
	if s := turn("a"); s != 200 {
		t.Fatal(s)
	}
	if s := turn("a", "reply", "b"); s != 200 {
		t.Fatalf("extension refused: %d", s)
	}
	if s := turn("DIFFERENT", "reply", "b", "r2", "c"); s != 409 {
		t.Fatalf("divergence accepted: %d", s)
	}
}

func TestStickyUnknownKeyAndPersistence(t *testing.T) {
	dir := t.TempDir()
	h := newHarness(t, func(o *Options) { o.StateDir = dir })
	resp, _ := h.do("POST", "/sticky/nope/v1/chat/completions", chat("llama3.2:3b"), nil)
	if resp.StatusCode != 404 {
		t.Fatalf("unknown key: %d", resp.StatusCode)
	}
	h.do("POST", "/v1/sticky", StickySpec{Key: "keep", Model: "llama3.2:3b"}, nil)
	h2 := newHarness(t, func(o *Options) { o.StateDir = dir })
	resp, out := h2.do("GET", "/v1/sticky/keep", nil, nil)
	if resp.StatusCode != 200 || out["key"] != "keep" {
		t.Fatalf("after restart: %d %v", resp.StatusCode, out)
	}
	resp, _ = h2.do("DELETE", "/v1/sticky/keep", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatal(resp.StatusCode)
	}
}

func TestStickyKeysAreScopedBySession(t *testing.T) {
	h := newHarness(t, nil)
	parent := NewRootSession()
	child := ExportedView(parent) + "abcd"
	h.do("POST", "/v1/sticky", StickySpec{Key: "private", Model: "llama3.2:3b"}, map[string]string{SessionHeader: parent})
	h.do("POST", "/v1/sticky", StickySpec{Key: "shared", Model: "llama3.2:3b", Export: true}, map[string]string{SessionHeader: parent})
	if resp, _ := h.do("GET", "/v1/sticky/private", nil, map[string]string{SessionHeader: child}); resp.StatusCode != 404 {
		t.Fatalf("child saw a non-exported key: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/sticky/shared", nil, map[string]string{SessionHeader: child}); resp.StatusCode != 200 {
		t.Fatalf("child missed an exported key: %d", resp.StatusCode)
	}
	if resp, _ := h.do("GET", "/v1/sticky/shared", nil, nil); resp.StatusCode != 404 {
		t.Fatalf("no-session request (a gate) saw a session key: %d", resp.StatusCode)
	}
	// URL form carries the session too.
	if resp, _ := h.do("GET", "/s/"+child+"/v1/sticky/shared", nil, nil); resp.StatusCode != 200 {
		t.Fatalf("session in URL: %d", resp.StatusCode)
	}
}

func TestSessionTurnsFollowShellScope(t *testing.T) {
	h := newHarness(t, nil)
	parent := NewRootSession()
	send := func(s string) {
		resp, _ := h.do("POST", "/api/chat", chat("llama3.2:3b"), map[string]string{SessionHeader: s})
		if resp.StatusCode != 200 {
			t.Fatal(resp.StatusCode)
		}
	}
	send(parent)
	time.Sleep(5 * time.Millisecond)
	clone := CloneSession(parent, time.Now())
	time.Sleep(5 * time.Millisecond)
	send(clone)
	child := ExportedView(parent) + "beef"
	count := func(s string) int {
		_, out := h.do("GET", "/v1/sessions/"+s+"/turns", nil, nil)
		turns, _ := out["turns"].([]any)
		return len(turns)
	}
	if n := count(parent); n != 1 {
		t.Fatalf("parent sees %d turns, want its own 1 (the clone's must not flow back)", n)
	}
	if n := count(clone); n != 2 {
		t.Fatalf("clone sees %d turns, want parent's 1 + its own 1", n)
	}
	if n := count(child); n != 0 {
		t.Fatalf("child sees %d turns, want 0 (turns are not exported)", n)
	}
}

func TestSessionLineageRules(t *testing.T) {
	p := "s-aaaaaaaaaaaa"
	t0 := time.UnixMilli(1_000_000)
	clone := p + ".cbbbb@" + "1000000"
	before := scoped{Principal: "o", Session: p, Created: t0.Add(-time.Second)}
	after := scoped{Principal: "o", Session: p, Created: t0.Add(time.Second)}
	if !visible(before, "o", clone) || visible(after, "o", clone) {
		t.Fatal("clone cutoff")
	}
	if visible(scoped{Principal: "o", Session: clone, Created: t0}, "o", p) {
		t.Fatal("clone item flowed back to the parent")
	}
	child := p + "~cccc"
	if visible(before, "o", child) {
		t.Fatal("child saw a non-exported item")
	}
	exp := before
	exp.Exported = true
	if !visible(exp, "o", child) {
		t.Fatal("child missed an exported item")
	}
	if visible(exp, "other", child) {
		t.Fatal("item crossed principals")
	}
	if !visible(scoped{Principal: "o"}, "o", child) || !visible(scoped{Principal: "o"}, "o", "") {
		t.Fatal("principal-scope item should be visible everywhere for its principal")
	}
	if ShellSession("") == "" || !strings.HasPrefix(ShellSession(p+"~"), p+"~") {
		t.Fatal("shell session derivation")
	}
	if ValidateSession("s-ok.c1@2~3") != nil || ValidateSession("bad") == nil || ValidateSession("s-x/y") == nil {
		t.Fatal("validation")
	}
}

func TestPrincipalSwitchEvictsResidentModels(t *testing.T) {
	h := newHarness(t, nil)
	var switched []string
	h.b.device.OnSwitch = func(ctx context.Context, from, to string) error {
		switched = append(switched, from+">"+to)
		return h.b.onPrincipalSwitch(ctx, from, to)
	}
	rel, _, _ := h.b.device.Acquire(context.Background(), ClassInteractive, "alice")
	rel()
	rel, _, _ = h.b.device.Acquire(context.Background(), ClassInteractive, "alice")
	rel()
	rel, _, _ = h.b.device.Acquire(context.Background(), ClassInteractive, "bob")
	rel()
	if len(switched) != 2 || switched[0] != ">alice" || switched[1] != "alice>bob" {
		t.Fatalf("switches %v", switched)
	}
	if len(h.eng.unloadedList()) != 2 || h.eng.unloadedList()[0] != "llama3.2:3b" {
		t.Fatalf("unloaded %v", h.eng.unloadedList())
	}
}

func TestDeviceWaiterCancel(t *testing.T) {
	d := NewDevice(4)
	rel, _, _ := d.Acquire(context.Background(), ClassBatch, "o")
	ctx, cancel := context.WithCancel(context.Background())
	errc := make(chan error, 1)
	go func() {
		_, _, err := d.Acquire(ctx, ClassBatch, "o")
		errc <- err
	}()
	waitQueued(t, d, 1)
	cancel()
	if err := <-errc; err == nil {
		t.Fatal("cancelled waiter got the device")
	}
	rel()
	rel2, _, err := d.Acquire(context.Background(), ClassBatch, "o")
	if err != nil {
		t.Fatal(err)
	}
	rel2()
	if d.Stats().Busy {
		t.Fatal("device left busy")
	}
}

func TestParseStickyHeader(t *testing.T) {
	s, err := ParseStickyHeader("g03; uses=50; bind=identity; reset=each; num_ctx=65536; temperature=0; export")
	if err != nil || s.Key != "g03" || s.Uses != 50 || !s.Export || s.Options["num_ctx"] != float64(65536) {
		t.Fatalf("%+v %v", s, err)
	}
	for _, bad := range []string{"", "k; uses=-1", "k; bind=x", "k; reset=none", "k; bogus=1", "k k"} {
		if _, err := ParseStickyHeader(bad); err == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
	// Header and JSON spellings of the same options produce one digest.
	a := Identity{Backend: BackendLocal, Model: "m", Options: map[string]any{"num_ctx": float64(8)}}
	var bOpts map[string]any
	json.Unmarshal([]byte(`{"num_ctx":8}`), &bOpts)
	b := Identity{Backend: BackendLocal, Model: "m", Options: bOpts}
	if a.Digest() != b.Digest() {
		t.Fatal("digest depends on number spelling")
	}
}

// A binding whose model left the engine is refused with 503, never served by
// another model.
func TestStickyUnavailableIdentityIsRefusedNotRerouted(t *testing.T) {
	h := newHarness(t, nil)
	h.do("POST", "/v1/sticky", StickySpec{Key: "gone", Model: "qwen3:8b"}, nil)
	h.eng.mu.Lock()
	delete(h.eng.models, "qwen3:8b")
	h.eng.mu.Unlock()
	h.b.refreshTags(context.Background(), true)
	before := h.eng.body()
	resp, out := h.do("POST", "/sticky/gone/v1/chat/completions", chat("llama3.2:3b"), nil)
	if resp.StatusCode != 503 || resp.Header.Get("Retry-After") == "" {
		t.Fatalf("got %d %v", resp.StatusCode, out)
	}
	if after := h.eng.body(); after != nil && before == nil {
		t.Fatalf("the engine served something: %v", after)
	}
}
