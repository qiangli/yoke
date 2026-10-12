package feed

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// indexProbe is an httptest registry that records every push.
type indexProbe struct {
	t      *testing.T
	mu     sync.Mutex
	auths  []string
	raws   []string
	status int
	body   string
}

func newIndexProbe(t *testing.T, status int, body string) (*indexProbe, *httptest.Server) {
	t.Helper()
	p := &indexProbe{t: t, status: status, body: body}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		p.mu.Lock()
		p.auths = append(p.auths, r.Header.Get("Authorization"))
		p.raws = append(p.raws, string(raw))
		p.mu.Unlock()
		if r.Method != http.MethodPost || r.URL.Path != "/api/v1/feed/registry" {
			t.Errorf("registry got %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(p.status)
		_, _ = w.Write([]byte(p.body))
	}))
	t.Cleanup(srv.Close)
	return p, srv
}

func (p *indexProbe) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.raws)
}

func (p *indexProbe) last() (auth, raw string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.auths[len(p.auths)-1], p.raws[len(p.raws)-1]
}

// decodeLast unmarshals the most recent push into the wire struct and into a
// generic map (the map catches stray keys the struct would ignore).
func (p *indexProbe) decodeLast(t *testing.T) (IndexRequest, map[string]any) {
	t.Helper()
	_, raw := p.last()
	var req IndexRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatalf("push is not the wire JSON: %v\n%s", err, raw)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatalf("push is not a JSON object: %v", err)
	}
	return req, m
}

func hasKey(m map[string]any, key string) bool {
	for k, v := range m {
		if k == key {
			return true
		}
		if sub, ok := v.(map[string]any); ok && hasKey(sub, key) {
			return true
		}
	}
	return false
}

func postToProbe(t *testing.T, p *indexProbe, srv *httptest.Server, extraArgs ...string) (stdout, stderr string) {
	t.Helper()
	ex := &fakeExcerpter{answers: []Excerpt{goodExcerpt}}
	te := newTestEnv(t, ex)
	te.env.Cloud = func() (string, string) { return srv.URL, "probe-credential" }
	sinkDir := t.TempDir()
	te.mustRun("sink", "add", "rclone", "local", sinkDir)
	args := append([]string{"post", writeBody(t, "body text that must never leave the host seven"),
		"--headline", "Seven", "--dek", "A dek.", "--label", "l1"}, extraArgs...)
	o, e, err := te.run(args...)
	if err != nil {
		t.Fatalf("post: %v\nstdout: %s\nstderr: %s", err, o, e)
	}
	if p.count() == 0 {
		t.Fatal("registry got no push")
	}
	if auth, _ := p.last(); auth != "Bearer "+"probe-credential" {
		t.Fatalf("auth header = %q", auth)
	}
	return o, e
}

func TestIndexPostOk(t *testing.T) {
	p, srv := newIndexProbe(t, 200, `{"indexed":"ok","uid":"uid123","viewer":"/u/uid123/posts/2026/x.md","expires_at":"2026-04-11T00:00:00Z"}`)
	_, stderr := postToProbe(t, p, srv)
	if !strings.Contains(stderr, "index: ok /u/uid123/posts/2026/x.md") {
		t.Fatalf("stderr = %q", stderr)
	}

	req, m := p.decodeLast(t)
	if req.Pubkey == "" || req.ForceKey {
		t.Fatalf("pubkey/force_key = %q/%v", req.Pubkey, req.ForceKey)
	}
	if len(req.SinkBases) != 1 || !strings.HasSuffix(req.SinkBases[0], "/") {
		t.Fatalf("sink_bases = %q", req.SinkBases)
	}
	post := req.Post
	if post.ID == "" || post.Path == "" || post.Sig == "" || post.Headline != "Seven" ||
		post.Dek != "A dek." || len(post.Labels) != 1 || post.Labels[0] != "l1" ||
		len(post.URLs) != 1 || post.Created == "" {
		t.Fatalf("post row = %+v", post)
	}
	if want := strings.TrimSuffix(post.URLs[0], post.Path); req.SinkBases[0] != want {
		t.Fatalf("sink_bases[0] = %q, want URL minus path %q", req.SinkBases[0], want)
	}

	// file_sha256 is over the exact bytes on the sink.
	file := post.URLs[0] // dirSink returns the file path as the URL
	data, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	if post.FileSHA256 != hex.EncodeToString(sum[:]) {
		t.Fatalf("file_sha256 = %s, want sha256 of sink bytes", post.FileSHA256)
	}
	parsed, err := ParsePost(data)
	if err != nil {
		t.Fatal(err)
	}
	if post.BodySHA256 != parsed.BodySHA256() {
		t.Fatalf("body_sha256 = %s, want %s", post.BodySHA256, parsed.BodySHA256())
	}

	// The body itself must never be on the wire.
	if hasKey(m, "body") {
		t.Fatalf("push contains a body key: %v", m)
	}
	for _, r := range p.raws {
		if strings.Contains(r, "must never leave the host") {
			t.Fatalf("push contains the post body: %s", r)
		}
	}

	// The accepted pubkey is remembered for this base.
	keys, err := loadRegistryKeys(KeyDir())
	if err != nil {
		t.Fatal(err)
	}
	if keys[srv.URL] != req.Pubkey {
		t.Fatalf("remembered keys = %v", keys)
	}
}

