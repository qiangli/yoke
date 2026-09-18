package ask

import (
	"strings"
	"testing"
)

// TestFramePutsPurposeAndDestinationFirst pins the ordering fix. The frame
// previously led with pid/principal/cwd/argv and buried the caller's message at
// the bottom, which in a GUI dialog rendered as a wall of text whose only
// interesting line was last — operators could not tell which host or account a
// prompt was for. A frame nobody reads protects nobody.
func TestFramePutsPurposeAndDestinationFirst(t *testing.T) {
	r := Request{
		ID: "abc123", Name: "SVC_PWD",
		Prompt:    sanitizePrompt("password for 'svc-account' on host-a"),
		Sink:      Sink{Kind: SinkFile, Detail: "/tmp/x/value"},
		Requester: Requester{PID: 1, PPID: 2, Principal: "you", Cwd: "/w", Tool: "claude", Argv: []string{"bashy", "ask"}},
	}
	frame := renderFrame(r)
	idx := func(s string) int { return strings.Index(frame, s) }

	purpose := idx("WHAT FOR")
	dest := idx("THE VALUE GOES")
	provenance := idx("who is asking")

	if purpose < 0 || dest < 0 || provenance < 0 {
		t.Fatalf("frame is missing a required section:\n%s", frame)
	}
	if !(purpose < dest && dest < provenance) {
		t.Errorf("want purpose < destination < provenance, got %d/%d/%d:\n%s",
			purpose, dest, provenance, frame)
	}
	// The caller's own text must still sit UNDER the untrusted banner, never above
	// it — that is what stops caller text masquerading as chrome.
	if idx("svc-account") < purpose {
		t.Errorf("caller text appears above the untrusted banner:\n%s", frame)
	}
}

// TestFrameTruncatesTheCommandLine covers the other half of the noise problem: an
// agent-invoked argv is routinely hundreds of characters, and left whole it
// dominated the dialog.
func TestFrameTruncatesTheCommandLine(t *testing.T) {
	long := make([]string, 0, 60)
	long = append(long, "bashy", "ask")
	for i := 0; i < 58; i++ {
		long = append(long, "--flag=averyverylongargumentvalue")
	}
	r := Request{
		ID: "x", Prompt: "p", Sink: Sink{Kind: SinkStdout},
		Requester: Requester{PID: 1, Argv: long},
	}
	frame := renderFrame(r)
	for _, ln := range strings.Split(frame, "\n") {
		if strings.Contains(ln, "command line") {
			if len(ln) > 200 {
				t.Errorf("command-line row is %d chars, want it truncated: %q", len(ln), ln)
			}
			if !strings.Contains(ln, "...") {
				t.Errorf("truncated row must say it was cut: %q", ln)
			}
			// The head identifies the caller and must survive.
			if !strings.Contains(ln, "bashy ask") {
				t.Errorf("truncation dropped the identifying head: %q", ln)
			}
			return
		}
	}
	t.Fatalf("no command-line row in frame:\n%s", frame)
}

// TestEllipsizeIsRuneSafe guards against slicing a multi-byte character in half,
// which would render as a replacement glyph inside trusted chrome.
func TestEllipsizeIsRuneSafe(t *testing.T) {
	s := strings.Repeat("é", 40)
	got := ellipsize(s, 10)
	if n := len([]rune(got)); n != 10 {
		t.Errorf("ellipsize returned %d runes, want 10: %q", n, got)
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Errorf("ellipsize produced a replacement glyph: %q", got)
	}
	if ellipsize("short", 100) != "short" {
		t.Error("ellipsize altered a string already within the limit")
	}
	if ellipsize("anything", 0) != "" {
		t.Error("ellipsize(_, 0) must be empty")
	}
}
