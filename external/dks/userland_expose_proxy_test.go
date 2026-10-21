package dks

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/qiangli/yoke/pkg/dagrun"
)

// proxyTrap stands in for the in-pod egress relay $HTTPS_PROXY points at: a
// listener that COUNTS every dial and answers nothing. A relay that honoured
// the proxy environment lands here instead of at its upstream.
func proxyTrap(t *testing.T) (string, *atomic.Int64) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen for the proxy trap: %v", err)
	}
	var dials atomic.Int64
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			dials.Add(1)
			_ = c.Close()
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return "http://" + ln.Addr().String(), &dials
}

// wsUpperServer is one upstream app route for the websocket-carried modes:
// it upgrades, upper-cases the four bytes it is sent, and holds the upgrade
// open long enough for the answer to be read. It serves both the tcp mode
// (no query) and the egress mode (?dst=host:port).
func wsUpperServer(t *testing.T, seen func(*http.Request)) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if seen != nil {
			seen(r)
		}
		if r.Header.Get(exposeTokenHeader) != testExposeToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		nc := websocket.NetConn(r.Context(), c, websocket.MessageBinary)
		buf := make([]byte, 4)
		if _, err := io.ReadFull(nc, buf); err != nil {
			return
		}
		_, _ = nc.Write([]byte(strings.ToUpper(string(buf))))
		time.Sleep(50 * time.Millisecond)
	}))
}

// RED (S487.2 live fix A): a relay's OWN upstream calls must never consult
// the proxy environment.
//
// What went wrong live: with an egress expose present the run env carries
// HTTPS_PROXY/HTTP_PROXY=http://127.0.0.1:3128 — the in-pod egress relay
// itself. Every relay started by `bashy dks relay --all` inherited it, so
// each relay dialled the SATELLITE through its own sibling, which refuses a
// host that is not in the egress allowlist: `http: proxy error: Forbidden`
// in the container log, and from the pod's side http/tcp empty, egress 403
// even for an allowed host, git 502.
//
// Two confounders make the end-to-end half of this test necessary but not
// sufficient, which is why the shape of the transport is pinned too: an
// upstream on 127.0.0.1 is exempt from $HTTP_PROXY by rule, and net/http
// resolves the proxy environment ONCE per process, so a test running after
// any other HTTP test sees a cached "no proxy" whatever it sets. A nil Proxy
// is the only shape that cannot be influenced by either.
func TestRelayUpstreamIgnoresProxyEnv(t *testing.T) {
	trap, trapDials := proxyTrap(t)
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "http_proxy", "https_proxy", "all_proxy"} {
		t.Setenv(k, trap)
	}
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")
	if req, err := http.NewRequest(http.MethodGet, "https://satellite.example.com/matrix/", nil); err == nil {
		if p, perr := http.ProxyFromEnvironment(req); perr == nil && p != nil {
			t.Logf("this process still resolves the proxy environment (%s): the end-to-end half bites too", p)
		}
	}

	// --- http mode: the ReverseProxy transport ---------------------------
	httpUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(exposeTokenHeader) != testExposeToken {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		fmt.Fprint(w, "ok")
	}))
	defer httpUpstream.Close()

	h, err := relayHTTPHandler(httpUpstream.URL+"/matrix/h/host-a/app/web/", testExposeToken, "")
	if err != nil {
		t.Fatalf("relayHTTPHandler: %v", err)
	}
	rp, ok := h.(*httputil.ReverseProxy)
	if !ok {
		t.Fatalf("relayHTTPHandler returned %T, want a *httputil.ReverseProxy", h)
	}
	if rp.Transport == nil {
		t.Fatal("the http relay has no Transport of its own: http.DefaultTransport honours $HTTPS_PROXY, so it would dial the satellite through the in-pod egress relay")
	}
	if tr, ok := rp.Transport.(*http.Transport); !ok {
		t.Errorf("the http relay's Transport is %T, want an *http.Transport with Proxy nil", rp.Transport)
	} else if tr.Proxy != nil {
		t.Error("the http relay's Transport has a Proxy func: it must be nil, never http.ProxyFromEnvironment")
	}

	front := httptest.NewServer(h)
	defer front.Close()
	resp, err := http.Get(front.URL + "/v1/messages")
	if err != nil {
		t.Fatalf("GET through the http relay: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || string(body) != "ok" {
		t.Fatalf("the http relay answered %d %q, want 200 ok", resp.StatusCode, body)
	}

	// --- the websocket dial options every bridged mode shares ------------
	opts := relayDialOptions(testExposeToken)
	if opts.HTTPClient == nil {
		t.Fatal("a relay websocket dial carries no HTTPClient: coder/websocket falls back to http.DefaultClient, which honours $HTTPS_PROXY")
	}
	if opts.HTTPClient.Timeout != 0 {
		t.Errorf("the relay dial client has Timeout %s: coder/websocket turns it into a deadline on the WHOLE bridged connection", opts.HTTPClient.Timeout)
	}
	if tr, ok := opts.HTTPClient.Transport.(*http.Transport); !ok {
		t.Errorf("the relay dial client's Transport is %T, want an *http.Transport with Proxy nil", opts.HTTPClient.Transport)
	} else if tr.Proxy != nil {
		t.Error("the relay dial client's Transport has a Proxy func: it must be nil, never http.ProxyFromEnvironment")
	}
	if got := opts.HTTPHeader.Get(exposeTokenHeader); got != testExposeToken {
		t.Errorf("the relay dial options carry %s = %q, want the expose token", exposeTokenHeader, got)
	}

	// --- tcp mode: one websocket upgrade per accepted connection ---------
	tcpUpstream := wsUpperServer(t, nil)
	defer tcpUpstream.Close()
	tcpLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tcpDone := make(chan error, 1)
	go func() {
		tcpDone <- relayTCPServe(ctx, tcpLn, tcpUpstream.URL+"/matrix/h/host-a/app/db/", testExposeToken)
	}()
	defer func() {
		cancel()
		_ = tcpLn.Close()
		select {
		case <-tcpDone:
		case <-time.After(5 * time.Second):
			t.Error("relayTCPServe did not return after cancel")
		}
	}()

	conn, err := net.Dial("tcp", tcpLn.Addr().String())
	if err != nil {
		t.Fatalf("dial the tcp relay: %v", err)
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := conn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through the tcp relay: %v", err)
	}
	back := make([]byte, 4)
	if _, err := io.ReadFull(conn, back); err != nil {
		t.Fatalf("read back through the tcp relay: %v", err)
	}
	if string(back) != "PING" {
		t.Errorf("the tcp relay bridged %q, want %q", back, "PING")
	}

	// --- egress mode: CONNECT over a ?dst= websocket ---------------------
	const dst = "allowed.example.com:443"
	var gotDst string
	egUpstream := wsUpperServer(t, func(r *http.Request) { gotDst = r.URL.Query().Get("dst") })
	defer egUpstream.Close()
	egLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	egDone := make(chan error, 1)
	go func() {
		egDone <- relayEgressServe(ctx, egLn, egUpstream.URL+"/matrix/h/host-a/app/proxy/", testExposeToken)
	}()
	defer func() {
		cancel()
		_ = egLn.Close()
		select {
		case <-egDone:
		case <-time.After(5 * time.Second):
			t.Error("relayEgressServe did not return after cancel")
		}
	}()

	egConn, err := net.Dial("tcp", egLn.Addr().String())
	if err != nil {
		t.Fatalf("dial the egress relay: %v", err)
	}
	defer egConn.Close()
	_ = egConn.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(egConn, "CONNECT "+dst+" HTTP/1.1\r\nHost: "+dst+"\r\n\r\n"); err != nil {
		t.Fatalf("write CONNECT: %v", err)
	}
	br := bufio.NewReader(egConn)
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read the CONNECT reply: %v", err)
	}
	if !strings.Contains(line, "200") {
		t.Fatalf("the egress relay answered CONNECT %q, want 200", strings.TrimSpace(line))
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read the 200 headers: %v", err)
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	if _, err := egConn.Write([]byte("ping")); err != nil {
		t.Fatalf("write through the egress relay: %v", err)
	}
	if _, err := io.ReadFull(br, back); err != nil {
		t.Fatalf("read back through the egress relay: %v", err)
	}
	if string(back) != "PING" {
		t.Errorf("the egress relay bridged %q, want %q", back, "PING")
	}
	if gotDst != dst {
		t.Errorf("the egress upgrade carried dst = %q, want %q", gotDst, dst)
	}

	if n := trapDials.Load(); n != 0 {
		t.Errorf("the relays dialled $HTTPS_PROXY %d times: a relay's own upstream call must never be proxied", n)
	}
}

