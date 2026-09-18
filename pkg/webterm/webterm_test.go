package webterm

import (
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// testShell is an interactive shell that exists on the host under test. The PTY
// plumbing is what is being asserted, not the shell — so this deliberately does
// NOT use bashy, whose startup files are the operator's and can end the session
// for reasons that have nothing to do with this package.
func testShell() []string {
	if runtime.GOOS == "windows" {
		return []string{"cmd.exe"}
	}
	return []string{"/bin/sh"}
}

// dial starts the handler on a test server and opens a terminal socket.
func dial(t *testing.T, opts Options) (*websocket.Conn, func()) {
	t.Helper()
	srv := httptest.NewServer(Handler(opts))
	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws?cols=80&rows=24"
	c, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		srv.Close()
		t.Fatalf("dial: %v", err)
	}
	return c, func() { c.Close(); srv.Close() }
}

// readUntil accumulates PTY output until want appears or the deadline passes.
func readUntil(t *testing.T, c *websocket.Conn, want string) string {
	t.Helper()
	var sb strings.Builder
	_ = c.SetReadDeadline(time.Now().Add(15 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read (want %q, got %q): %v", want, sb.String(), err)
		}
		sb.Write(data)
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
	}
}

func TestTerminalRunsACommand(t *testing.T) {
	// /bin/sh, not bashy: this asserts the PTY plumbing, and depending on a
	// built sibling binary would make the test fail for an unrelated reason.
	c, done := dial(t, Options{Shell: testShell()})
	defer done()

	nl := "\n"
	if runtime.GOOS == "windows" {
		nl = "\r\n"
	}
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("echo hi-from-webterm"+nl)); err != nil {
		t.Fatalf("write: %v", err)
	}
	readUntil(t, c, "hi-from-webterm")
}

func TestResizeReachesTheChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no stty on Windows; ConPTY resize is asserted by the child not dying")
	}
	c, done := dial(t, Options{Shell: testShell()})
	defer done()

	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"type":"size","cols":133,"rows":47}`)); err != nil {
		t.Fatalf("resize: %v", err)
	}
	// stty reads the window size off the pty, so this proves TIOCSWINSZ landed
	// rather than merely that the frame parsed.
	if err := c.WriteMessage(websocket.BinaryMessage, []byte("stty size\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if out := readUntil(t, c, "47 133"); !strings.Contains(out, "47 133") {
		t.Fatalf("resize not applied: %q", out)
	}
}

func TestCrossOriginUpgradeRefused(t *testing.T) {
	srv := httptest.NewServer(Handler(Options{Shell: testShell()}))
	defer srv.Close()

	u := "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"
	_, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Origin": {"http://evil.example"}})
	if err == nil {
		t.Fatal("cross-origin upgrade was accepted; an attacker's page could open a shell")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Fatalf("want 403, got %v (err %v)", resp, err)
	}
}

// A browser following the tile's link must get a page, not gorilla's raw 400.
func TestPanelRootServesAPageNotAnUpgradeError(t *testing.T) {
	srv := httptest.NewServer(Handler(Options{Shell: testShell()}))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /term/ = %d, want 200", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "<title>Terminal") {
		t.Errorf("terminal panel root did not serve its page: %q", string(body)[:min(120, len(body))])
	}
}
