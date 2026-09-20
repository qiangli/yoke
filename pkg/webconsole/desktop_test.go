// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

const testPeer = "12D3KooWRRq3Lxgb4rtb3VyptQj9RaaQT41h5VjioVpNs3BNdgUs"

func TestDesktopPeerParsing(t *testing.T) {
	cases := []struct {
		path string
		peer string
		ws   bool
		ok   bool
	}{
		{"/desktop/" + testPeer, testPeer, false, true},
		{"/desktop/" + testPeer + "/", testPeer, false, true},
		{"/desktop/" + testPeer + "/ws", testPeer, true, true},
		{"/desktop/", "", false, false},
		{"/desktop/not a peer", "", false, false},
		{"/desktop/0OIl0OIl0OIl0OIl0OIl0OIl", "", false, false}, // outside base58
		{"/desktop/../etc", "", false, false},
		{"/term/ws", "", false, false},
	}
	for _, c := range cases {
		peer, ws, ok := desktopPeer(c.path)
		if peer != c.peer || ws != c.ws || ok != c.ok {
			t.Errorf("desktopPeer(%q) = %q,%v,%v want %q,%v,%v", c.path, peer, ws, ok, c.peer, c.ws, c.ok)
		}
	}
	for addr, want := range map[string]bool{
		"127.0.0.1:5901": true, "[::1]:5901": true, "localhost:1": true, "127.9.9.9:80": true,
		"10.0.0.5:5900": false, "203.0.113.1:9": false, "127.0.0.1": false, "example.com:80": false, "": false,
	} {
		if got := isLoopbackHostPort(addr); got != want {
			t.Errorf("isLoopbackHostPort(%q) = %v", addr, got)
		}
	}
}

// The page is served for a peer id and gated like every console page: a LAN
// caller without a session is sent to login, loopback gets the page.
func TestDesktopPageServedAndGated(t *testing.T) {
	h := newTestHandler(t, Options{})
	w := do(h, "GET", "/desktop/"+testPeer, "127.0.0.1:4444", nil)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "desktop.js") || !strings.Contains(w.Body.String(), `id="creds"`) {
		t.Fatalf("loopback page = %d %q", w.Code, firstLine(w.Body.String(), nil))
	}
	gated := newTestHandler(t, Options{RequireLogin: true})
	for _, peer := range []string{"10.1.2.3:5555", "127.0.0.1:4444"} {
		if w := do(gated, "GET", "/desktop/"+testPeer, peer, nil); w.Code == http.StatusOK {
			t.Fatalf("%s without a session got the page; the login ladder must gate /desktop", peer)
		}
	}
	if w := do(h, "GET", "/desktop/nope", "127.0.0.1:4444", nil); w.Code != http.StatusNotFound {
		t.Fatalf("bad peer = %d, want 404", w.Code)
	}
	// noVNC ships with its license text, unmodified from the tarball.
	for _, p := range []string{"/vendor/novnc/core/rfb.js", "/vendor/novnc/LICENSE.txt", "/vendor/novnc/docs/LICENSE.MPL-2.0"} {
		if w := do(h, "GET", p, "127.0.0.1:4444", nil); w.Code != http.StatusOK {
			t.Errorf("GET %s = %d", p, w.Code)
		}
	}
}

// fakeRelay stands in for a peer's outpost /desktop: it answers the websocket
// upgrade, records the first (credentials) frame, and echoes every frame after
// it — enough to prove the console proxies the relay's wire byte for byte.
type fakeRelay struct {
	srv   *httptest.Server
	mu    sync.Mutex
	creds string
	paths []string
}

func newFakeRelay(t *testing.T) *fakeRelay {
	t.Helper()
	f := &fakeRelay{}
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.mu.Unlock()
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, first, err := c.ReadMessage()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.creds = string(first)
		f.mu.Unlock()
		_ = c.WriteMessage(websocket.BinaryMessage, []byte("RFB 003.008\n"))
		for {
			mt, msg, err := c.ReadMessage()
			if err != nil {
				return
			}
			if err := c.WriteMessage(mt, msg); err != nil {
				return
			}
		}
	}))
	t.Cleanup(f.srv.Close)
	return f
}

