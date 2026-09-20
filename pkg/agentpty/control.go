// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package agentpty

// The pty-agnostic half of Run: the control-line protocol (what `chat steer`
// / `weave say` write at the agent), the output taps, and the terminal size.
// Shared by the unix (creack/pty) and Windows (ConPTY) backends, so the way a
// steer is typed at a TUI cannot drift between them.

import (
	"bufio"
	"encoding/base64"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"golang.org/x/term"
)

// A managed opening turn includes the agent's pending unified inbox. That can
// legitimately exceed bufio.Scanner's 64 KiB default even though each authored
// message is individually bounded. Keep the line protocol bounded, but size it
// for a real accumulated inbox rather than silently closing the socket halfway
// through the frame (which surfaces to the sender as EPIPE).
const maxPTYControlFrameBytes = 4 << 20

func newPTYControlScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), maxPTYControlFrameBytes)
	return sc
}

// steerEnterDelay separates the typed text from the Enter that submits it.
//
// It is not a magic sleep — it is the difference between typing and pasting.
var steerEnterDelay = 150 * time.Millisecond

func writePTYControlLine(ptmx io.Writer, line string) {
	if line == "" {
		return
	}
	// Verbatim frame: \x00R<base64> — decoded bytes written
	// to PTY exactly as-is (no trailing \r).
	if strings.HasPrefix(line, "\x00R") {
		if decoded, err := base64.StdEncoding.DecodeString(line[2:]); err == nil {
			_, _ = ptmx.Write(decoded)
		}
		return
	}

	// Text first, THEN Enter, as two writes with a pause between them.
	//
	// Sending `text + "\r"` in one write looks like a PASTE, not typing — and a
	// TUI in bracketed-paste mode (codex turns it on, along with the kitty
	// keyboard protocol) puts the pasted text in its input box and does NOT
	// submit it. The steer lands on screen and nothing happens; worse, the echoed
	// text is indistinguishable from an answer to anything reading the output, so
	// the failure looks like a success.
	//
	// That is exactly how `supports_say: false` came to be believed about codex.
	// Two writes, and it submits.
	//
	// And the TEXT itself goes in CHUNKS, because a terminal's input buffer is not
	// infinite. A pty's canonical input queue is ~4096 bytes (MAX_CANON); write a
	// whole conductor brief at it in one go and the tail is simply dropped — no
	// error, no short write, nothing. The agent gets a truncated prompt, or none at
	// all, and sits at an empty input box looking exactly like a model with nothing
	// to say.
	//
	// Measured: a 4 KB opening prompt vanished entirely into an opencode session
	// while a 40-byte probe on the SAME socket went straight through. Every real
	// conductor prompt is 4 KB, so opening a session on any tool that does not take
	// its prompt on argv (opencode, codex) was structurally broken — and it failed
	// as "deepseek did nothing", which is a lie about the model.
	writePTYChunked(ptmx, line)
	time.Sleep(steerEnterDelay)
	_, _ = io.WriteString(ptmx, "\r")
}

// ptyChunkBytes is comfortably under MAX_CANON (4096) so a chunk can never sit at
// the boundary, and small enough that a TUI's own input handling keeps up.
const ptyChunkBytes = 512

// ptyChunkDelay lets the reader drain between chunks. Without it we simply refill
// the buffer as fast as we overflowed it.
const ptyChunkDelay = 25 * time.Millisecond

// writePTYChunked feeds text to the terminal in pieces, the way typing does.
func writePTYChunked(ptmx io.Writer, s string) {
	b := []byte(s)
	for len(b) > 0 {
		n := min(ptyChunkBytes, len(b))
		// Do not split a multi-byte rune across a chunk: a terminal handed half a
		// UTF-8 sequence renders garbage and may drop the rest of the line.
		for n < len(b) && n > 0 && b[n]&0xC0 == 0x80 {
			n--
		}
		if n == 0 {
			n = min(ptyChunkBytes, len(b))
		}
		if _, err := ptmx.Write(b[:n]); err != nil {
			return
		}
		b = b[n:]
		if len(b) > 0 {
			time.Sleep(ptyChunkDelay)
		}
	}
}

func tailPTYControlFile(path string, ptmx io.Writer) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	r := bufio.NewReader(f)
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			line = strings.TrimSuffix(line, "\n")
			line = strings.TrimSuffix(line, "\r")
			writePTYControlLine(ptmx, line)
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			time.Sleep(50 * time.Millisecond)
			continue
		}
		return
	}
}

// activityTap wraps an io.Writer and calls bump(n) on each write,
// so the watchdog goroutine can detect a stalled subagent. The
// goroutine uses sync/atomic on the timestamp; the writer itself
// stays lock-free.
type activityTap struct {
	w    io.Writer
	bump func(int)
}

// trustClearTap watches the live output and clears a trust prompt the moment it
// appears — the difference between an agent that attends and one that sits at a
// question nobody is there to answer.
//
// Only GateTrust is routed here. The other gates need a browser or a human, and
// those routes belong to the caller, which knows where an escalation should go.
type trustClearTap struct {
	w    io.Writer
	deps RouteDeps
	tail string
}

func newTrustClearTap(w io.Writer, ctlSock string) io.Writer {
	return &trustClearTap{
		w: w,
		deps: RouteDeps{
			State: &GateRouteState{},
			Say: func(payload string) error {
				return BrokerSay(ctlSock, payload)
			},
		},
	}
}

func (t *trustClearTap) Write(p []byte) (int, error) {
	n, err := t.w.Write(p)
	if len(p) > 0 {
		t.tail += string(p)
		if len(t.tail) > 8192 {
			t.tail = t.tail[len(t.tail)-8192:]
		}
		if verdict := ClassifyGate(t.tail); verdict.Kind == GateTrust {
			_, _ = RouteGate(verdict, t.deps)
		}
	}
	return n, err
}

func (a *activityTap) Write(p []byte) (int, error) {
	n, err := a.w.Write(p)
	a.bump(n)
	return n, err
}

// ptySize returns the controlling terminal's size, or 24x80 as
// a fallback so backgrounded subagents still get a sensible default.
func ptySize() (uint16, uint16) {
	if w, h, err := term.GetSize(int(os.Stdout.Fd())); err == nil {
		return uint16(h), uint16(w)
	}
	return 24, 80
}
