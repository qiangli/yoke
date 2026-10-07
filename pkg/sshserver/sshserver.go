// Package sshserver provides the shared SSH transport, SFTP, and loopback
// forwarding used by userland and service hosts. Shell execution belongs to
// the embedding host; this package contains no interpreter.
package sshserver

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os/user"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

// Config supplies a standalone key-authenticated SSH listener. Only the current
// OS user can authenticate. AuthorizedKeysPath defaults to ~/.ssh/authorized_keys.
type Config struct {
	AuthorizedKeysPath string
	HostKey            ssh.Signer
	// Execute runs a command using the embedding host's shell. A nil callback
	// disables exec; SFTP and loopback forwarding remain available.
	Execute func(context.Context, string, io.Reader, io.Writer, io.Writer) uint32
}

// Serve accepts connections until cancellation or listener failure. It owns
// the listener and all accepted connections, including incomplete handshakes.
func Serve(ctx context.Context, ln net.Listener, cfg Config) error {
	if ctx == nil {
		return errors.New("sshserver: nil context")
	}
	if ln == nil {
		return errors.New("sshserver: nil listener")
	}
	if cfg.HostKey == nil {
		return errors.New("sshserver: nil host key")
	}
	me, err := user.Current()
	if err != nil || me.Username == "" {
		return errors.New("sshserver: current OS user unavailable")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer ln.Close()
	stop := context.AfterFunc(ctx, func() { _ = ln.Close() })
	defer stop()
	config := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(meta ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !sameUser(meta.User(), me.Username) || !AuthorizedKeyAllowed(strings.TrimSpace(cfg.AuthorizedKeysPath), key) {
				return nil, errors.New("public-key authentication rejected")
			}
			return nil, nil
		},
	}
	config.AddHostKey(cfg.HostKey)
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}
		go func() {
			_ = ServeConn(ctx, conn, config, Handlers{
				AllowRemoteForward: true,
				Session: func(ctx context.Context, _ *ssh.ServerConn, ch ssh.Channel, reqs <-chan *ssh.Request) {
					serveSession(ctx, ch, reqs, cfg.Execute)
				},
				Channel: serveLoopbackChannel,
			})
		}()
	}
}

// ListenAndServe accepts host:port or a bare port, and owns its listener.
func ListenAndServe(ctx context.Context, address string, cfg Config) error {
	address = strings.TrimSpace(address)
	if address == "" {
		return errors.New("sshserver: empty listen address")
	}
	if !strings.Contains(address, ":") {
		address = ":" + address
	}
	ln, err := net.Listen("tcp", address)
	if err != nil {
		return fmt.Errorf("sshserver: listen %s: %w", address, err)
	}
	defer ln.Close()
	return Serve(ctx, ln, cfg)
}

// Handlers lets service hosts reuse the SSH connection and forwarding lifecycle
// with their existing authentication, session, and destination policies.
type Handlers struct {
	AllowRemoteForward bool
	Session            func(context.Context, *ssh.ServerConn, ssh.Channel, <-chan *ssh.Request)
	Channel            func(context.Context, ssh.NewChannel)
}

// ServeConn performs a handshake and routes channels until the connection ends.
// It owns conn. All callbacks receive a context canceled on disconnect.
func ServeConn(ctx context.Context, conn net.Conn, config *ssh.ServerConfig, handlers Handlers) error {
	defer conn.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	// Bound unauthenticated clients even when they never send a protocol banner.
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	sc, chans, reqs, err := ssh.NewServerConn(conn, config)
	if err != nil {
		return err
	}
	_ = conn.SetDeadline(time.Time{})
	defer sc.Close()
	fwds := newForwardRegistry()
	// Wait for the global request loop before closing listeners so a concurrent
	// tcpip-forward cannot register a listener after shutdown has swept the map.
	requestsDone := make(chan struct{})
	defer func() { _ = sc.Close(); <-requestsDone; fwds.closeAll() }()
	go func() {
		defer close(requestsDone)
		for req := range reqs {
			switch req.Type {
			case "tcpip-forward":
				handleTCPIPForward(ctx, sc, fwds, handlers.AllowRemoteForward, req)
			case "cancel-tcpip-forward":
				handleCancelTCPIPForward(fwds, req)
			default:
				if req.WantReply {
					_ = req.Reply(false, nil)
				}
			}
		}
	}()
	for newCh := range chans {
		if newCh.ChannelType() == "session" && handlers.Session != nil {
			ch, requests, err := newCh.Accept()
			if err != nil {
				continue
			}
			go handlers.Session(ctx, sc, ch, requests)
		} else if handlers.Channel != nil {
			go handlers.Channel(ctx, newCh)
		} else {
			_ = newCh.Reject(ssh.UnknownChannelType, "unsupported channel")
		}
	}
	return nil
}

func sameUser(submitted, canonical string) bool {
	submitted, canonical = strings.TrimSpace(submitted), strings.TrimSpace(canonical)
	if submitted == "" || canonical == "" {
		return false
	}
	bare := func(s string) string {
		if i := strings.LastIndexByte(s, '\\'); i >= 0 {
			return s[i+1:]
		}
		if i := strings.IndexByte(s, '@'); i >= 0 {
			return s[:i]
		}
		return s
	}
	return strings.EqualFold(submitted, canonical) || strings.EqualFold(bare(submitted), bare(canonical))
}