// The seam: `mesh listen <peer> desktop` answers with the relay's loopback
// address; `mesh unlisten <addr>` is recorded so the test can see the forward
// released. No other outpost verb is ever asked for on this path.
func stubMeshForward(t *testing.T, listenAddr string) (calls *[]string) {
	t.Helper()
	prev := outpostJSON
	t.Cleanup(func() { outpostJSON = prev })
	fake := filepath.Join(t.TempDir(), "outpost")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BIN", fake)
	var mu sync.Mutex
	rec := []string{}
	calls = &rec
	outpostJSON = func(_ context.Context, args ...string) ([]byte, error) {
		mu.Lock()
		defer mu.Unlock()
		rec = append(rec, strings.Join(args, " "))
		switch {
		case len(args) == 4 && args[0] == "mesh" && args[1] == "listen" && args[2] == testPeer && args[3] == "desktop":
			return []byte(listenAddr + "\n"), nil
		case len(args) == 3 && args[0] == "mesh" && args[1] == "unlisten":
			return []byte("closed listener " + args[2] + "\n"), nil
		}
		return nil, errors.New("unexpected " + strings.Join(args, " "))
	}
	return calls
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The websocket rides a mesh forward and nothing else: the console asks the
// local agent for `mesh listen <peer> desktop`, proxies the upgrade to the
// loopback address it printed (path rewritten to the relay's /desktop, the
// credentials frame delivered first, RFB bytes echoed back unchanged), and
// releases the forward when the browser goes away. There is no cloudbox client
// on this path to fall back to — the only addresses ever dialled are the
// seam's, and the next test shows a non-loopback one is refused.
func TestDesktopSocketRidesTheMeshForward(t *testing.T) {
	relay := newFakeRelay(t)
	calls := stubMeshForward(t, strings.TrimPrefix(relay.srv.URL, "http://"))
	h := newTestHandler(t, Options{})
	srv := httptest.NewServer(h)
	defer srv.Close()

	c, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/desktop/"+testPeer+"/ws", http.Header{"Sec-WebSocket-Protocol": {"binary"}})
	if err != nil {
		t.Fatalf("dial: %v (resp %v)", err, resp)
	}
	if err := c.WriteMessage(websocket.TextMessage, []byte(`{"user":"","password":"hunter2x"}`)); err != nil {
		t.Fatal(err)
	}
	_, ver, err := c.ReadMessage()
	if err != nil || string(ver) != "RFB 003.008\n" {
		t.Fatalf("first relay frame = %q, %v", ver, err)
	}
	if err := c.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3, 250}); err != nil {
		t.Fatal(err)
	}
	if _, echo, err := c.ReadMessage(); err != nil || string(echo) != string([]byte{1, 2, 3, 250}) {
		t.Fatalf("echo = %v, %v", echo, err)
	}
	c.Close()

	relay.mu.Lock()
	creds, paths := relay.creds, relay.paths
	relay.mu.Unlock()
	if creds != `{"user":"","password":"hunter2x"}` {
		t.Fatalf("relay saw credentials frame %q", creds)
	}
	if len(paths) != 1 || paths[0] != "/desktop" {
		t.Fatalf("relay saw paths %v, want [/desktop]", paths)
	}
	waitFor(t, "mesh unlisten", func() bool {
		for _, c := range *calls {
			if strings.HasPrefix(c, "mesh unlisten ") {
				return true
			}
		}
		return false
	})
	if (*calls)[0] != "mesh listen "+testPeer+" desktop" {
		t.Fatalf("first outpost call = %q", (*calls)[0])
	}
}

// A forward that is not loopback is refused before anything is dialled — the
// invariant that keeps this path local to the mesh agent — and the forward is
// released anyway.
func TestDesktopSocketRefusesNonLoopbackForward(t *testing.T) {
	calls := stubMeshForward(t, "203.0.113.5:5900")
	h := newTestHandler(t, Options{})
	srv := httptest.NewServer(h)
	defer srv.Close()
	_, resp, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(srv.URL, "http")+"/desktop/"+testPeer+"/ws", nil)
	if err == nil {
		t.Fatal("expected the upgrade to be refused")
	}
	if resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status = %v", resp)
	}
	want := []string{"mesh listen " + testPeer + " desktop", "mesh unlisten 203.0.113.5:5900"}
	if strings.Join(*calls, "|") != strings.Join(want, "|") {
		t.Fatalf("outpost calls = %v, want %v", *calls, want)
	}
	// Without the upgrade header the socket route is not a page either.
	if w := do(h, "GET", "/desktop/"+testPeer+"/ws", "127.0.0.1:1", nil); w.Code != http.StatusBadRequest {
		t.Fatalf("plain GET on /ws = %d, want 400", w.Code)
	}
}

// stubNeighborhoodOnePeer makes the Neighborhood list one direct LAN mesh peer
// (testPeer) and nothing from mDNS.
func stubNeighborhoodOnePeer(t *testing.T) {
	t.Helper()
	prev := outpostJSON
	t.Cleanup(func() { outpostJSON = prev })
	fake := filepath.Join(t.TempDir(), "outpost")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OUTPOST_BIN", fake)
	outpostJSON = func(_ context.Context, args ...string) ([]byte, error) {
		switch strings.Join(args, " ") {
		case "mesh status --json":
			return []byte(`{"status":{"peers":[{"id":"` + testPeer + `","name":"peerbox","direct":true,"link_class":"lan","remote":["/ip4/10.0.0.45/udp/2/quic-v1"]}]}}`), nil
		case "scan --json --timeout 2s":
			return []byte(`[]`), nil
		}
		return nil, errors.New("unexpected " + strings.Join(args, " "))
	}
}

// The Neighborhood JSON carries the peer id the card links to.
func TestNeighborhoodCarriesPeerIDForDesktop(t *testing.T) {
	stubNeighborhoodOnePeer(t)
	v := neighborhood(context.Background())
	if len(v.Hosts) != 1 || v.Hosts[0].PeerID != testPeer || v.Hosts[0].Name != "peerbox" {
		t.Fatalf("hosts = %+v", v.Hosts)
	}
}