func TestIndexPostDeferred(t *testing.T) {
	p, srv := newIndexProbe(t, 200, `{"indexed":"deferred","reason":"over quota until tomorrow"}`)
	_, stderr := postToProbe(t, p, srv)
	if !strings.Contains(stderr, "index: deferred (over quota until tomorrow)") {
		t.Fatalf("stderr = %q", stderr)
	}
	// Deferred means nothing was stored server-side, so nothing is recorded.
	if _, err := os.Stat(filepath.Join(KeyDir(), registryKeysFile)); !os.IsNotExist(err) {
		t.Fatalf("registry keys recorded on deferred: %v", err)
	}
}

func TestIndexPostConflictPointsAtForceKey(t *testing.T) {
	p, srv := newIndexProbe(t, 409, `{"reason":"another key is registered"}`)
	_, stderr := postToProbe(t, p, srv)
	if !strings.Contains(stderr, "--force-key") {
		t.Fatalf("conflict message lacks --force-key: %q", stderr)
	}

	// ... and --force-key sets force_key:true on the next push.
	_, _ = postToProbe(t, p, srv, "--force-key")
	req, _ := p.decodeLast(t)
	if !req.ForceKey {
		t.Fatalf("force_key not set: %+v", req)
	}
}

func TestIndexPostRefusedPrintsServerReason(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		reason string
	}{
		{410, `{"reason":"delisted: court order"}`, "delisted: court order"},
		{422, `{"reason":"sink bytes do not hash to file_sha256"}`, "sink bytes do not hash to file_sha256"},
	} {
		p, srv := newIndexProbe(t, tc.status, tc.body)
		_, stderr := postToProbe(t, p, srv)
		if !strings.Contains(stderr, "index: refused: "+tc.reason) {
			t.Fatalf("HTTP %d: stderr = %q", tc.status, stderr)
		}
	}
}

func TestIndexPostServerErrorWarnsButExits0(t *testing.T) {
	p, srv := newIndexProbe(t, 500, `oops`)
	_, stderr := postToProbe(t, p, srv)
	if !strings.Contains(stderr, "index: failed:") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestIndexPostUnpairedSkips(t *testing.T) {
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	// newTestEnv leaves Cloud unpaired.
	te.mustRun("sink", "add", "rclone", "local", t.TempDir())
	_, stderr, err := te.run("post", writeBody(t, "unpaired body"), "--headline", "H", "--dek", "D")
	if err != nil {
		t.Fatalf("unpaired post must exit 0: %v", err)
	}
	if !strings.Contains(stderr, "index: skipped (host not paired)") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestIndexPostServerDownWarnsButExits0(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close() // nothing listens now
	te := newTestEnv(t, &fakeExcerpter{answers: []Excerpt{goodExcerpt}})
	te.env.Cloud = func() (string, string) { return url, "probe-credential" }
	te.mustRun("sink", "add", "rclone", "local", t.TempDir())
	_, stderr, err := te.run("post", writeBody(t, "offline body"), "--headline", "H", "--dek", "D")
	if err != nil {
		t.Fatalf("post with the registry down must exit 0: %v", err)
	}
	if !strings.Contains(stderr, "index: failed:") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestIndexPostNoIndexFlag(t *testing.T) {
	p, srv := newIndexProbe(t, 200, `{"indexed":"ok","viewer":"/u/x"}`)
	ex := &fakeExcerpter{answers: []Excerpt{goodExcerpt}}
	te := newTestEnv(t, ex)
	te.env.Cloud = func() (string, string) { return srv.URL, "probe-credential" }
	te.mustRun("sink", "add", "rclone", "local", t.TempDir())
	o, e, err := te.run("post", writeBody(t, "quiet body"), "--headline", "H", "--dek", "D", "--no-index")
	if err != nil {
		t.Fatalf("post --no-index: %v", err)
	}
	if p.count() != 0 {
		t.Fatalf("registry got %d pushes", p.count())
	}
	if strings.Contains(o+e, "index:") {
		t.Fatalf("index output despite --no-index: %q %q", o, e)
	}
}

func TestSinkBase(t *testing.T) {
	for _, tc := range []struct{ url, rel, want string }{
		{"https://raw.example.com/o/r/main/posts/2026/id.md", "posts/2026/id.md", "https://raw.example.com/o/r/main/"},
		{"https://raw.example.com/o/r/main/", "posts/2026/id.md", "https://raw.example.com/o/r/main/"},
		{"/tmp/sink/posts/2026/id.md", "posts/2026/id.md", "/tmp/sink/"},
		{"https://example.com/other.md", "posts/id.md", "https://example.com/other.md/"},
	} {
		if got := SinkBase(tc.url, tc.rel); got != tc.want {
			t.Errorf("SinkBase(%q, %q) = %q, want %q", tc.url, tc.rel, got, tc.want)
		}
	}
}
