package sshserver

import (
	"context"
	"golang.org/x/crypto/ssh"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
)

type tcpipForwardMsg struct {
	BindAddr string
	BindPort uint32
}

// tcpipForwardReplyMsg is the success reply payload when the client asked
// for BindPort == 0 (let the server pick) — we tell it which port we
// actually bound. RFC 4254 §7.1.
type tcpipForwardReplyMsg struct {
	BoundPort uint32
}

// forwardedTCPIPMsg is the channel-open payload the agent sends when a
// `tcpip-forward` listener accepts a connection and we push it back to
// the client as a `forwarded-tcpip` channel. RFC 4254 §7.2.
type forwardedTCPIPMsg struct {
	DestAddr string
	DestPort uint32
	OrigAddr string
	OrigPort uint32
}

// allowTCPIPForwardBind restricts which bind addresses the agent will
// accept for a `tcpip-forward` listener. Loopback only, mirroring the
// `allowDirectTCPIPDest` posture for `direct-tcpip`. Empty BindAddr is
// treated as "127.0.0.1" — that's what openssh's sshd does too, and a
// 0.0.0.0 bind on the home host would expose the operator's laptop to
// the agent's LAN, which is outside the trust model.
func allowTCPIPForwardBind(host string) bool {
	h := strings.ToLower(strings.TrimSpace(host))
	switch h {
	case "", "localhost", "127.0.0.1", "::1":
		return true
	}
	return false
}

// canonicalBindAddr maps the loopback-equivalent inputs to a single
// representation so listener-registry keys are stable across a
// `tcpip-forward` / `cancel-tcpip-forward` pair that uses different
// spellings.
func canonicalBindAddr(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if h == "" || h == "localhost" {
		return "127.0.0.1"
	}
	return h
}

// forwardRegistry tracks listeners spawned by `tcpip-forward` requests on
// one SSH connection. Keyed by "addr:port" of the bound listener. Lifetime
// is per-SSH-conn — the dispatcher's deferred closeAll() in sshHandler
// covers serverConn teardown.
type forwardRegistry struct {
	mu  sync.Mutex
	lns map[string]net.Listener
}

func newForwardRegistry() *forwardRegistry {
	return &forwardRegistry{lns: make(map[string]net.Listener)}
}

func (r *forwardRegistry) add(key string, ln net.Listener) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lns[key] = ln
}

func (r *forwardRegistry) remove(key string) net.Listener {
	r.mu.Lock()
	defer r.mu.Unlock()
	ln := r.lns[key]
	delete(r.lns, key)
	return ln
}

func (r *forwardRegistry) closeAll() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, ln := range r.lns {
		_ = ln.Close()
		delete(r.lns, k)
	}
}

func fwdKey(addr string, port uint32) string {
	return net.JoinHostPort(addr, strconv.Itoa(int(port)))
}

