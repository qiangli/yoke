package feed

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// viewerServer serves GET /api/v1/feed/read with the given read set, inbox
// and viewers map, counting requests.
func viewerServer(t *testing.T, read []string, inbox string, viewers map[string]string, calls *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*calls++
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		if r.URL.Path != "/api/v1/feed/read" {
			t.Errorf("expected /api/v1/feed/read, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, "Bearer ") || len(got) <= len("Bearer ") {
			t.Errorf("expected a Bearer authorization header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		json.NewEncoder(w).Encode(ServerReadResponse{
			Read:    read,
			Inbox:   inbox,
			Viewers: viewers,
		})
	}))
}

// defaultViewerWiring points the env's default server wiring at srv.
func defaultViewerWiring(te *testEnv, srv *httptest.Server, token string) {
	te.env.Cloud = func() (string, string) { return srv.URL, token }
	te.env.serverRead = nil
	te.env.inboxURL = nil
}

func TestDeliverViewerLinkReplacesSinkLink(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	var calls int
	const viewer1 = "https://cloud.example/u/7/posts/p1"
	srv := viewerServer(t, nil, testInbox, map[string]string{"p1": viewer1}, &calls)
	defer srv.Close()
	defaultViewerWiring(te, srv, "viewer-token")

	addChannel(te, "c")
	dlvSpool(t, te.env.dir(),
		dlvPost{"p1", "alice", "First post"},
		dlvPost{"p2", "bob", "Second post"})

	out := te.mustRun("deliver", "--dry-run")
	if calls != 1 {
		t.Errorf("expected one shared server round trip for read set, inbox and viewers, got %d calls", calls)
	}
	// p1 has a viewer URL: the message links to the viewer, not the sink.
	if !strings.Contains(out, "First post "+viewer1) {
		t.Errorf("message lacks the viewer link:\n%s", out)
	}
	if strings.Contains(out, "https://feed.example/alice/posts/p1.md") {
		t.Errorf("message still links p1 to the raw sink file:\n%s", out)
	}
	// p2 is absent from viewers: the sink link stands.
	if !strings.Contains(out, "Second post https://feed.example/alice/posts/p2.md") {
		t.Errorf("message lacks the p2 sink link:\n%s", out)
	}
	if n := len(fx.calls(t)); n != 0 {
		t.Fatalf("dry-run ran the command %d times", n)
	}
}

func TestDeliverViewerUnpairedKeepsSinkLinks(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected HTTP call when unpaired: %s %s", r.Method, r.URL.Path)
	}))
	defer srv.Close()
	// Empty token means unpaired: the server must not be contacted.
	defaultViewerWiring(te, srv, "")

	addChannel(te, "c")
	dlvSpool(t, te.env.dir(), dlvPost{"p1", "alice", "First post"})

	out, stderr, err := te.run("deliver", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "First post https://feed.example/alice/posts/p1.md") {
		t.Errorf("unpaired deliver changed the sink link:\n%s", out)
	}
	if stderr != "" {
		t.Errorf("expected no warning when unpaired, got %q", stderr)
	}
	if n := len(fx.calls(t)); n != 0 {
		t.Fatalf("dry-run ran the command %d times", n)
	}
}

func TestDeliverViewerServerErrorKeepsSinkLinks(t *testing.T) {
	te := newTestEnv(t, nil)
	fx := newDeliverFixture(t, te)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "server boom", http.StatusInternalServerError)
	}))
	defer srv.Close()
	defaultViewerWiring(te, srv, "viewer-token")

	addChannel(te, "c")
	dlvSpool(t, te.env.dir(), dlvPost{"p1", "alice", "Only post"})

	out, stderr, err := te.run("deliver", "--dry-run")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stderr, "server read set unavailable") {
		t.Errorf("expected a warning about the unavailable server set, got %q", stderr)
	}
	if !strings.Contains(out, "Only post https://feed.example/alice/posts/p1.md") {
		t.Errorf("server error changed the sink link:\n%s", out)
	}
	if n := len(fx.calls(t)); n != 0 {
		t.Fatalf("dry-run ran the command %d times", n)
	}
}
