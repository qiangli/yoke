package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// NoSocketEnv forces the file-based answer channel on every platform.
//
// It exists for tests. The socket path and the file path have genuinely different
// failure modes, and without a way to force the degraded one it would only ever be
// exercised on the Windows CI leg — which is to say, rarely, and never by the
// person changing it.
const NoSocketEnv = "BASHY_ASK_NO_SOCKET"

// errNoSocket means the socket transport is unavailable and the caller should fall
// back to the file channel.
var errNoSocket = errors.New("ask: unix socket transport unavailable")

// answerEnvelope is the wire shape of a delivered answer. Versioned like every
// other record so a future transport can be recognised rather than guessed at.
type answerEnvelope struct {
	SchemaVersion string `json:"schema_version"`
	ID            string `json:"id"`
	Value         string `json:"value"`
}

// listener waits for exactly one answer.
type listener interface {
	Wait(ctx context.Context) ([]byte, error)
	Close() error
}

// listen opens the answer channel for a pending request.
//
// The socket is strongly preferred and the reason is not performance: on the
// socket path the value NEVER BECOMES A FILE. It travels through a kernel buffer
// into the waiting process's memory, so there is no moment at which the plaintext
// is an object on disk, nothing to clean up if we crash, no TOCTOU between the
// write and the read, and — through peer credentials — a way to verify who is
// answering that a file's mode cannot provide.
func listen(dir string) (listener, error) {
	if !forceFileChannel() {
		l, err := listenSocket(filepath.Join(dir, answerSock))
		if err == nil {
			return l, recordChannel(dir, channelSocket)
		}
		if !errors.Is(err, errNoSocket) {
			return nil, err
		}
	}
	return &fileListener{path: filepath.Join(dir, answerFile)}, recordChannel(dir, channelFile)
}

// The transport actually in use, recorded on disk by the listener.
const (
	channelSocket = "socket"
	channelFile   = "file"
)

// recordChannel publishes which transport is open.
//
// The answering side must not INFER this. The obvious inference — "sockets are
// supported on this platform, so a missing socket means the channel closed" — is
// wrong whenever the listener itself fell back, and it falls back for reasons that
// have nothing to do with the platform: a unix socket path is limited to about 104
// bytes on macOS, so a deep XDG_RUNTIME_DIR or a long temp path silently pushes a
// perfectly healthy request onto the file channel. Inferring there would refuse
// every answer to those requests with "no longer accepting an answer", which is
// both wrong and unfalsifiable from the outside.
//
// So the listener states what it opened, and send obeys the statement.
func recordChannel(dir, kind string) error {
	path := filepath.Join(dir, channelFileName)
	if err := os.WriteFile(path, []byte(kind), 0o600); err != nil {
		return fmt.Errorf("ask: recording the answer channel: %w", err)
	}
	return nil
}

