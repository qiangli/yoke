// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

//go:build verifydom

package webconsole

import (
	"encoding/binary"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/chromedp"
	"github.com/gorilla/websocket"
)

// fakeRFBRelay speaks the relay's wire to a real browser: the credentials
// frame first, then the server side of an auth-None RFB 3.8 handshake ending
// in a ServerInit that names the desktop, then one raw framebuffer update so
// noVNC has something to paint. Enough for the vendored client to fire
// `connect` and `desktopname` — the events the page renders from.
func fakeRFBRelay(t *testing.T) (*httptest.Server, func() string) {
	t.Helper()
	var mu sync.Mutex
	var creds string
	up := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }, Subprotocols: []string{"binary"}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		_, first, err := c.ReadMessage()
		if err != nil {
			return
		}
		mu.Lock()
		creds = string(first)
		mu.Unlock()
		send := func(b []byte) { _ = c.WriteMessage(websocket.BinaryMessage, b) }
		// A byte reader over frames: noVNC may split or batch its writes.
		var buf []byte
		read := func(n int) []byte {
			for len(buf) < n {
				_, msg, err := c.ReadMessage()
				if err != nil {
					return nil
				}
				buf = append(buf, msg...)
			}
			out := buf[:n]
			buf = buf[n:]
			return out
		}
		send([]byte("RFB 003.008\n"))
		if read(12) == nil {
			return
		}
		send([]byte{1, 1})
		if read(1) == nil {
			return
		}
		send([]byte{0, 0, 0, 0})
		if read(1) == nil {
			return
		}
		const w0, h0 = 64, 48
		name := "verifydom-desktop"
		init := make([]byte, 24)
		binary.BigEndian.PutUint16(init[0:2], w0)
		binary.BigEndian.PutUint16(init[2:4], h0)
		// pixel format: 32 bpp, depth 24, big-endian 0, true-colour 1, max 255×3, shifts 16/8/0
		copy(init[4:20], []byte{32, 24, 0, 1, 0, 255, 0, 255, 0, 255, 16, 8, 0, 0, 0, 0})
		binary.BigEndian.PutUint32(init[20:24], uint32(len(name)))
		send(append(init, name...))
		// One raw update covering the screen, then drain whatever the client
		// sends until it leaves.
		upd := make([]byte, 4+12+w0*h0*4)
		upd[0] = 0
		binary.BigEndian.PutUint16(upd[2:4], 1)
		binary.BigEndian.PutUint16(upd[8:10], w0)
		binary.BigEndian.PutUint16(upd[10:12], h0)
		for i := 16; i < len(upd); i += 4 {
			upd[i], upd[i+1], upd[i+2] = 0x20, 0x60, 0xc0
		}
		send(upd)
		for {
			if _, _, err := c.ReadMessage(); err != nil {
				return
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, func() string { mu.Lock(); defer mu.Unlock(); return creds }
}

// The desktop page, in a real browser, against a fake relay behind the mesh
// seam: the vendored noVNC module loads (an ES-module import that fails is a
// blank page, whatever the served bytes say), the credentials card submits,
// the socket rides the proxy, the client connects and shows the desktop's
// name, and nothing throws.
func TestDOMDesktopConnectsThroughTheMeshProxy(t *testing.T) {
	relay, creds := fakeRFBRelay(t)
	stubMeshForward(t, strings.TrimPrefix(relay.URL, "http://"))
	base, ctx, errs := domEnv(t, Options{})

	var status, state, name string
	var formShown bool
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/desktop/"+testPeer),
		chromedp.WaitVisible(`#creds`, chromedp.ByID),
		// The form is static HTML and visible before the module script has
		// run; wait for the script's own ready mark or the click is a native
		// submit.
		chromedp.Poll(`document.body.dataset.desktopReady === "1"`, nil, chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.SendKeys(`#password`, "hunter2x", chromedp.ByID),
		// Click, not Submit: form.submit() skips the submit event and would
		// navigate away — exactly the failure the handler's preventDefault
		// exists to stop.
		chromedp.Click(`#creds button[type=submit]`, chromedp.ByQuery),
		chromedp.Poll(`document.body.dataset.desktop === "connected" && !!document.body.dataset.desktopName`, nil,
			chromedp.WithPollingTimeout(20*time.Second)),
		chromedp.Evaluate(`document.getElementById("status").textContent`, &status),
		chromedp.Evaluate(`document.body.dataset.desktop`, &state),
		chromedp.Evaluate(`document.body.dataset.desktopName`, &name),
		// A layout fact: the card is display:flex, which outranks [hidden]
		// unless the stylesheet says otherwise. Ask the renderer, not the DOM.
		chromedp.Evaluate(`getComputedStyle(document.getElementById("creds")).display !== "none"`, &formShown),
	); err != nil {
		t.Fatalf("chromedp: %v (errors: %v)", err, errs())
	}
	assertNoJSErrors(t, "desktop", errs())
	if state != "connected" || name != "verifydom-desktop" || status != "verifydom-desktop" {
		t.Fatalf("state=%q name=%q status=%q", state, name, status)
	}
	if formShown {
		t.Fatal("the credentials card is still rendered beside the connected desktop")
	}
	if got := creds(); got != `{"user":"","password":"hunter2x"}` {
		t.Fatalf("relay saw credentials %q", got)
	}
}

// The Neighborhood card for a mesh peer is the Desktop action: a link to the
// peer's desktop page.
func TestDOMNeighborhoodCardOpensDesktop(t *testing.T) {
	stubNeighborhoodOnePeer(t)
	base, ctx, errs := domEnv(t, Options{})
	var href, action string
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/"),
		chromedp.WaitVisible(`#neighborhood-section a.tile.has-desktop`, chromedp.ByQuery),
		chromedp.Evaluate(`document.querySelector("#neighborhood-section a.tile.has-desktop").getAttribute("href")`, &href),
		chromedp.Evaluate(`document.querySelector("#neighborhood-section a.tile.has-desktop .action").textContent`, &action),
	); err != nil {
		t.Fatalf("chromedp: %v (errors: %v)", err, errs())
	}
	assertNoJSErrors(t, "neighborhood", errs())
	if !strings.HasSuffix(href, "/desktop/"+testPeer) || !strings.HasPrefix(action, "Desktop") {
		t.Fatalf("href=%q action=%q", href, action)
	}
}
