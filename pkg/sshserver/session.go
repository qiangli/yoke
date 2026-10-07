package sshserver

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/pkg/sftp"
	"golang.org/x/crypto/ssh"
)

func serveSession(ctx context.Context, ch ssh.Channel, reqs <-chan *ssh.Request, execute func(context.Context, string, io.Reader, io.Writer, io.Writer) uint32) {
	defer ch.Close()
	defer func() {
		if r := recover(); r != nil {
			slog.Error("ssh session panic", "panic", r)
		}
	}()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = ch.Close() })
	defer stop()
	for req := range reqs {
		switch req.Type {
		case "exec":
			var msg struct{ Command string }
			if execute == nil || ssh.Unmarshal(req.Payload, &msg) != nil {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			// Keep reading requests so closing just this session cancels its command.
			go func() { ssh.DiscardRequests(reqs); cancel() }()
			status := execute(ctx, msg.Command, ch, ch, ch.Stderr())
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			return
		case "subsystem":
			var msg struct{ Name string }
			if ssh.Unmarshal(req.Payload, &msg) != nil || msg.Name != "sftp" {
				_ = req.Reply(false, nil)
				continue
			}
			_ = req.Reply(true, nil)
			srv, err := sftp.NewServer(ch)
			if err != nil {
				return
			}
			stop := context.AfterFunc(ctx, func() { _ = srv.Close() })
			status := uint32(0)
			if err = srv.Serve(); err != nil && !errors.Is(err, io.EOF) {
				status = 1
			}
			stop()
			_, _ = ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{status}))
			_ = srv.Close()
			return
		default:
			// There is no hidden interpreter or PTY here. Hosts that offer interactive
			// shells supply their own Session handler through ServeConn.
			if req.WantReply {
				_ = req.Reply(false, nil)
			}
		}
	}
}

func serveLoopbackChannel(ctx context.Context, newCh ssh.NewChannel) {
	if newCh.ChannelType() != "direct-tcpip" {
		_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel")
		return
	}
	var msg struct {
		Host       string
		Port       uint32
		OriginHost string
		OriginPort uint32
	}
	if ssh.Unmarshal(newCh.ExtraData(), &msg) != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "malformed direct-tcpip payload")
		return
	}
	host := strings.ToLower(strings.TrimSpace(msg.Host))
	if host != "localhost" && host != "127.0.0.1" && host != "::1" {
		_ = newCh.Reject(ssh.Prohibited, "destination must be loopback")
		return
	}
	target := net.JoinHostPort(canonicalBindAddr(host), strconv.Itoa(int(msg.Port)))
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", target)
	if err != nil {
		_ = newCh.Reject(ssh.ConnectionFailed, "connect failed")
		return
	}
	defer conn.Close()
	ch, reqs, err := newCh.Accept()
	if err != nil {
		return
	}
	defer ch.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close(); _ = ch.Close() })
	defer stop()
	go ssh.DiscardRequests(reqs)
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(conn, ch)
		if tcp, ok := conn.(*net.TCPConn); ok {
			_ = tcp.CloseWrite()
		}
	}()
	go func() { defer wg.Done(); _, _ = io.Copy(ch, conn); _ = ch.CloseWrite() }()
	wg.Wait()
}
