package ctty

import (
	"bufio"
	"errors"
	"strings"
	"testing"
)

// The channel decision is the most important logic in this package and the
// hardest to reach through the world: exercising it for real needs a terminal, a
// window server, and an SSH session, three at a time. That is exactly why
// ChooseChannel takes a Probe instead of calling syscalls — so the remote-execution
// cases, the ones that would otherwise hang forever on a dialog nobody can see, get
// covered here rather than in production.
func TestChooseChannel(t *testing.T) {
	cases := []struct {
		name  string
		probe Probe
		want  Channel
	}{
		{
			name:  "a human at a shell gets their own terminal",
			probe: Probe{Requested: ChannelAuto, TTY: true, GUI: true},
			want:  ChannelTTY,
		},
		{
			name: "under an agent harness the terminal is gone, so the GUI serves",
			// The measured Claude Code case: setsid'd child, /dev/tty is ENXIO,
			// but the desktop is right there.
			probe: Probe{Requested: ChannelAuto, TTY: false, GUI: true},
			want:  ChannelGUI,
		},
		{
			name:  "no terminal and no attended GUI falls back to the rendezvous",
			probe: Probe{Requested: ChannelAuto, TTY: false, GUI: false},
			want:  ChannelRendezvous,
		},
		{
			name:  "a first-party harness handler preempts every other rung",
			probe: Probe{Requested: ChannelAuto, HandlerSet: true, TTY: true, GUI: true},
			want:  ChannelHandler,
		},
		{
			name:  "an explicit --channel is honoured even when the probe says it will fail",
			probe: Probe{Requested: ChannelTTY, TTY: false, GUI: true},
			want:  ChannelTTY,
		},
		{
			name:  "an explicit --channel outranks a harness handler",
			probe: Probe{Requested: ChannelRendezvous, HandlerSet: true, TTY: true, GUI: true},
			want:  ChannelRendezvous,
		},
		{
			name:  "an empty Requested is treated as auto",
			probe: Probe{TTY: true},
			want:  ChannelTTY,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ChooseChannel(tc.probe); got != tc.want {
				t.Errorf("ChooseChannel(%+v) = %q, want %q", tc.probe, got, tc.want)
			}
		})
	}
}

// A GUI helper must never turn "nobody was there" into a successful empty secret.
//
// This is the concrete form of the evidence rule: a success state may not be
// reached by the ABSENCE of a signal. Every row here is an output a real helper
// produces when the human did NOT supply a value, and every one must be an error.
func TestGuiAnswerRequiresPositiveEvidence(t *testing.T) {
	cases := []struct {
		name    string
		raw     string
		want    error
		wantVal string
	}{
		{
			name:    "a real answer is returned",
			raw:     "OK:ghp_secretvalue\n",
			wantVal: "ghp_secretvalue",
		},
		{
			// The measured trap: `osascript ... giving up after N` EXITS 0 and
			// prints gave-up. Our AppleScript maps that to TIMEOUT precisely so it
			// cannot be mistaken for an answer.
			name: "a dialog nobody answered is a timeout, not an empty secret",
			raw:  "TIMEOUT\n",
			want: ErrTimeout,
		},
		{
			name: "cancel is a decline",
			raw:  "CANCEL\n",
			want: ErrDeclined,
		},
		{
			name: "OK with an empty field is a decline, never an empty secret",
			raw:  "OK:\n",
			want: ErrDeclined,
		},
		{
			name: "an unparseable result is an error, not a fourth silent outcome",
			raw:  "what is this\n",
		},
		{
			name: "no output at all is an error",
			raw:  "",
		},
		{
			name:    "a value containing a colon survives intact",
			raw:     "OK:user:pass:with:colons\n",
			wantVal: "user:pass:with:colons",
		},
		{
			name:    "trailing CRLF from a Windows helper is stripped",
			raw:     "OK:token\r\n",
			wantVal: "token",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseTaggedResult("test", tc.raw)
			switch {
			case tc.wantVal != "":
				if err != nil {
					t.Fatalf("wanted value %q, got error %v", tc.wantVal, err)
				}
				if string(got) != tc.wantVal {
					t.Errorf("value = %q, want %q", got, tc.wantVal)
				}
			case tc.want != nil:
				if !errors.Is(err, tc.want) {
					t.Fatalf("error = %v, want %v", err, tc.want)
				}
			default:
				if err == nil {
					t.Fatalf("wanted an error, got value %q", got)
				}
			}
			// The invariant that matters most: on any error path, no value leaks
			// out alongside it.
			if err != nil && len(got) != 0 {
				t.Errorf("a failed prompt returned a value %q — it must return nothing", got)
			}
		})
	}
}

