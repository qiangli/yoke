//go:build !windows

package agentpty

import (
	"bytes"
	"encoding/base64"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestControlScannerAcceptsAccumulatedManagedInboxFrame(t *testing.T) {
	payload := bytes.Repeat([]byte("x"), 128*1024)
	sc := newPTYControlScanner(bytes.NewReader(append(payload, '\n')))
	if !sc.Scan() {
		t.Fatalf("128 KiB managed opening frame was rejected: %v", sc.Err())
	}
	if got := len(sc.Bytes()); got != len(payload) {
		t.Fatalf("managed opening frame length = %d, want %d", got, len(payload))
	}
}

// The wire had three implementations — this one, agentlaunch's copy, and weave's
// hand-rolled base64 — and they had already begun to disagree. These tests pin the
// encoding so a fourth cannot quietly appear.

func TestTextFrameFlattensNewlines(t *testing.T) {
	// A newline mid-payload would SUBMIT half the sentence and leave the rest in
	// the agent's input box. weave's attach loop sent operator input verbatim and
	// would have done exactly that on a paste.
	got := TextFrame("stay on the gate question\nyou are re-litigating the schema")
	if strings.Contains(strings.TrimSuffix(got, "\r\n"), "\n") {
		t.Errorf("TextFrame left an embedded newline: %q", got)
	}
	if !strings.HasSuffix(got, "\r\n") {
		t.Errorf("TextFrame must end in the Enter terminator: %q", got)
	}
}

func TestTextFrameEscapesNUL(t *testing.T) {
	// A NUL cannot survive the line protocol — it is the verbatim frame's own
	// marker byte. It must escape, not corrupt the stream.
	got := TextFrame("a\x00b")
	if !strings.HasPrefix(got, "\x00R") {
		t.Fatalf("a NUL payload must fall back to the verbatim frame, got %q", got)
	}
	payload := strings.TrimSuffix(strings.TrimPrefix(got, "\x00R"), "\n")
	dec, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatalf("verbatim frame is not valid base64: %v", err)
	}
	if string(dec) != "a\x00b" {
		t.Errorf("round-trip lost the payload: %q", dec)
	}
}

func TestVerbatimFrameRoundTrips(t *testing.T) {
	// This is the frame `weave say --enter/--tab/--raw` needs: a KEYSTROKE, not a
	// sentence. Bare \r, bare \t, escape sequences — none survive a line protocol.
	for _, payload := range [][]byte{{'\r'}, {'\t'}, []byte("/quit\n"), {0x1b, '[', 'A'}} {
		frame := VerbatimFrame(payload)
		enc := strings.TrimSuffix(strings.TrimPrefix(frame, "\x00R"), "\n")
		dec, err := base64.StdEncoding.DecodeString(enc)
		if err != nil {
			t.Fatalf("%q: %v", payload, err)
		}
		if string(dec) != string(payload) {
			t.Errorf("VerbatimFrame(%q) round-tripped to %q", payload, dec)
		}
	}
}

// THE FAILURE THIS EXISTS TO PREVENT: a steer that reports success and arrives
// nowhere. `meet say` did this for months — it wrote to a socket that its headless
// one-shot turns never listened on, printed "→ Sable (round 2): …", and delivered
// nothing.
//
// An empty socket path is not a place. Refuse it.
func TestSendFrameRefusesAnEmptySocket(t *testing.T) {
	if err := SendFrame("", "hello\r\n"); err == nil {
		t.Fatal("SendFrame accepted an empty control socket — a steer with nowhere to go " +
			"must fail, not silently succeed")
	}
	if err := BrokerSay("", "hello"); err == nil {
		t.Fatal("BrokerSay accepted an empty control socket")
	}
}

func TestSendFrameReachesAListener(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Skipf("unix socket unavailable: %v", err)
	}
	defer ln.Close()

	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		buf := make([]byte, 256)
		n, _ := c.Read(buf)
		got <- string(buf[:n])
	}()

	if err := BrokerSay(sock, "come back to the agenda"); err != nil {
		t.Fatalf("BrokerSay: %v", err)
	}
	select {
	case frame := <-got:
		if frame != "come back to the agenda\r\n" {
			t.Errorf("wire carried %q", frame)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("nothing arrived on the control socket")
	}
}

// A unix socket address caps at ~104 bytes. A caller with a long path used to lose
// steering ENTIRELY rather than merely polling for it — so the file fallback is
// load-bearing, not a nicety.
func TestSendFrameFallsBackToAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ctl.file")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := BrokerSay(path, "hello"); err != nil {
		t.Fatalf("file fallback: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "hello\r\n" {
		t.Errorf("file channel holds %q", b)
	}
}
