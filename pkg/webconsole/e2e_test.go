//go:build e2e

// End-to-end tests for the launcher and its apps.
//
// These bind a REAL port and speak real HTTP and WebSocket, because the failures
// this package has actually shipped were invisible to handler-level tests: a
// script whose entry point had been spliced out (every response still 200, page
// blank), assets served with no validator (fix shipped, browser kept the old
// copy), a panel mounted at a path its own tile did not link to. Each of those
// needs the whole thing running to see.
//
//	go test -tags e2e ./pkg/webconsole/
package webconsole

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/qiangli/yoke/pkg/board"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/hostauth"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/websession"
	"github.com/qiangli/yoke/pkg/webterm"
)

// serve starts the launcher on a real loopback port and returns its base URL.
func serve(t *testing.T, opts Options) string {
	t.Helper()
	opts.Ctx = context.Background()
	if opts.Terminal.Shell == nil && runtime.GOOS != "windows" {
		// A fixed, dependency-free shell: the point here is the plumbing, and a
		// login shell would drag in the operator's startup files.
		opts.Terminal.Shell = []string{"/bin/sh"}
	}

	h, closer, err := Handler(opts)
	if err != nil {
		t.Fatalf("Handler: %v", err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		_ = closer()
	})

	base := "http://" + ln.Addr().String()
	// Do not proceed until it actually answers; a race here would be reported as
	// whatever assertion happened to run first.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if resp, err := http.Get(base + "/healthz"); err == nil {
			resp.Body.Close()
			return base
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("launcher never became ready")
	return ""
}

func get(t *testing.T, url string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b := make([]byte, 1<<20)
	n, _ := resp.Body.Read(b)
	for n < len(b) {
		m, err := resp.Body.Read(b[n:])
		n += m
		if err != nil {
			break
		}
	}
	return resp.StatusCode, string(b[:n]), resp.Header
}

// ---------------------------------------------------------------- launcher --

func TestE2ELauncherServesItsApps(t *testing.T) {
	base := serve(t, Options{})

	code, body, _ := get(t, base+"/")
	if code != http.StatusOK {
		t.Fatalf("start page = %d", code)
	}
	if !strings.Contains(body, `id="grid-host"`) {
		t.Error("start page is not the launcher")
	}
	// Asset URLs must be content-versioned, or a browser can keep running the
	// previous build's script indefinitely (embed.FS has a zero modtime).
	if !strings.Contains(body, "app.js?v=") {
		t.Error("assets are not content-versioned; a cached script can outlive a fix")
	}

	code, body, _ = get(t, base+"/api/apps")
	if code != http.StatusOK {
		t.Fatalf("/api/apps = %d", code)
	}
	var apps struct {
		Schema string `json:"schema_version"`
		Apps   []struct {
			Name, Label, Path, Status, StartHint string
		} `json:"apps"`
	}
	if err := json.Unmarshal([]byte(body), &apps); err != nil {
		t.Fatalf("decode /api/apps: %v", err)
	}
	if apps.Schema != appsSchemaVersion {
		t.Errorf("schema = %q", apps.Schema)
	}

	// EVERY app, not the three that happened to exist when this was written: a
	// tile whose path serves the launcher back is the exact defect this loop
	// catches, and it can only catch it for apps it names.
	want := map[string]bool{
		"terminal": false, "files": false, "meet": false,
		"sprint": false, "inbox": false, "mb": false,
	}
	for _, a := range apps.Apps {
		if _, ok := want[a.Name]; ok {
			want[a.Name] = true
			if a.Status != StatusReady {
				t.Errorf("%s is %q, want ready", a.Name, a.Status)
			}
			// Every tile links somewhere that answers; the terminal once
			// advertised a path nothing was mounted at.
			if c, b, _ := get(t, base+a.Path); c != http.StatusOK {
				t.Errorf("%s advertises %s which serves %d", a.Name, a.Path, c)
			} else if strings.Contains(b, `id="grid-host"`) {
				t.Errorf("%s advertises %s but that fell through to the start page", a.Name, a.Path)
			}
		}
	}
	for name, seen := range want {
		if !seen {
			t.Errorf("%s missing from /api/apps", name)
		}
	}
}

func TestE2EAssetsRevalidate(t *testing.T) {
	base := serve(t, Options{})

	_, body, _ := get(t, base+"/")
	// Follow the versioned URL the page actually asks for.
	i := strings.Index(body, `src="app.js?v=`)
	if i < 0 {
		t.Fatal("no versioned app.js reference")
	}
	ref := body[i+len(`src="`):]
	ref = ref[:strings.IndexByte(ref, '"')]

	code, _, hdr := get(t, base+"/"+ref)
	if code != http.StatusOK {
		t.Fatalf("%s = %d", ref, code)
	}
	etag := hdr.Get("ETag")
	if etag == "" {
		t.Fatal("asset has no ETag")
	}

	req, _ := http.NewRequest("GET", base+"/"+ref, nil)
	req.Header.Set("If-None-Match", etag)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("conditional GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Errorf("conditional GET = %d, want 304", resp.StatusCode)
	}
}