func openChannel(dir string) string {
	b, err := os.ReadFile(filepath.Join(dir, channelFileName))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// errClosed is returned when a request is no longer accepting an answer.
var errClosed = errors.New("ask: this request is no longer accepting an answer (already answered, cancelled, or expired)")

// send delivers an answer over whichever channel the request opened.
//
// The single-use rule is enforced here rather than left to the transport, and the
// fallback is deliberately NOT taken when the socket has closed. Falling through
// in that case was a real bug: after the listener finished, a second delivery
// wrote the value into the answer FILE, where nothing would ever read it and
// nothing would ever unlink it — turning a completed request into exactly the kind
// of abandoned plaintext on disk this package exists to eliminate.
func send(dir string, env answerEnvelope) error {
	if answered(dir) {
		return errClosed
	}
	payload, err := json.Marshal(env)
	if err != nil {
		return err
	}

	switch openChannel(dir) {
	case channelSocket:
		sock := filepath.Join(dir, answerSock)
		if _, statErr := os.Stat(sock); statErr != nil {
			// The listener said socket and the socket is gone: it has closed.
			return errClosed
		}
		err := sendSocket(sock, payload)
		if errors.Is(err, errNoSocket) {
			// The socket file is there but nothing is listening — the requester
			// went away between our stat and our dial.
			return errClosed
		}
		return err
	case channelFile:
		return sendFile(filepath.Join(dir, answerFile), payload)
	default:
		// No channel recorded: the requester has not finished opening one. This is
		// not reachable from a human following the printed instruction, which is
		// only emitted after the channel is up.
		return fmt.Errorf("ask: this request is not ready for an answer yet")
	}
}

func forceFileChannel() bool {
	v := strings.ToLower(strings.TrimSpace(os.Getenv(NoSocketEnv)))
	return v != "" && v != "0" && v != "false" && v != "no"
}

// --- the file channel ----------------------------------------------------

// fileListener polls for an answer file.
//
// This is the degraded path (Windows, or a filesystem that will not host a
// socket), and its cost must be stated rather than glossed: the plaintext IS
// briefly a file on disk. It is created 0600 inside a 0700 directory and unlinked
// as soon as it is read, but between those two moments it exists. Overwriting the
// bytes before unlinking would be theatre on a copy-on-write or journaling
// filesystem, so it is not attempted — an honest note beats a comforting no-op.
type fileListener struct{ path string }

func (f *fileListener) Wait(ctx context.Context) ([]byte, error) {
	const poll = 100 * time.Millisecond
	t := time.NewTicker(poll)
	defer t.Stop()
	for {
		b, err := os.ReadFile(f.path)
		if err == nil {
			// Unlink IMMEDIATELY, before returning: single use is enforced by the
			// answer no longer existing, and an early return path must not be able
			// to leave it behind.
			_ = os.Remove(f.path)
			return b, nil
		}
		if !os.IsNotExist(err) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (f *fileListener) Close() error {
	_ = os.Remove(f.path)
	return nil
}

// sendFile writes the answer atomically.
//
// Write-then-rename, so the reader never observes a partial file and mistakes a
// truncated secret for the whole one. The temporary is created with O_EXCL and
// 0600 in the same 0700 directory, so it is never visible to anyone else even
// mid-write.
func sendFile(path string, payload []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY|noFollow, 0o600)
	if err != nil {
		return fmt.Errorf("ask: delivering the answer: %w", err)
	}
	if _, err := f.Write(payload); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// --- raising and answering ------------------------------------------------

// waitForAnswer publishes the request, tells the human how to answer it, and
// blocks.
//
// The instruction goes to STDERR, and that placement is the whole design of the
// harness seam. Under an agentic CLI, stderr is captured and shown to the model —
// which is exactly what we want, because the model's job here is to RELAY the
// instruction to the human it is talking to. The request carries no secret, so
// putting it in the transcript costs nothing; the answer travels the other way,
// through a channel the agent never touches. The transcript carries the question,
// the human's own terminal carries the value.
func waitForAnswer(r Request, status io.Writer) ([]byte, error) {
	dir := requestDir(r.ID)
	l, err := listen(dir)
	if err != nil {
		return nil, err
	}
	defer l.Close()

	// The sentinel first (machine-readable, for a harness that implements the
	// seam), then the human-readable line (for one that does not). A harness that
	// recognises the prefix suppresses both and renders its own UI.
	sentinel, err := json.Marshal(sentinelLine(r))
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(status, "%s %s\n", SchemaVersion, sentinel)
	fmt.Fprintf(status, "bashy ask: waiting for a human. In YOUR OWN terminal, run:  bashy ask claim %s\n",
		shortID(r.ID))

	ctx, cancel := context.WithDeadline(context.Background(), r.Expires)
	defer cancel()

	value, err := l.Wait(ctx)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) {
			return nil, fmt.Errorf("ask: nobody answered within %s",
				r.Expires.Sub(r.Created).Round(time.Second))
		}
		return nil, err
	}

	// Latch the request closed the moment an answer arrives, before it is even
	// validated: whatever happens next, this request has had its one delivery.
	markAnswered(dir)

	var env answerEnvelope
	if err := json.Unmarshal(value, &env); err != nil {
		return nil, fmt.Errorf("ask: the answer was malformed: %w", err)
	}
	// The id check closes a confused-deputy hole: without it, an answer intended
	// for one request could satisfy another, so a low-stakes prompt could be used
	// to harvest the reply meant for a high-stakes one.
	if env.ID != r.ID {
		return nil, fmt.Errorf("ask: the answer was for a different request")
	}
	if env.Value == "" {
		// Same rule as the GUI channel: an empty answer is a decline, never a
		// successful empty secret.
		return nil, errDeclined
	}
	return []byte(env.Value), nil
}

// Answer delivers a value to a pending request. Used by `ask answer` (a harness
// piping a value in) and by `ask claim` (a human typing one).
func Answer(r Request, value []byte) error {
	if len(value) == 0 {
		return fmt.Errorf("ask: refusing to deliver an empty value")
	}
	return send(requestDir(r.ID), answerEnvelope{
		SchemaVersion: SchemaVersion,
		ID:            r.ID,
		Value:         string(value),
	})
}

var errDeclined = errors.New("ask: the operator declined")

// sentinelLine is the machine-readable request a first-party harness parses.
// It deliberately mirrors the on-disk Request minus anything a harness has no
// business acting on.
func sentinelLine(r Request) map[string]any {
	return map[string]any{
		"schema_version": SchemaVersion,
		"id":             r.ID,
		"name":           r.Name,
		"prompt":         r.Prompt,
		"secret":         r.Secret,
		"sink":           r.Sink,
		"timeout_ms":     time.Until(r.Expires).Milliseconds(),
		"requester":      r.Requester,
		"claim":          "bashy ask claim " + shortID(r.ID),
	}
}

// shortID is the prefix a human is asked to type. Long enough to be unambiguous
// among the handful of requests that can be pending at once, short enough to
// retype from a glance.
func shortID(id string) string {
	if len(id) > 8 {
		return id[:8]
	}
	return id
}
