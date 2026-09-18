package fetchcmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

func runFetch(t *testing.T, stdin string, args ...string) (out, errOut string, code int) {
	t.Helper()
	var o, e bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Stdio: tool.Stdio{In: strings.NewReader(stdin), Out: &o, Err: &e},
	}
	code = cmd.Run(rc, args)
	return o.String(), e.String(), code
}

func testServer(t *testing.T) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/text", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "hello world")
	})
	mux.HandleFunc("/html", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		io.WriteString(w, "<h1>Title</h1><p>Some <b>bold</b> text.</p>")
	})
	mux.HandleFunc("/echo", func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen-Method", r.Method)
		w.Header().Set("X-Seen-Auth", r.Header.Get("Authorization"))
		w.Header().Set("X-Seen-Custom", r.Header.Get("X-Custom"))
		io.WriteString(w, string(body))
	})
	mux.HandleFunc("/missing", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	s := httptest.NewServer(mux)
	t.Cleanup(s.Close)
	return s
}

func TestFetchGetText(t *testing.T) {
	s := testServer(t)
	out, _, code := runFetch(t, "", s.URL+"/text")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if strings.TrimSpace(out) != "hello world" {
		t.Errorf("body = %q, want hello world", out)
	}
}

func TestFetchJSONEnvelope(t *testing.T) {
	s := testServer(t)
	out, _, code := runFetch(t, "", "--json", s.URL+"/text")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatalf("not JSON: %v (%q)", err, out)
	}
	if env["schema_version"] != fetchSchemaVersion {
		t.Errorf("schema = %v", env["schema_version"])
	}
	if env["status_code"].(float64) != 200 || env["body"] != "hello world" {
		t.Errorf("envelope = %+v", env)
	}
}

func TestFetchHeadersAuthAndPost(t *testing.T) {
	s := testServer(t)
	// -d implies POST; -H and -t set request headers the echo server reflects.
	out, _, code := runFetch(t, "", "--json", "-H", "X-Custom: abc", "-t", "tok123",
		"-d", "payload", s.URL+"/echo")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	var env map[string]any
	json.Unmarshal([]byte(out), &env)
	if env["body"] != "payload" {
		t.Errorf("echoed body = %v, want payload", env["body"])
	}
	hdr := env["headers"].(map[string]any)
	if hdr["X-Seen-Method"] != "POST" {
		t.Errorf("method = %v, want POST (implied by -d)", hdr["X-Seen-Method"])
	}
	if hdr["X-Seen-Custom"] != "abc" {
		t.Errorf("custom header not sent: %v", hdr["X-Seen-Custom"])
	}
	if hdr["X-Seen-Auth"] != "Bearer tok123" {
		t.Errorf("bearer token not sent: %v", hdr["X-Seen-Auth"])
	}
}

func TestFetchMarkdown(t *testing.T) {
	s := testServer(t)
	out, _, code := runFetch(t, "", "--md", s.URL+"/html")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	// h2m renders <h1> as "# Title" and <b> as **bold**.
	if !strings.Contains(out, "# Title") || !strings.Contains(out, "**bold**") {
		t.Errorf("markdown conversion failed:\n%s", out)
	}
}

func TestFetchFailFlag(t *testing.T) {
	s := testServer(t)
	// Without -f, a 404 is still exit 0 (body delivered).
	if _, _, code := runFetch(t, "", s.URL+"/missing"); code != 0 {
		t.Errorf("404 without -f should be exit 0, got %d", code)
	}
	// With -f, status >= 400 exits non-zero.
	if _, _, code := runFetch(t, "", "-f", s.URL+"/missing"); code == 0 {
		t.Error("-f on 404 should exit non-zero")
	}
}

func TestFetchDataFromStdin(t *testing.T) {
	s := testServer(t)
	out, _, _ := runFetch(t, "from-stdin", "--json", "-d", "@-", s.URL+"/echo")
	var env map[string]any
	json.Unmarshal([]byte(out), &env)
	if env["body"] != "from-stdin" {
		t.Errorf("stdin body = %v, want from-stdin", env["body"])
	}
}