// ------------------------------------------------------------------- files --

func TestE2EFilesServesItsScope(t *testing.T) {
	dir := t.TempDir()
	marker := "e2e-marker-file.txt"
	if err := os.WriteFile(filepath.Join(dir, marker), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	base := serve(t, Options{Scope: dir})

	code, body, _ := get(t, base+"/files/")
	if code != http.StatusOK {
		t.Fatalf("/files/ = %d", code)
	}
	if !strings.Contains(strings.ToLower(body), "file browser") {
		t.Error("/files/ did not serve File Browser")
	}
	// fbembed carries the mount in StaticURL rather than a <base href> — asserting
	// the wrong mechanism is how a passing test can describe a panel it never
	// actually checked. If this is absent the panel is loading its assets from
	// the launcher root and the page is broken under any prefix.
	if !strings.Contains(body, `"/files/static"`) {
		t.Error("/files/ did not rewrite StaticURL for its mount")
	}

	// And the scope must be the one we asked for: this panel already shipped once
	// defaulting to the server's working directory, which showed a different tree
	// per invocation.
	//
	// The listing needs the token the SPA itself carries. This check used to be
	// written as `if code == 200 && ...`, which meant the 401 an unauthenticated
	// probe actually gets skipped the assertion entirely — the test reported a
	// scope it had never looked at.
	code, listing, _ := getAuthed(t, base+"/files/api/resources/", filesToken(t, base))
	if code != http.StatusOK {
		t.Fatalf("/files/api/resources/ = %d (%s)", code, ellipsize(strings.TrimSpace(listing), 200))
	}
	if !strings.Contains(listing, marker) {
		t.Errorf("scope %s does not list %s: %s", dir, marker, ellipsize(listing, 200))
	}
}

// The Files panel is READ-ONLY unless the operator said otherwise, and the
// refusal is asserted against the DISK.
//
// A status-only assertion would pass just as happily against a typo'd URL, so
// the write that must be refused is the same request, byte for byte, as the one
// proven to work in the AllowWrite half — that positive control is what stops
// this from being a test that cannot fail.
func TestE2EFilesIsReadOnlyUnlessAsked(t *testing.T) {
	write := func(t *testing.T, allowWrite bool) (int, bool) {
		t.Helper()
		dir := t.TempDir()
		base := serve(t, Options{Scope: dir, AllowWrite: allowWrite})
		req, err := http.NewRequest("POST", base+"/files/api/resources/written.txt",
			strings.NewReader("written by the e2e"))
		if err != nil {
			t.Fatalf("request: %v", err)
		}
		req.Header.Set("X-Auth", filesToken(t, base))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("POST: %v", err)
		}
		resp.Body.Close()
		_, statErr := os.Stat(filepath.Join(dir, "written.txt"))
		return resp.StatusCode, statErr == nil
	}

	code, created := write(t, false)
	if code == http.StatusOK || created {
		t.Errorf("read-only console accepted a write: status %d, file created %v", code, created)
	}

	code, created = write(t, true)
	if code != http.StatusOK || !created {
		t.Fatalf("AllowWrite console refused the same write: status %d, file created %v — "+
			"the refusal above proves nothing until this succeeds", code, created)
	}
}

// filesToken logs in the way the File Browser SPA does. fbembed runs NoAuth —
// the embedding console is the access gate — but its API still wants the JWT,
// so an unauthenticated probe gets a 401 that says nothing about scope or
// permissions.
func filesToken(t *testing.T, base string) string {
	t.Helper()
	code, body, _ := post(t, base+"/files/api/login", "")
	if code != http.StatusOK {
		t.Fatalf("/files/api/login = %d (%s)", code, ellipsize(strings.TrimSpace(body), 120))
	}
	tok := strings.TrimSpace(body)
	if tok == "" {
		t.Fatal("/files/api/login returned an empty token")
	}
	return tok
}

func getAuthed(t *testing.T, url, token string) (int, string, http.Header) {
	t.Helper()
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		t.Fatalf("request %s: %v", url, err)
	}
	req.Header.Set("X-Auth", token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b := make([]byte, 1<<20)
	n := 0
	for n < len(b) {
		m, err := resp.Body.Read(b[n:])
		n += m
		if err != nil {
			break
		}
	}
	return resp.StatusCode, string(b[:n]), resp.Header
}