// RED (S487.2 live fix A): the relays opt out of the proxy environment, the
// wrapped CMD does NOT. The child is the run, and a run that was given an
// egress expose is MEANT to reach the internet through it.
func TestRelayAllKeepsProxyEnvForChild(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "ok")
	}))
	defer app.Close()

	trap, _ := proxyTrap(t)
	t.Setenv("HTTPS_PROXY", trap)
	t.Setenv("HTTP_PROXY", trap)
	t.Setenv(relayChildProxyEnv, trap)

	ports := freeLoopbackPorts(t, 1)
	relays := []dagrun.Relay{
		{Name: "web", Mode: dagrun.RelayModeHTTP, Upstream: app.URL + "/matrix/h/host-a/app/web/", LocalPort: ports[0]},
	}
	spec, err := json.Marshal(relays)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv(dagrun.RelaysEnv, string(spec))
	t.Setenv(relays[0].TokenEnv(), testExposeToken)
	t.Setenv(relayChildPortsEnv, strconv.Itoa(ports[0]))

	et := newExposeTest(t)
	code, stdout, stderr := et.exec(t, append([]string{"relay", "--all", "--"}, helperChildArgv("TestRelayAllChild")...)...)
	if code != relayChildExitCode {
		t.Fatalf("relay --all exited %d, want the child's %d\nstdout %q\nstderr %q", code, relayChildExitCode, stdout, stderr)
	}
}
