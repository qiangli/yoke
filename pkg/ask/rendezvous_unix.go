//go:build !windows

package ask

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"syscall"
)

// listenSocket binds the answer socket.
//
// The umask is set ACROSS the bind rather than chmod'ing afterwards. Bind-then-
// chmod leaves a window in which the socket exists and is world-accessible, and a
// race that small is still a race. The explicit Chmod after it is belt and braces —
// umask can only clear bits, but an exotic filesystem should not get to decide
// the permissions on a channel carrying a credential.
//
// Adapted from bashy's warm-session socket (internal/agentos/session), which
// settled these same questions; it lives there behind internal/, so the logic is
// re-stated here rather than imported.
func listenSocket(path string) (listener, error) {
	// A stale socket from a crashed request would make bind fail with EADDRINUSE.
	// The containing directory is ours and per-request, so nothing else can be
	// listening here.
	_ = os.Remove(path)

	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		// Some filesystems cannot host a socket, and the path length limit
		// (~104 bytes) can bite on a deep XDG_RUNTIME_DIR. Both mean "use the file
		// channel", not "fail the request".
		return nil, fmt.Errorf("%w: %v", errNoSocket, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, fmt.Errorf("%w: securing %s: %v", errNoSocket, path, err)
	}
	return &socketListener{ln: ln, path: path}, nil
}

type socketListener struct {
	ln   net.Listener
	path string
}

func (s *socketListener) Wait(ctx context.Context) ([]byte, error) {
	// net.Listener has no context-aware Accept, so cancellation is delivered by
	// closing the listener out from under it — Accept then returns immediately.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			s.ln.Close()
		case <-done:
		}
	}()

	for {
		conn, err := s.ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		b, err := readAnswer(conn)
		conn.Close()
		if err != nil {
			// A connection that fails authorization or arrives malformed must not
			// kill the request — the human may simply be about to answer it
			// correctly. Keep listening.
			continue
		}
		return b, nil
	}
}

func (s *socketListener) Close() error {
	err := s.ln.Close()
	_ = os.Remove(s.path)
	return err
}

// readAnswer authorizes the peer and reads one envelope.
//
// The uid check is the authoritative control and it runs FIRST. Filesystem
// permissions answer "who could open this path"; a kernel-supplied peer credential
// answers "who is actually talking to me", and it cannot be forged by the caller.
// On macOS in particular a unix socket's own mode is not consulted at connect time
// at all, so without this the only protection would be the containing directory.
//
// It fails CLOSED: if we cannot establish who is calling, we do not take their
// answer. Accepting an unidentifiable peer would let any local process feed a
// value into a prompt the human is about to be shown as legitimate.
func readAnswer(conn net.Conn) ([]byte, error) {
	if err := authorizePeer(conn); err != nil {
		return nil, err
	}
	var env answerEnvelope
	dec := json.NewDecoder(conn)
	if err := dec.Decode(&env); err != nil {
		return nil, err
	}
	return json.Marshal(env)
}

func sendSocket(path string, payload []byte) error {
	conn, err := net.Dial("unix", path)
	if err != nil {
		return fmt.Errorf("%w: %v", errNoSocket, err)
	}
	defer conn.Close()
	if _, err := conn.Write(payload); err != nil {
		return err
	}
	return nil
}

// authorizePeer rejects a connection from any uid but our own.
//
// Same-uid is the only trust boundary that means anything here: a process already
// running as us can read our files and ptrace us, so it could obtain the value
// anyway. What this stops is a DIFFERENT local user answering — or, on a machine
// where the directory permissions were somehow wrong, harvesting.
func authorizePeer(conn net.Conn) error {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("ask: refusing an answer from a non-unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return err
	}
	var (
		uid   int
		inner error
	)
	if err := raw.Control(func(fd uintptr) { uid, inner = peerUIDFromFD(fd) }); err != nil {
		return err
	}
	if inner != nil {
		return fmt.Errorf("ask: cannot identify the answering process: %w", inner)
	}
	if me := os.Getuid(); uid != me {
		return fmt.Errorf("ask: refusing an answer from uid %d (this request belongs to uid %d)", uid, me)
	}
	return nil
}
