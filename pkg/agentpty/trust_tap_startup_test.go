package agentpty

import (
	"bytes"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
)

// Sprint #328 lost its codex conductor twice to this tap. It classified the
// whole 8 KB of scrollback on every write, for the whole life of the session, so
// any output QUOTING a trust dialog — a diff of gate.go, a story, a log — made
// it type "1" at a working agent. In codex that "1" can land in the permissions
// menu, where 1 is Read Only: every later write to ~/.bashy and .git failed.

func newTestTrustTap(input *atomic.Bool) (*trustClearTap, *[]string) {
	said := &[]string{}
	return &trustClearTap{
		w:     &bytes.Buffer{},
		input: input,
		deps: RouteDeps{State: &GateRouteState{}, Say: func(p string) error {
			*said = append(*said, p)
			return nil
		}},
	}, said
}

// Trust-dialog text that has scrolled past is not a dialog: the screen has to
// END with it.
func TestTrustClearTapIgnoresTrustTextScrolledPast(t *testing.T) {
	var lines, rows strings.Builder
	lines.WriteString("+\t\t\"do you trust\", \"trust the contents\", \"trust this directory\",\n" +
		"+\t\t\"trust this folder\", \"continue? 1\", \"1. yes\", \"1) yes\",\n")
	rows.WriteString(codexTrustDialog)
	for i := range 20 {
		fmt.Fprintf(&lines, "ok  \tgithub.com/qiangli/yoke/pkg/p%d\t0.4s\n", i)
		// The same, as a full-screen TUI draws it: a cursor move per row.
		fmt.Fprintf(&rows, "\x1b[%d;1Hok\x1b[%d;5Hgithub.com/qiangli/yoke/pkg/p%d", 11+i, 11+i, i)
	}
	for name, screen := range map[string]string{"newlines": lines.String(), "cursor rows": rows.String()} {
		tap, said := newTestTrustTap(nil)
		if _, err := tap.Write([]byte(screen)); err != nil {
			t.Fatal(err)
		}
		if len(*said) != 0 {
			t.Errorf("%s: typed %q at a session whose screen merely quoted a trust dialog", name, *said)
		}
	}
}

// The tap answers a STARTUP dialog. Once the session has been typed at it is
// working, and nothing it draws is a folder-trust question any more.
func TestTrustClearTapDisarmsAfterFirstInput(t *testing.T) {
	var typed atomic.Bool
	tap, said := newTestTrustTap(&typed)
	ctl := &inputTap{w: &bytes.Buffer{}, typed: &typed}
	if _, err := ctl.Write([]byte("fix story #1275")); err != nil {
		t.Fatal(err)
	}
	if _, err := tap.Write([]byte(codexTrustDialog)); err != nil {
		t.Fatal(err)
	}
	if len(*said) != 0 {
		t.Fatalf("typed %q at a session that had already been given input", *said)
	}
}

// The startup dialog is still answered — once. A second trust-shaped screen
// later in the session is somebody else's question.
func TestTrustClearTapAnswersStartupDialogOnce(t *testing.T) {
	var typed atomic.Bool
	tap, said := newTestTrustTap(&typed)
	for _, screen := range []string{
		"codex v0.156.1\n\n" + codexTrustDialog,
		"Do you trust the contents of this folder?\n 1. Yes  2. No\n",
	} {
		if _, err := tap.Write([]byte(screen)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*said) != 1 || (*said)[0] != GateTrustClearPayload {
		t.Fatalf("said = %q, want exactly one %q", *said, GateTrustClearPayload)
	}
}