// handleTCPIPForward services one `tcpip-forward` global request: bind a
// loopback listener at the requested port (or one picked by the OS when
// BindPort == 0), register it, and run an accept loop that pushes each
// accepted connection back to the SSH client as a `forwarded-tcpip`
// channel.
//
// Trust model: same OS-password gate that already protects session and
// direct-tcpip channels. The bind is loopback-only by policy regardless
// of `allowRemoteForward`, so widening reach to the LAN is impossible
// from this surface.
func handleTCPIPForward(ctx context.Context, sc *ssh.ServerConn, fwds *forwardRegistry, allowRemoteForward bool, req *ssh.Request) {
	if !allowRemoteForward {
		_ = req.Reply(false, nil)
		return
	}
	var msg tcpipForwardMsg
	if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
		slog.Warn("tcpip-forward: bad payload", "err", err)
		_ = req.Reply(false, nil)
		return
	}
	if !allowTCPIPForwardBind(msg.BindAddr) {
		slog.Info("tcpip-forward: refused non-loopback bind",
			"bind_addr", msg.BindAddr, "bind_port", msg.BindPort)
		_ = req.Reply(false, nil)
		return
	}
	bindAddr := canonicalBindAddr(msg.BindAddr)

	ln, err := net.Listen("tcp", net.JoinHostPort(bindAddr, strconv.Itoa(int(msg.BindPort))))
	if err != nil {
		slog.Info("tcpip-forward: listen failed", "bind", bindAddr, "port", msg.BindPort, "err", err)
		_ = req.Reply(false, nil)
		return
	}
	boundPort := uint32(ln.Addr().(*net.TCPAddr).Port)
	key := fwdKey(bindAddr, boundPort)
	fwds.add(key, ln)

	// RFC 4254 §7.1: reply payload carries the bound port only when the
	// client asked the server to pick (BindPort == 0). Replying with no
	// payload when BindPort != 0 keeps openssh-clients happy.
	if msg.BindPort == 0 {
		_ = req.Reply(true, ssh.Marshal(tcpipForwardReplyMsg{BoundPort: boundPort}))
	} else {
		_ = req.Reply(true, nil)
	}
	slog.Info("tcpip-forward: listening", "bind", bindAddr, "port", boundPort)

	// clientDestAddr / clientDestPort are what we'll stuff into the
	// `forwarded-tcpip` channel-open payload when a connection arrives.
	// They must match what the client recorded at tcpip-forward time —
	// OpenSSH's client looks up its forward table with a `strcmp` on
	// listen_address and `==` on listen_port. So we echo the ORIGINAL
	// bind_addr the client sent (NOT canonicalBindAddr — that would
	// turn "" into "127.0.0.1" and the lookup would fail with the
	// `WARNING: Server requests forwarding for unknown listen_port`
	// noise that breaks the channel). For BindPort, the only time
	// boundPort can differ from msg.BindPort is the ephemeral-port
	// case (BindPort == 0) — in which case the client recorded the
	// port we echoed back via tcpipForwardReplyMsg, so we send that.
	clientDestAddr := msg.BindAddr
	clientDestPort := msg.BindPort
	if clientDestPort == 0 {
		clientDestPort = boundPort
	}

	go func() {
		defer func() {
			_ = ln.Close()
			// Remove from registry whether we got here via cancel
			// (registry already empty for this key) or via accept-loop
			// error — keeps the map from leaking on accept failures.
			fwds.remove(key)
		}()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go bridgeForwardedTCPIP(ctx, sc, c, clientDestAddr, clientDestPort)
		}
	}()
}

// handleCancelTCPIPForward services `cancel-tcpip-forward` by closing the
// matching listener (which trips its accept loop's exit path).
func handleCancelTCPIPForward(fwds *forwardRegistry, req *ssh.Request) {
	var msg tcpipForwardMsg
	if err := ssh.Unmarshal(req.Payload, &msg); err != nil {
		_ = req.Reply(false, nil)
		return
	}
	bindAddr := canonicalBindAddr(msg.BindAddr)
	key := fwdKey(bindAddr, msg.BindPort)
	if ln := fwds.remove(key); ln != nil {
		_ = ln.Close()
		_ = req.Reply(true, nil)
		return
	}
	_ = req.Reply(false, nil)
}

// bridgeForwardedTCPIP opens a `forwarded-tcpip` channel back to the SSH
// client and byte-bridges it to the accepted local connection. Mirror
// image of handleDirectTCPIP — the io.Copy pattern is identical.
//
// destAddr/destPort MUST be the original values the client requested in
// its tcpip-forward — the client uses them as the lookup key into its
// remote-forward table (`strcmp` on address, `==` on port). See
// handleTCPIPForward for the reasoning.
func bridgeForwardedTCPIP(ctx context.Context, sc *ssh.ServerConn, c net.Conn, destAddr string, destPort uint32) {
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	origHost, origPortStr, _ := net.SplitHostPort(c.RemoteAddr().String())
	origPort, _ := strconv.Atoi(origPortStr)
	payload := ssh.Marshal(forwardedTCPIPMsg{
		DestAddr: destAddr,
		DestPort: destPort,
		OrigAddr: origHost,
		OrigPort: uint32(origPort),
	})
	ch, chReqs, err := sc.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		slog.Info("forwarded-tcpip: channel open rejected",
			"dest", destAddr, "port", destPort, "err", err)
		return
	}
	defer ch.Close()
	// `forwarded-tcpip` channels never carry channel requests — drain to
	// keep the crypto/ssh request goroutine from piling up.
	go ssh.DiscardRequests(chReqs)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(c, ch)
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(ch, c)
		_ = ch.CloseWrite()
	}()
	wg.Wait()
}
