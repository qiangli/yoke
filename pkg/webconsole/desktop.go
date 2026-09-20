// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package webconsole

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// The desktop page (sprint 222): a mesh peer's screen, in the console.
//
// Two routes under one prefix, dispatched the way /term/ is:
//
//	GET /desktop/<peer>      the page — vendored noVNC, a credentials card
//	    /desktop/<peer>/ws   the websocket, proxied to a MESH FORWARD of that
//	                         peer's outpost /desktop relay
//
// The forward is `outpost mesh listen <peer> desktop` through the same exec
// seam the Neighborhood uses: outpost prints the loopback address it bound,
// the console proxies the upgrade there byte for byte (the credentials frame
// first, then RFB — the relay's wire, unchanged), and closes the forward when
// the session ends. Nothing on this path names the cloudbox base: there is no
// cloudbox client here to fall back to, by design — the page IS the p2p path.
//
// The forward target must be loopback. That refusal is a real invariant (the
// exec seam is the only thing that could ever hand us somewhere else) and,
// with the fake-relay test, is the pin that cloudbox is off the path.

// peerIDPattern is a libp2p peer id as outpost prints it: base58btc, so the
// alphabet excludes 0, O, I and l. Anything else never reaches the exec seam.
var peerIDPattern = regexp.MustCompile(`^[1-9A-HJ-NP-Za-km-z]{20,128}$`)

// desktopPeer pulls <peer> out of /desktop/<peer>[/ws]; ok=false for any
// other shape.
func desktopPeer(p string) (peer string, ws bool, ok bool) {
	rest := strings.TrimPrefix(p, "/desktop/")
	if rest == p {
		return "", false, false
	}
	rest = strings.TrimSuffix(rest, "/")
	if strings.HasSuffix(rest, "/ws") {
		ws = true
		rest = strings.TrimSuffix(rest, "/ws")
	}
	if !peerIDPattern.MatchString(rest) {
		return "", false, false
	}
	return rest, ws, true
}

func (s *server) handleDesktop(w http.ResponseWriter, r *http.Request) {
	peer, ws, ok := desktopPeer(r.URL.Path)
	if !ok {
		http.Error(w, "desktop: /desktop/<peer-id>", http.StatusNotFound)
		return
	}
	if ws {
		s.proxyDesktop(w, r, peer)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.servePageFile(w, r, "desktop.html")
}

// meshForward opens a forward to (peer, service) and returns the loopback
// address outpost bound, plus the call that closes it. It is the ONLY place
// the desktop path talks to anything, and what it talks to is the local mesh
// agent.
func meshForward(ctx context.Context, peer, service string) (addr string, closeFn func(), err error) {
	octx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	raw, err := outpostJSON(octx, "mesh", "listen", peer, service)
	if err != nil {
		return "", nil, err
	}
	addr = strings.TrimSpace(string(raw))
	if addr == "" {
		return "", nil, errors.New("mesh listen printed no address")
	}
	closeFn = func() {
		cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer ccancel()
		if _, err := outpostJSON(cctx, "mesh", "unlisten", addr); err != nil {
			slog.Debug("desktop: mesh unlisten", "addr", addr, "err", err)
		}
	}
	if !isLoopbackHostPort(addr) {
		closeFn()
		return "", nil, fmt.Errorf("mesh forward bound %q, not loopback — refused", addr)
	}
	return addr, closeFn, nil
}

// isLoopbackHostPort is true for 127.0.0.0/8, ::1 and "localhost" with a port.
func isLoopbackHostPort(addr string) bool {
	host, port, err := net.SplitHostPort(addr)
	if err != nil || port == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// proxyDesktop bridges the browser's websocket to the peer's /desktop through
// a fresh mesh forward. httputil.ReverseProxy carries the Upgrade natively and
// returns only when the tunnel closes, which is when the forward is released.
func (s *server) proxyDesktop(w http.ResponseWriter, r *http.Request, peer string) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		http.Error(w, "desktop: websocket upgrade required", http.StatusBadRequest)
		return
	}
	addr, closeFn, err := meshForward(r.Context(), peer, "desktop")
	if err != nil {
		slog.Warn("desktop: mesh forward", "peer", peer, "err", err)
		http.Error(w, "desktop: cannot reach the peer over the mesh: "+err.Error(), http.StatusBadGateway)
		return
	}
	defer closeFn()

	target := &url.URL{Scheme: "http", Host: addr}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/desktop"
			pr.Out.URL.RawPath = ""
			pr.Out.URL.RawQuery = ""
			pr.Out.Host = addr
			// The relay is a websocket endpoint and nothing else; the console
			// session cookie is the console's, not the peer's.
			pr.Out.Header.Del("Cookie")
		},
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, dial string) (net.Conn, error) {
				if !isLoopbackHostPort(dial) { // belt and braces: the seam already checked
					return nil, fmt.Errorf("desktop: refusing non-loopback dial %q", dial)
				}
				return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, dial)
			},
			ResponseHeaderTimeout: 20 * time.Second,
		},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("desktop: relay", "peer", peer, "err", err)
			http.Error(w, "desktop: the peer's relay did not answer: "+err.Error(), http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}
