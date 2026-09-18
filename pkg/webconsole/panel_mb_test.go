// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/bus"
)

// boardInTemp points the board at a scratch directory and seeds it.
//
// Every assertion below runs against a REAL server and a REAL board on disk.
// That is deliberate: the room shipped three UI bugs whose single cause was
// contracts modelled on a mock rather than on the server, and a board panel
// built the same way would repeat all three.
func boardInTemp(t *testing.T, posts ...bus.Post) {
	t.Helper()
	t.Setenv("BASHY_MB_DIR", t.TempDir())
	for _, p := range posts {
		if err := bus.PostMessage(p); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
}

func getJSON(t *testing.T, h http.Handler, path string) map[string]any {
	t.Helper()
	w := do(h, "GET", path, "127.0.0.1:5555", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200 (%s)", path, w.Code, strings.TrimSpace(w.Body.String()))
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: %v", path, err)
	}
	return out
}

func seqs(t *testing.T, d map[string]any) []int {
	t.Helper()
	raw, _ := d["posts"].([]any)
	out := make([]int, 0, len(raw))
	for _, p := range raw {
		m, _ := p.(map[string]any)
		out = append(out, int(m["seq"].(float64)))
	}
	return out
}

func postJSON(h http.Handler, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest("POST", "/api/mb", strings.NewReader(body))
	r.RemoteAddr = "127.0.0.1:5555"
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBoardPageAndFilters(t *testing.T) {
	boardInTemp(t,
		bus.Post{From: "alice", Topic: "posix-cert", Body: "arm is green"},
		bus.Post{From: "bob", To: "alice", Topic: "harness", Body: "gate is red on main"},
		bus.Post{From: "alice", To: "bob", Topic: "mb", Body: "taking the baseline"},
	)
	h := newTestHandler(t, Options{})

	if got := do(h, "GET", "/mb/", "127.0.0.1:5555", nil); got.Code != http.StatusOK ||
		!strings.Contains(got.Body.String(), "mb.js") {
		t.Fatalf("board page = %d, want 200 serving mb.js", got.Code)
	}

	all := getJSON(t, h, "/api/mb")
	if n := len(seqs(t, all)); n != 3 {
		t.Fatalf("unfiltered board has %d posts, want 3", n)
	}
	// Newest first: a scan wants the newest, and trimming before reversing
	// would keep the oldest page.
	if got := seqs(t, all); got[0] != 3 {
		t.Fatalf("posts[0].seq = %d, want the newest (3)", got[0])
	}

	for _, tc := range []struct {
		q    string
		want []int
	}{
		{"from=alice", []int{3, 1}},
		{"topic=harness", []int{2}},
		{"to=bob", []int{3}},
		{"q=baseline", []int{3}},
		{"from=alice&topic=posix-cert", []int{1}},
		{"from=nobody", nil},
	} {
		if got := seqs(t, getJSON(t, h, "/api/mb?"+tc.q)); !equalInts(got, tc.want) {
			t.Errorf("?%s = %v, want %v", tc.q, got, tc.want)
		}
	}

	// Facets come from the WHOLE board even under a filter: a dropdown that
	// listed only what the current filter matched could not change the filter.
	one := getJSON(t, h, "/api/mb?from=alice")
	facets, _ := one["facets"].(map[string]any)
	if from, _ := facets["from"].([]any); len(from) != 2 {
		t.Errorf("facets.from under a filter = %d entries, want 2 (the whole board)", len(from))
	}
}

// The incremental poll: the board is append-only with a monotonic seq, so
// `since` is the whole synchronisation mechanism.
func TestBoardSinceReturnsOnlyNewPosts(t *testing.T) {
	boardInTemp(t, bus.Post{From: "alice", Body: "one"})
	h := newTestHandler(t, Options{})

	first := getJSON(t, h, "/api/mb")
	high := int(first["high_seq"].(float64))

	if got := seqs(t, getJSON(t, h, "/api/mb?since="+strconv.Itoa(high))); len(got) != 0 {
		t.Fatalf("a quiet poll returned %v, want nothing", got)
	}
	if err := bus.PostMessage(bus.Post{From: "bob", Body: "two"}); err != nil {
		t.Fatal(err)
	}
	if got := seqs(t, getJSON(t, h, "/api/mb?since="+strconv.Itoa(high))); !equalInts(got, []int{high + 1}) {
		t.Fatalf("poll after one new post = %v, want [%d]", got, high+1)
	}
}

// THE cursor invariant. Reading the board in a browser must not consume an
// agent's mail: nothing here may call MarkSeen, and no reader identity may be
// accepted from the query string.
func TestBoardReadNeverAdvancesACursor(t *testing.T) {
	boardInTemp(t, bus.Post{From: "alice", Body: "one"}, bus.Post{From: "bob", Body: "two"})
	h := newTestHandler(t, Options{})

	reader := getJSON(t, h, "/api/mb")["reader"].(string)
	if err := bus.MarkSeen(reader, 1); err != nil {
		t.Fatal(err)
	}
	before := bus.SeenSeq(reader)

	for _, p := range []string{"/mb/", "/api/mb", "/api/mb?unread=1", "/api/mb?since=1", "/api/mb/2/viewers"} {
		do(h, "GET", p, "127.0.0.1:5555", nil)
	}
	if after := bus.SeenSeq(reader); after != before {
		t.Fatalf("cursor moved %d -> %d by READING the board in a browser;\n"+
			"a page that marks posts read eats an agent's mail every time a tab is opened",
			before, after)
	}

	// And a URL cannot make the page read as somebody else.
	other := getJSON(t, h, "/api/mb?as=someone-else&reader=someone-else")["reader"].(string)
	if other != reader {
		t.Fatalf("reader = %q with an ?as= in the URL, want the request's own identity %q", other, reader)
	}
}

func TestBoardSendSignsWithTheRequestIdentity(t *testing.T) {
	boardInTemp(t)
	h := newTestHandler(t, Options{})
	me := getJSON(t, h, "/api/mb")["reader"].(string)

	// The browser names a `from`. It must be ignored: a browser that could sign
	// as any agent would break the board's one guarantee.
	w := postJSON(h, `{"from":"someone-else","body":"hello board"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("post = %d (%s)", w.Code, strings.TrimSpace(w.Body.String()))
	}
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if out["from"] != me {
		t.Fatalf("post signed as %v, want %q — the body's `from` was believed", out["from"], me)
	}

	posts, err := bus.Posts()
	if err != nil || len(posts) != 1 {
		t.Fatalf("board has %d posts (%v), want 1", len(posts), err)
	}
	if posts[0].From != me || posts[0].Topic != "mb" || posts[0].Body != "hello board" {
		t.Fatalf("stored %+v, want from=%q topic=mb", posts[0], me)
	}
	if !posts[0].Broadcast() {
		t.Fatal("a post with no addressee should be a broadcast")
	}
}

func TestBoardSendRefusalsWriteNothing(t *testing.T) {
	boardInTemp(t)
	h := newTestHandler(t, Options{})

	for _, tc := range []struct{ name, body, want string }{
		{"oversized body", `{"body":"` + strings.Repeat("x", bus.MaxCoordinationBodyBytes+1) + `"}`, "1024"},
		{"unresolvable addressee", `{"to":"nobody-here","body":"hi"}`, "nobody-here"},
		{"empty body", `{"body":"   "}`, "body"},
		{"malformed json", `{`, "malformed"},
	} {
		w := postJSON(h, tc.body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", tc.name, w.Code)
			continue
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Errorf("%s error = %s, want it to mention %q", tc.name, strings.TrimSpace(w.Body.String()), tc.want)
		}
	}

	// Every one of those is a refusal, and a refusal appends NOTHING: a post
	// nobody answers is a receipt indistinguishable from a real delivery.
	if posts, err := bus.Posts(); err != nil || len(posts) != 0 {
		t.Fatalf("board has %d posts after four refusals (%v), want 0", len(posts), err)
	}
}

func TestBoardTileIsDeclaredAndNotDoubleMounted(t *testing.T) {
	var found bool
	for _, p := range Discover() {
		if p.Name == "mb" {
			found = true
			if p.Path != "/mb/" || p.Source != "atlas" || !p.Available {
				t.Fatalf("mb panel = %+v, want an available atlas panel at /mb/", p)
			}
		}
	}
	if !found {
		t.Fatal("no mb tile: the atlas WebSurface declaration is what Discover reads")
	}
	// Like the terminal, the page is served at the launcher's root so it keeps
	// the launcher's <base href>; panelHandler must therefore claim nothing.
	s := &server{}
	if h, _ := s.panelHandler(Panel{Name: "mb", Path: "/mb/"}); h != nil {
		t.Fatal("mb must not also be mounted: coopauth.Mount would take the <base href> with it")
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