// ellipsize is the test-local shortener. It is NOT named truncate: pair_http.go
// declares a package-level truncate, and a same-package _test.go redeclaring it
// makes the whole e2e build fail — which is why the e2e tag had stopped
// compiling at all, taking the login round-trip guarantee with it.
func ellipsize(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ------------------------------------------------------------------- relay --

func TestE2ERelayServesTheRoom(t *testing.T) {
	base := serve(t, Options{})

	code, body, _ := get(t, base+"/meet/")
	if code != http.StatusOK {
		t.Fatalf("/meet/ = %d", code)
	}
	// Mounting under a prefix must rewrite the room's base href, or every asset
	// it requests resolves against the launcher instead.
	if !strings.Contains(body, `<base href="/meet/"`) {
		t.Errorf("/meet/ base href not rewritten for the mount")
	}

	// The room's own API must work through the mount. This is the regression
	// that matters most: stamping X-Forwarded-Prefix makes the room read the
	// request as cloud-vouched, and its default gate would 403 the machine owner.
	code, body, _ = get(t, base+"/meet/api/rooms")
	if code != http.StatusOK {
		t.Fatalf("/meet/api/rooms = %d (%s) — the mounted gate is wrong",
			code, strings.TrimSpace(body))
	}
	if !json.Valid([]byte(body)) {
		t.Errorf("/meet/api/rooms did not return JSON: %q", body)
	}
}

// ---------------------------------------------------------------- terminal --

func TestE2ETerminalRunsAShell(t *testing.T) {
	if !webterm.Supported() {
		t.Skip("no pseudo-console on this platform")
	}
	base := serve(t, Options{})

	// The page and the socket are different things at the same mount.
	code, body, _ := get(t, base+"/term/")
	if code != http.StatusOK {
		t.Fatalf("/term/ = %d", code)
	}
	if !strings.Contains(body, "term.js") {
		t.Error("/term/ did not serve the terminal page")
	}

	u := "ws" + strings.TrimPrefix(base, "http") + "/term/ws?cols=100&rows=30"
	c, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		code := 0
		if resp != nil {
			code = resp.StatusCode
		}
		t.Fatalf("dial %s: %v (status %d)", u, err, code)
	}
	defer c.Close()

	if err := c.WriteMessage(websocket.BinaryMessage, []byte("echo E2E-TERMINAL-OK\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	var sb strings.Builder
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (saw %q)", err, sb.String())
		}
		sb.Write(data)
		if strings.Contains(sb.String(), "E2E-TERMINAL-OK") {
			break
		}
	}

	// Resize is a control frame, not input, and must reach the pty.
	if err := c.WriteMessage(websocket.TextMessage,
		[]byte(`{"type":"size","cols":133,"rows":47}`)); err != nil {
		t.Fatalf("resize: %v", err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("stty size\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	sb.Reset()
	_ = c.SetReadDeadline(time.Now().Add(20 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read after resize: %v (saw %q)", err, sb.String())
		}
		sb.Write(data)
		if strings.Contains(sb.String(), "47 133") {
			return
		}
	}
}

// A terminal that hands out a shell must not accept an upgrade from another
// origin: the launcher can be bound to a LAN address.
func TestE2ETerminalRefusesCrossOrigin(t *testing.T) {
	if !webterm.Supported() {
		t.Skip("no pseudo-console on this platform")
	}
	base := serve(t, Options{})

	u := "ws" + strings.TrimPrefix(base, "http") + "/term/ws"
	_, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Origin": {"http://evil.example"}})
	if err == nil {
		t.Fatal("cross-origin upgrade accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %v", resp)
	}
}

// ------------------------------------------------------------------- login --

// stubAuth accepts one password, so the e2e can exercise the real gate, cookie
// and rate limiter without needing this machine's actual credentials.
type stubAuth struct{ password string }

func (s stubAuth) Authenticate(user, pass string) error {
	if pass == s.password {
		return nil
	}
	return hostauth.ErrInvalidCredentials
}

func TestE2ELoginGuardsAnExposedConsole(t *testing.T) {
	base := serve(t, Options{
		RequireLogin: true,
		Auth:         stubAuth{password: "correct-horse"},
		Sessions:     websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
	})

	// Unauthenticated: the launcher and every app must be closed, and a browser
	// should be sent to the form rather than given a bare 403.
	jar := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	for _, p := range []string{"/", "/api/apps", "/term/", "/files/", "/meet/"} {
		req, _ := http.NewRequest("GET", base+p, nil)
		req.Header.Set("Accept", "text/html")
		resp, err := jar.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusFound && resp.StatusCode != http.StatusForbidden {
			t.Errorf("%s is reachable without a session (%d)", p, resp.StatusCode)
		}
	}

	// The form itself must be reachable, or there is no way in.
	if code, body, _ := get(t, base+"/login"); code != http.StatusOK ||
		!strings.Contains(body, `name="password"`) ||
		!strings.Contains(body, `aria-label="Show password"`) ||
		!strings.Contains(body, `input.type = show ? "text" : "password"`) {
		t.Fatalf("/login = %d, no form", code)
	}

	// A wrong password does not mint a session.
	c := &http.Client{Jar: newJar(t), CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := c.PostForm(base+"/api/login", map[string][]string{
		"user": {currentOSUser()}, "password": {"wrong"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusSeeOther {
		t.Fatal("a wrong password was accepted")
	}

	// The right one does, and the session then opens the launcher and its apps.
	resp, err = c.PostForm(base+"/api/login", map[string][]string{
		"user": {currentOSUser()}, "password": {"correct-horse"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login returned %d, want 303", resp.StatusCode)
	}

	for _, p := range []string{"/", "/api/apps", "/files/", "/meet/api/rooms"} {
		req, _ := http.NewRequest("GET", base+p, nil)
		resp, err := c.Do(req)
		if err != nil {
			t.Fatalf("GET %s: %v", p, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Errorf("%s = %d after signing in", p, resp.StatusCode)
		}
	}

	// The session must be visible to the page, since that is what decides whether
	// a sign-out control is shown at all.
	req, _ := http.NewRequest("GET", base+"/api/session", nil)
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var sess struct{ User, Via string }
	_ = json.NewDecoder(resp.Body).Decode(&sess)
	resp.Body.Close()
	if sess.Via != "session" {
		t.Errorf("/api/session via = %q, want \"session\" — the header cannot know it is signed in", sess.Via)
	}

	// Signing out must actually revoke: a logout that only clears the cookie
	// leaves a still-valid token in anything that captured it.
	resp, err = c.PostForm(base+"/api/logout", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("logout = %d, want 303", resp.StatusCode)
	}
	req, _ = http.NewRequest("GET", base+"/api/apps", nil)
	req.Header.Set("Accept", "text/html")
	resp, err = c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		t.Error("still authenticated after signing out")
	}
}

// The throttle is the only thing standing between a LAN peer and unlimited
// password guesses.
func TestE2ELoginIsRateLimited(t *testing.T) {
	base := serve(t, Options{
		RequireLogin: true,
		Auth:         stubAuth{password: "correct-horse"},
		Sessions:     websession.NewStore(time.Hour, []byte("test-key-test-key-test-key-32byt")),
	})

	limited := false
	for i := 0; i < 12; i++ {
		resp, err := http.PostForm(base+"/api/login", map[string][]string{
			"user": {currentOSUser()}, "password": {"wrong"},
		})
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode == http.StatusTooManyRequests {
			limited = true
			break
		}
	}
	if !limited {
		t.Error("unlimited password attempts accepted")
	}
}

func newJar(t *testing.T) http.CookieJar {
	t.Helper()
	j, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return j
}

// ------------------------------------------------------------------ chrome --

// Every embedded app page carries the console's own chrome, in the console's
// own bar.
//
// The four pages here are the ones served from the artifact tree, so the
// injection in embed.go is what puts the return control on them. It is asserted
// at this level because the position is a whole-page fact: the button must be
// inside <header id="bar">, and the inbox is the page that proves the rule —
// it has a SECOND header over its message list, and appending before the last
// one filed the console's control inside the app's content area.
func TestE2EAppPagesCarryTheConsoleChrome(t *testing.T) {
	base := serve(t, Options{})

	for _, page := range []string{"/term/", "/sprint/", "/mb/", "/inbox/"} {
		t.Run(page, func(t *testing.T) {
			code, body, _ := get(t, base+page)
			if code != http.StatusOK {
				t.Fatalf("%s = %d", page, code)
			}
			btn := strings.Index(body, `id="all-apps-btn"`)
			if btn < 0 {
				t.Fatalf("%s has no return control in same-tab mode", page)
			}
			barEnd := strings.Index(body, "</header>")
			if barEnd < 0 || btn > barEnd {
				t.Errorf("%s: return control is outside the console bar", page)
			}
			if !strings.Contains(body, `id="copyright"`) {
				t.Errorf("%s: no copyright footer", page)
			}
		})
	}
}

// ------------------------------------------------------------------ sprint --

// The Sprint app over the wire: its page, the overview its page polls, and the
// story endpoint behind a card.
//
// collectBoardFn is stubbed. The real collector fans out across the HOST's
// weave queues and forks `weave doctor` per root, so a test that used it would
// report the machine it ran on rather than the code under test — and would take
// seconds doing it.
func TestE2ESprintServesItsBoard(t *testing.T) {
	b := fakeBoard(t)
	prev := collectBoardFn
	collectBoardFn = func(context.Context) (*board.Board, error) { return b, nil }
	t.Cleanup(func() { collectBoardFn = prev })

	base := serve(t, Options{})

	code, body, _ := get(t, base+"/sprint/")
	if code != http.StatusOK {
		t.Fatalf("/sprint/ = %d", code)
	}
	if !strings.Contains(body, "board.js") {
		t.Error("/sprint/ did not serve the board page")
	}
	// The retired mount is not an alias for Sprint.
	for _, old := range []string{"/board", "/board/"} {
		if _, b2, _ := get(t, base+old); strings.Contains(b2, "<title>Sprint") {
			t.Errorf("%s still reaches the Sprint page", old)
		}
	}

	code, body, _ = get(t, base+"/api/sprint")
	if code != http.StatusOK {
		t.Fatalf("/api/sprint = %d (%s)", code, ellipsize(strings.TrimSpace(body), 200))
	}
	var view struct {
		Schema  string `json:"schema_version"`
		Sprints []struct {
			ID     int    `json:"id"`
			Title  string `json:"title"`
			Column string `json:"column"`
		} `json:"sprints"`
		Todos []struct {
			ID       string `json:"id"`
			SprintID int    `json:"sprint_id"`
		} `json:"todos"`
		SprintTotal int `json:"sprint_total"`
	}
	if err := json.Unmarshal([]byte(body), &view); err != nil {
		t.Fatalf("decode /api/sprint: %v", err)
	}
	if view.Schema == "" {
		t.Error("/api/sprint carries no schema_version")
	}
	// The seeded board reaches the browser, not an empty projection: this
	// endpoint has shipped a summary strip whose counts had nothing behind them.
	var live bool
	for _, s := range view.Sprints {
		if s.ID == 1 && s.Title == "live sprint" {
			live = true
		}
	}
	if !live {
		t.Errorf("the live sprint is missing from /api/sprint: %+v", view.Sprints)
	}
	if len(view.Todos) == 0 {
		t.Error("/api/sprint carries no stories; a sprint whose stories are invisible reads as empty")
	}

	// An id nothing matches is a 404 with a message, never an empty 200 — a
	// detail pane that silently shows nothing is indistinguishable from a story
	// with no body.
	code, body, _ = get(t, base+"/api/sprint/story/no-such-story")
	if code != http.StatusNotFound {
		t.Errorf("story detail for an unknown id = %d, want 404", code)
	}
	if !strings.Contains(body, "no-such-story") {
		t.Errorf("404 body does not name the id asked for: %s", ellipsize(body, 120))
	}
}

// ------------------------------------------------------------------- inbox --

// The Inbox app over the wire, and its defining property: LOOKING IS NOT
// READING.
//
// The page repaints on a timer, so a read path that advanced a cursor would
// drain the fleet's mail while nobody was looking at a message — and would do
// it invisibly, because the mail is not lost, just marked handed to an agent
// that was never handed it. The bytes on disk are the only witness to that, so
// this fingerprints the store around a full page load plus every polled
// endpoint.
func TestE2EInboxPeeksWithoutConsuming(t *testing.T) {
	dir := inboxInTemp(t,
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "pick up #12"},
		room.Event{Principal: "cairn", To: "lintel", Topic: "gate", Body: "86/86"},
	)
	base := serve(t, Options{})

	before := roomFingerprint(t, dir)

	code, body, _ := get(t, base+"/inbox/")
	if code != http.StatusOK {
		t.Fatalf("/inbox/ = %d", code)
	}
	if !strings.Contains(body, "inbox.js") {
		t.Error("/inbox/ did not serve the inbox page")
	}

	code, body, _ = get(t, base+"/api/inbox")
	if code != http.StatusOK {
		t.Fatalf("/api/inbox = %d (%s)", code, ellipsize(strings.TrimSpace(body), 200))
	}
	var roster struct {
		Schema  string `json:"schema_version"`
		Viewer  string `json:"viewer"`
		Unread  int    `json:"unread"`
		Holders []struct {
			Name string `json:"name"`
		} `json:"holders"`
	}
	if err := json.Unmarshal([]byte(body), &roster); err != nil {
		t.Fatalf("decode /api/inbox: %v", err)
	}
	if roster.Schema == "" {
		t.Error("/api/inbox carries no schema_version")
	}
	if roster.Unread == 0 {
		t.Error("seeded mail is not counted; the roster is reporting an empty fleet")
	}
	var sawCairn bool
	for _, h := range roster.Holders {
		if h.Name == "cairn" {
			sawCairn = true
		}
	}
	if !sawCairn {
		t.Errorf("the seeded addressee is not on the roster: %+v", roster.Holders)
	}

	// Someone ELSE's inbox, which is the whole reason this page exists — and
	// the request that must not consume.
	code, body, _ = get(t, base+"/api/inbox/cairn")
	if code != http.StatusOK {
		t.Fatalf("/api/inbox/cairn = %d (%s)", code, ellipsize(strings.TrimSpace(body), 200))
	}
	if !strings.Contains(body, "pick up #12") {
		t.Errorf("cairn's mail is not in its own inbox view: %s", ellipsize(body, 200))
	}

	if after := roomFingerprint(t, dir); !sameFingerprint(before, after) {
		t.Errorf("the inbox page changed the store on disk:\nbefore %v\nafter  %v", before, after)
	}
}

// sameFingerprint compares two directory hashes. A map compare is written out
// rather than reflect.DeepEqual'd so a failure names the file that moved.
func sameFingerprint(before, after map[string]string) bool {
	if len(before) != len(after) {
		return false
	}
	for name, sum := range before {
		if after[name] != sum {
			return false
		}
	}
	return true
}

// The one write this app has, over the wire: it acts on the CALLER's own inbox,
// and there is no route through which another can be named.
//
// A non-GET on a named path falls through to the SPA catch-all — the console
// has no method-based dispatch that could 405 — so "no route" is observable as
// two things together: nothing answers with the panel's own payload, and the
// named inbox is untouched afterwards. The second half is the one that matters,
// because consuming another agent's mail looks like nothing from anywhere: the
// message stays durable on the timeline and is simply never handed over.
func TestE2EInboxMarkReadTakesNoName(t *testing.T) {
	inboxInTemp(t,
		room.Event{Principal: "cairn", To: "operator", Topic: "gate", Body: "yours to read"},
		room.Event{Principal: "operator", To: "cairn", Topic: "sprint", Body: "not yours to read"},
	)
	base := serve(t, Options{})

	unread := func(name string) int {
		t.Helper()
		code, body, _ := get(t, base+"/api/inbox/"+name)
		if code != http.StatusOK {
			t.Fatalf("/api/inbox/%s = %d", name, code)
		}
		var v struct {
			Unread int `json:"unread"`
		}
		if err := json.Unmarshal([]byte(body), &v); err != nil {
			t.Fatalf("decode /api/inbox/%s: %v", name, err)
		}
		return v.Unread
	}

	cairnBefore := unread("cairn")
	if cairnBefore == 0 {
		t.Fatal("seeded mail for cairn is already read; the rest of this proves nothing")
	}
	if unread("operator") == 0 {
		t.Fatal("the viewer has no waiting mail; the rest of this proves nothing")
	}

	for _, path := range []string{"/api/inbox/cairn/read", "/api/inbox/read/cairn"} {
		_, body, hdr := post(t, base+path, `{"all":true}`)
		if strings.Contains(hdr.Get("Content-Type"), "json") || strings.Contains(body, inboxSchemaVersion) {
			t.Errorf("POST %s answered with the panel's payload; only the nameless route may write", path)
		}
	}
	if got := unread("cairn"); got != cairnBefore {
		t.Errorf("cairn's unread went %d -> %d; a named write path reached another inbox", cairnBefore, got)
	}

	code, body, _ := post(t, base+"/api/inbox/read", `{"all":true}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/inbox/read = %d (%s)", code, ellipsize(strings.TrimSpace(body), 200))
	}
	if got := unread("operator"); got != 0 {
		t.Errorf("the viewer's own inbox still has %d unread after marking all read", got)
	}
	if got := unread("cairn"); got != cairnBefore {
		t.Errorf("marking the viewer's mail read changed cairn's inbox (%d -> %d)", cairnBefore, got)
	}

	// Exactly one of seq or all — a request that says both, or neither, is a
	// refusal rather than a guess about what the caller meant.
	for _, in := range []string{`{}`, `{"all":true,"seq":3}`} {
		if code, _, _ := post(t, base+"/api/inbox/read", in); code != http.StatusBadRequest {
			t.Errorf("POST /api/inbox/read %s = %d, want 400", in, code)
		}
	}
}

// ---------------------------------------------------------------- messages --

// The Messages app over the wire: post from the browser, read it back, and the
// attribution rule that makes the board worth reading.
func TestE2EMessagesRoundTrip(t *testing.T) {
	boardInTemp(t, bus.Post{From: "alice", Topic: "posix-cert", Body: "arm is green"})
	base := serve(t, Options{})

	code, body, _ := get(t, base+"/mb/")
	if code != http.StatusOK {
		t.Fatalf("/mb/ = %d", code)
	}
	if !strings.Contains(body, "mb.js") {
		t.Error("/mb/ did not serve the messages page")
	}

	// THE SENDER IS DERIVED, NEVER SUPPLIED. A browser that could name its own
	// `from` could sign as any agent on the host.
	code, body, _ = post(t, base+"/api/mb",
		`{"from":"someone-else","topic":"e2e","body":"posted over the wire"}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/mb = %d (%s)", code, ellipsize(strings.TrimSpace(body), 200))
	}
	var sent struct {
		From string `json:"from"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decode POST /api/mb: %v", err)
	}
	if sent.From == "" || sent.From == "someone-else" {
		t.Fatalf("post signed as %q; the body's `from` was believed", sent.From)
	}

	code, body, _ = get(t, base+"/api/mb")
	if code != http.StatusOK {
		t.Fatalf("/api/mb = %d", code)
	}
	var list struct {
		Schema string `json:"schema_version"`
		Posts  []struct {
			From string `json:"from"`
			Body string `json:"body"`
		} `json:"posts"`
	}
	if err := json.Unmarshal([]byte(body), &list); err != nil {
		t.Fatalf("decode /api/mb: %v", err)
	}
	if list.Schema == "" {
		t.Error("/api/mb carries no schema_version")
	}
	var seeded, mine bool
	for _, p := range list.Posts {
		if p.Body == "arm is green" && p.From == "alice" {
			seeded = true
		}
		if p.Body == "posted over the wire" && p.From == sent.From {
			mine = true
		}
	}
	if !seeded {
		t.Error("the seeded post is missing from the board")
	}
	if !mine {
		t.Error("the post made through the browser did not come back")
	}

	// A refusal appends nothing: a receipt for a post nobody can answer is
	// indistinguishable from a delivery.
	if code, _, _ := post(t, base+"/api/mb", `{"body":"   "}`); code != http.StatusBadRequest {
		t.Errorf("empty post = %d, want 400", code)
	}
}

// post is the e2e POST. It is deliberately NOT named postJSON: panel_mb_test.go
// already declares that at handler level, and a same-package redeclaration
// breaks the whole e2e build — the lesson ellipsize records above.
func post(t *testing.T, url, body string) (int, string, http.Header) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b := make([]byte, 1<<20)
	n := 0
	for n < len(b) {
		m, err := resp.Body.Read(b[n:])
		n += m
		if err != nil {
			break
		}
	}
	return resp.StatusCode, string(b[:n]), resp.Header
}

// --------------------------------------------------------- asset plumbing --

// Every app serves every asset its own page asks for, at the URL the page asks
// for it.
//
// This is the failure class the file header opens with — a spliced-out entry
// point, a panel loading its assets from the launcher root because the mount
// was not rewritten, a bundle that was never promoted into the tree it is
// embedded from. All of them return 200 on the PAGE and leave the app blank, so
// a test that only fetches the page cannot see any of them.
//
// The references are resolved the way a browser resolves them: against the
// page's own <base href> when it has one, and against the page URL otherwise.
// Asserting a hardcoded asset path instead would prove the file exists while
// saying nothing about whether the page can reach it under this mount.
func TestE2EEveryAppServesTheAssetsItsPageAsks(t *testing.T) {
	base := serve(t, Options{Scope: t.TempDir()})

	for _, page := range []string{"/term/", "/sprint/", "/mb/", "/inbox/", "/meet/", "/files/"} {
		t.Run(page, func(t *testing.T) {
			code, body, _ := get(t, base+page)
			if code != http.StatusOK {
				t.Fatalf("%s = %d", page, code)
			}
			refs := assetRefs(t, base+page, body)
			if len(refs) == 0 {
				t.Fatalf("%s references no script or stylesheet; its entry point is gone", page)
			}
			for _, ref := range refs {
				if c, b, _ := get(t, ref); c != http.StatusOK {
					t.Errorf("%s asks for %s which serves %d", page, ref, c)
				} else if strings.Contains(b, `id="grid-host"`) {
					// The catch-all answers 200 with the launcher for anything
					// unrouted, so a missing asset is not a 404 — it is the
					// start page arriving where a script was expected.
					t.Errorf("%s asks for %s and got the launcher page back", page, ref)
				}
			}
		})
	}
}

// assetRefs returns the absolute URLs of the scripts and stylesheets a page
// asks for, resolved the way a browser would.
func assetRefs(t *testing.T, pageURL, body string) []string {
	t.Helper()
	pu, err := url.Parse(pageURL)
	if err != nil {
		t.Fatalf("parse %s: %v", pageURL, err)
	}
	// <base href> wins for every relative reference on the page — it is the
	// whole mechanism by which these apps compose under a route prefix.
	if i := strings.Index(body, `<base href="`); i >= 0 {
		rest := body[i+len(`<base href="`):]
		if j := strings.IndexByte(rest, '"'); j > 0 {
			if b, err := pu.Parse(rest[:j]); err == nil {
				pu = b
			}
		}
	}

	seen := map[string]bool{}
	out := []string{}
	for _, attr := range []string{`src="`, `href="`} {
		rest := body
		for {
			i := strings.Index(rest, attr)
			if i < 0 {
				break
			}
			rest = rest[i+len(attr):]
			j := strings.IndexByte(rest, '"')
			if j < 0 {
				break
			}
			ref := rest[:j]
			rest = rest[j:]
			switch {
			case ref == "", strings.HasPrefix(ref, "data:"), strings.HasPrefix(ref, "http"):
				continue
			}
			path := ref
			if q := strings.IndexByte(path, '?'); q >= 0 {
				path = path[:q]
			}
			if !strings.HasSuffix(path, ".js") && !strings.HasSuffix(path, ".css") {
				continue
			}
			abs, err := pu.Parse(ref)
			if err != nil || seen[abs.String()] {
				continue
			}
			seen[abs.String()] = true
			out = append(out, abs.String())
		}
	}
	return out
}

// -------------------------------------------------------------------- meet --

// The Meet app's own API answers through the mount, and its room list is the
// real one rather than a fixture.
//
// The gate is the part worth pinning: stamping X-Forwarded-Prefix on the way in
// makes the room read the request as cloud-vouched, and its default gate would
// then 403 the machine owner sitting at the console.
func TestE2EMeetAnswersThroughItsMount(t *testing.T) {
	base := serve(t, Options{})

	code, body, _ := get(t, base+"/meet/api/rooms")
	if code != http.StatusOK {
		t.Fatalf("/meet/api/rooms = %d (%s) — the mounted gate is wrong",
			code, ellipsize(strings.TrimSpace(body), 200))
	}
	if !json.Valid([]byte(body)) {
		t.Fatalf("/meet/api/rooms did not return JSON: %q", ellipsize(body, 200))
	}

	// The unmounted twin must NOT exist: a second path to the same API is a
	// second gate to keep right, and the one that is easy to forget is the one
	// nothing links to. It is asserted on the PAYLOAD rather than the status,
	// because the console answers anything unrouted with the launcher page —
	// so "not routed" reads as 200 here, and only the body can tell them apart.
	if _, body, _ := get(t, base+"/api/rooms"); !strings.Contains(body, `id="grid-host"`) {
		t.Errorf("/api/rooms answered something other than the launcher page: %s",
			ellipsize(strings.TrimSpace(body), 200))
	}
}

// ------------------------------------------------------------- QR pairing --

// THE PHONE STAYS PAIRED FOR AS LONG AS THE OPERATOR SAID, and "never" is a
// real answer.
//
// End to end because the value crosses four layers on its way to mattering — a
// select on the Settings page, a JSON field, a boundary that reads ZERO AS
// NEVER (the opposite of the store's own convention one layer down), and a
// grant that used to silently shorten anything past twelve hours. A unit test
// on any single one of them would have passed while a phone still stopped
// working overnight, which is exactly what happened.
func TestE2EPairingHonoursTheChosenExpiry(t *testing.T) {
	stubPairAddresses(t, "workshop.local", "192.168.1.20")
	base := serve(t, Options{
		RequireLogin:  true,
		Pairing:       true,
		Auth:          stubAuth{password: "correct-horse"},
		Sessions:      websession.NewStore(12*time.Hour, []byte("test-key-test-key-test-key-32byt")),
		PairStorePath: filepath.Join(t.TempDir(), "pairing.json"),
	})

	client := &http.Client{Jar: newJar(t), CheckRedirect: func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err := client.PostForm(base+"/api/login", map[string][]string{
		"user": {currentOSUser()}, "password": {"correct-horse"},
	})
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("login = %d, want 303", resp.StatusCode)
	}

	mint := func(t *testing.T, body string) struct {
		Enabled      bool   `json:"enabled"`
		DeviceTTL    string `json:"device_ttl"`
		NeverExpires bool   `json:"never_expires"`
		Error        string `json:"error"`
	} {
		t.Helper()
		var out struct {
			Enabled      bool   `json:"enabled"`
			DeviceTTL    string `json:"device_ttl"`
			NeverExpires bool   `json:"never_expires"`
			Error        string `json:"error"`
		}
		resp, err := client.Post(base+"/api/pair", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("POST /api/pair: %v", err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("POST /api/pair %s = %d", body, resp.StatusCode)
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatalf("decode mint: %v", err)
		}
		if out.Error != "" {
			t.Fatalf("mint %s: %s", body, out.Error)
		}
		return out
	}

	// No choice at all is a DAY. This is the value every operator who never
	// opens the control gets, and it is the one the four-hour default made
	// annoying enough to be worth changing.
	got := mint(t, `{}`)
	if got.DeviceTTL != (24 * time.Hour).String() {
		t.Errorf("default device TTL = %q, want %q", got.DeviceTTL, (24 * time.Hour).String())
	}
	if got.NeverExpires {
		t.Error("the default pairing reported itself as never expiring")
	}

	// An explicit choice is honoured PAST THE OPERATOR GRANT. Seven days is
	// longer than the twelve-hour session that minted it, which is precisely
	// the case that used to come back looking granted and expire overnight.
	got = mint(t, `{"ttl_hours":168}`)
	if got.DeviceTTL != (168 * time.Hour).String() {
		t.Errorf("device TTL = %q, want %q — a request past the grant was clamped",
			got.DeviceTTL, (168 * time.Hour).String())
	}

	// Zero means NEVER, and the response says so in words rather than leaving a
	// reader to decode a year.
	got = mint(t, `{"ttl_hours":0}`)
	if !got.NeverExpires {
		t.Errorf("ttl_hours:0 reported never_expires=false with TTL %q", got.DeviceTTL)
	}
	ttl, err := time.ParseDuration(got.DeviceTTL)
	if err != nil {
		t.Fatalf("device TTL %q does not parse: %v", got.DeviceTTL, err)
	}
	if ttl <= neverExpiresAfter {
		t.Errorf("never-expiring device lasts %s, inside the window that still reads as a date", ttl)
	}
}