// pinentry expresses the same three outcomes in a different shape, so it needs its
// own coverage of the same rule.
func TestPinentryRequiresPositiveEvidence(t *testing.T) {
	cases := []struct {
		name    string
		resp    string
		want    error
		wantVal string
	}{
		{
			name:    "a D line then OK is an answer",
			resp:    "D ghp_secretvalue\nOK\n",
			wantVal: "ghp_secretvalue",
		},
		{
			name: "OK with no D line is an empty entry — a decline",
			resp: "OK\n",
			want: ErrDeclined,
		},
		{
			name: "ERR is the user cancelling",
			resp: "ERR 83886179 Operation cancelled <Pinentry>\n",
			want: ErrDeclined,
		},
		{
			name:    "percent escapes in the value are decoded",
			resp:    "D two%0Alines\nOK\n",
			wantVal: "two\nlines",
		},
		{
			name:    "status lines before the value are ignored",
			resp:    "S PASSWORD_FROM_CACHE\nD cached-secret\nOK\n",
			wantVal: "cached-secret",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parsePinentryGetpin(bufio.NewReader(strings.NewReader(tc.resp)))
			if tc.wantVal != "" {
				if err != nil {
					t.Fatalf("wanted %q, got error %v", tc.wantVal, err)
				}
				if string(got) != tc.wantVal {
					t.Errorf("value = %q, want %q", got, tc.wantVal)
				}
				return
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want %v", err, tc.want)
			}
			if len(got) != 0 {
				t.Errorf("a failed prompt returned a value %q", got)
			}
		})
	}
}

// A truncated stream must not be read as an empty answer either.
func TestPinentryTruncatedStreamIsAnError(t *testing.T) {
	got, err := parsePinentryGetpin(bufio.NewReader(strings.NewReader("D partial")))
	if err == nil {
		t.Fatalf("wanted an error on a truncated response, got %q", got)
	}
	if len(got) != 0 {
		t.Errorf("returned a value %q from a truncated stream", got)
	}
}

// The Assuan escaping is what keeps a multi-line frame from being read as protocol.
func TestAssuanEscapeRoundTrip(t *testing.T) {
	for _, s := range []string{"plain", "two\nlines", "100% sure", "cr\rlf\n", ""} {
		if got := assuanUnescape(assuanEscape(s)); got != s {
			t.Errorf("round trip of %q gave %q", s, got)
		}
	}
	if got := assuanEscape("a\nb"); strings.Contains(got, "\n") {
		t.Errorf("escaped form %q still contains a newline — it would break the protocol", got)
	}
}

// The answer must be handed back byte-for-byte apart from the line terminator.
// Trailing spaces and tabs are legitimate in some tokens, and silently trimming
// them produces an authentication failure the operator cannot explain.
func TestTrimAnswerKeepsMeaningfulWhitespace(t *testing.T) {
	cases := map[string]string{
		"secret\n":      "secret",
		"secret\r\n":    "secret",
		"secret ":       "secret ",
		"secret\t\n":    "secret\t",
		"  leading\n":   "  leading",
		"":              "",
		"no-newline":    "no-newline",
		"multi\nline\n": "multi\nline",
	}
	for in, want := range cases {
		if got := string(trimAnswer([]byte(in))); got != want {
			t.Errorf("trimAnswer(%q) = %q, want %q", in, got, want)
		}
	}
}

// Request.text composes the trustworthy frame and the untrusted prompt in a fixed
// order, so a caller cannot accidentally show the prompt without its provenance.
func TestRequestTextPutsFrameBeforePrompt(t *testing.T) {
	r := Request{Frame: "FRAME LINE", Prompt: "type it"}
	got := r.text()
	if !strings.HasPrefix(got, "FRAME LINE") {
		t.Errorf("frame must come first, got %q", got)
	}
	if !strings.HasSuffix(got, "type it") {
		t.Errorf("prompt must come last, got %q", got)
	}
	if bare := (Request{Prompt: "only"}).text(); bare != "only" {
		t.Errorf("an empty frame should not add padding, got %q", bare)
	}
}
