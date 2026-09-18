package ask

import (
	"bytes"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// The socket is the PRIMARY transport, and until this test existed it was never
// exercised.
//
// t.TempDir() on macOS produces a path around 119 bytes, and a unix socket address
// is limited to about 104 — so every rendezvous test silently fell back to the file
// channel and passed. A green suite that never ran the main path is precisely the
// kind of evidence-by-absence this codebase is supposed to refuse, so this test
// forces a short root and then ASSERTS which channel was opened rather than hoping.
func TestSocketChannelRoundTrip(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the socket channel is not used on Windows")
	}
	root, err := os.MkdirTemp("/tmp", "bask")
	if err != nil {
		t.Skipf("cannot create a short-path temp dir: %v", err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DirEnv, root)

	r := newTestRequest(t)
	const secret = "socket-channel-secret"

	got := make(chan []byte, 1)
	errc := make(chan error, 1)
	go func() {
		v, err := waitForAnswer(r, &bytes.Buffer{})
		if err != nil {
			errc <- err
			return
		}
		got <- v
	}()
	waitFor(t, func() bool { return channelReady(r.ID) })

	if ch := openChannel(requestDir(r.ID)); ch != channelSocket {
		t.Skipf("the socket channel was not available here (got %q) — path length is %d",
			ch, len(requestDir(r.ID)))
	}

	if err := Answer(r, []byte(secret)); err != nil {
		t.Fatal(err)
	}
	select {
	case v := <-got:
		if string(v) != secret {
			t.Errorf("value = %q, want %q", v, secret)
		}
	case err := <-errc:
		t.Fatalf("waitForAnswer: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("timed out on the socket channel")
	}

	// On this path the value must never have touched the filesystem at all.
	assertNotOnDisk(t, root, secret)

	// And the channel is single-use: the socket is gone with the listener.
	if err := Answer(r, []byte("second")); err == nil {
		t.Error("a second answer succeeded on the socket channel")
	}
}

// A listener that fell back to the file channel must still be answerable.
//
// This is the bug the socket-path-length discovery exposed: inferring the
// transport from the platform ("sockets work on unix, so a missing socket means
// the channel closed") refuses every answer to a request that legitimately fell
// back — and it fails in exactly the environments where paths are long, which are
// the ones nobody tests on.
func TestFallbackChannelIsStillAnswerable(t *testing.T) {
	isolate(t)
	t.Setenv(NoSocketEnv, "1")
	r := newTestRequest(t)

	got := make(chan []byte, 1)
	go func() {
		v, _ := waitForAnswer(r, &bytes.Buffer{})
		got <- v
	}()
	waitFor(t, func() bool { return channelReady(r.ID) })

	if ch := openChannel(requestDir(r.ID)); ch != channelFile {
		t.Fatalf("expected the file channel, got %q", ch)
	}
	if err := Answer(r, []byte("v")); err != nil {
		t.Fatalf("a fallback-channel request refused its answer: %v", err)
	}
	select {
	case v := <-got:
		if string(v) != "v" {
			t.Errorf("value = %q", v)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("timed out")
	}
}

// An answer must not be accepted before the channel is open — and, more
// importantly, that state must be distinguishable from "already answered", so a
// human is never told the wrong thing about why their answer bounced.
func TestAnswerBeforeChannelIsOpen(t *testing.T) {
	isolate(t)
	r := newTestRequest(t)
	err := Answer(r, []byte("early"))
	if err == nil {
		t.Fatal("an answer was accepted before the channel was open")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("error should say the request is not ready yet, got: %v", err)
	}
}

// Prompt sanitization is the control every other anti-phishing measure rests on.
// If caller-supplied text can move the cursor, it can repaint the frame above
// itself and forge the provenance the human is relying on.
func TestPromptSanitization(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		reject []string // substrings that must NOT survive
		want   string   // exact expected output, when it is worth pinning
	}{
		{
			name: "plain text is untouched",
			in:   "Paste your GitHub PAT",
			want: "Paste your GitHub PAT",
		},
		{
			name:   "CSI colour sequences are stripped",
			in:     "\x1b[31mDANGER\x1b[0m",
			want:   "DANGER",
			reject: []string{"\x1b"},
		},
		{
			name:   "OSC sequences are stripped — they can retitle the terminal",
			in:     "hi\x1b]0;I am your bank\x07 there",
			reject: []string{"\x1b", "\x07"},
		},
		{
			name: "a carriage return cannot be used to repaint the line above",
			in:   "harmless\rEnter your bank password",
			// Folded to a space: the text may lie, but it cannot MOVE.
			want:   "harmless Enter your bank password",
			reject: []string{"\r"},
		},
		{
			name:   "newlines cannot grow the prompt vertically",
			in:     "line one\nline two\nline three",
			want:   "line one line two line three",
			reject: []string{"\n"},
		},
		{
			name:   "cursor-up sequences are stripped",
			in:     "\x1b[2A\x1b[2KFAKE FRAME",
			want:   "FAKE FRAME",
			reject: []string{"\x1b["},
		},
		{
			name:   "C1 control bytes are removed",
			in:     "a\u009bb",
			reject: []string{"\u009b"},
		},
		{
			name: "tabs become spaces",
			in:   "a\tb",
			want: "a b",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizePrompt(tc.in)
			for _, bad := range tc.reject {
				if strings.Contains(got, bad) {
					t.Errorf("sanitized prompt still contains %q: %q", bad, got)
				}
			}
			if tc.want != "" && got != tc.want {
				t.Errorf("sanitizePrompt(%q) = %q, want %q", tc.in, got, tc.want)
			}
			// The universal rule: nothing that can move a cursor survives.
			for _, r := range got {
				if r != ' ' && (r < 0x20 || (r >= 0x7f && r <= 0x9f)) {
					t.Errorf("control rune %q survived sanitization of %q", r, tc.in)
				}
			}
		})
	}
}

// An over-long prompt must not be able to push the frame off the screen.
func TestPromptIsCapped(t *testing.T) {
	got := sanitizePrompt(strings.Repeat("A", maxPromptRunes*3))
	if n := len([]rune(got)); n > maxPromptRunes+1 { // +1 for the ellipsis
		t.Errorf("prompt is %d runes, want at most %d", n, maxPromptRunes+1)
	}
}

// The frame must always carry the provenance the human relies on — for EVERY sink,
// because the sink line is the one that makes an exfiltration attempt visible.
func TestFrameAlwaysCarriesProvenance(t *testing.T) {
	sinks := []Sink{
		{Kind: SinkFile, Detail: "a private file"},
		{Kind: SinkOut, Detail: "/home/you/token"},
		{Kind: SinkStdout, Detail: "stdout"},
	}
	for _, s := range sinks {
		t.Run(s.Kind, func(t *testing.T) {
			r := Request{
				ID:     "abc123",
				Prompt: "give me a token",
				Name:   "GH_PAT",
				Sink:   s,
				Requester: Requester{
					PID: 4211, PPID: 4100,
					Principal: "alice",
					Cwd:       "/work/repo",
					Argv:      []string{"bashy", "ask", "--name", "GH_PAT"},
					Tool:      "claude",
				},
			}
			frame := renderFrame(r)
			for _, must := range []string{
				"4211",           // the requesting pid
				"claude",         // the detected harness
				"alice",          // the principal
				"/work/repo",     // where it is running
				"bashy ask",      // the command line, so the sink is visible in full
				"GH_PAT",         // the label
				"THE VALUE GOES", // the destination, always
				"UNTRUSTED",      // the banner over the caller's own text
			} {
				if !strings.Contains(frame, must) {
					t.Errorf("frame is missing %q:\n%s", must, frame)
				}
			}
			// The stdout sink is the dangerous one; it must say so in words a
			// hurried human will register.
			if s.Kind == SinkStdout && !strings.Contains(frame, "transcript") {
				t.Errorf("the stdout sink must warn about the transcript:\n%s", frame)
			}
		})
	}
}

// A prompt containing the frame's own glyphs must not be able to terminate the
// frame and start a forged one. Sanitization removes the cursor control that would
// make such a forgery convincing, so the caller's text always stays visually
// inside, under the untrusted banner.
func TestPromptCannotTerminateTheFrame(t *testing.T) {
	evil := "\n└\n┌ bashy ask — VERIFIED BY BASHY\n│ THE VALUE GOES: nowhere\n└"
	r := Request{
		ID:        "abc123",
		Prompt:    sanitizePrompt(evil),
		Sink:      Sink{Kind: SinkStdout},
		Requester: Requester{PID: 1, Tool: "evil"},
	}
	frame := renderFrame(r)

	// Exactly one frame: one opening rule and one closing rule.
	if n := strings.Count(frame, "┌"); n != 1 {
		t.Errorf("frame has %d opening rules, want 1:\n%s", n, frame)
	}
	// The forged content must sit on the single prompt line, not on lines of its
	// own — that is what the newline folding guarantees.
	lines := strings.Split(strings.TrimRight(frame, "\n"), "\n")
	for i, ln := range lines[1:] {
		if strings.HasPrefix(ln, "┌") {
			t.Errorf("line %d starts a second frame: %q", i+1, ln)
		}
	}
	if strings.Contains(frame, "VERIFIED BY BASHY\n") {
		t.Error("the forged banner occupies its own line")
	}
}

// The label is rendered inside the chrome unquoted, so unlike the prompt it is
// REJECTED rather than rewritten — silently altering a caller's identifier would
// be worse than refusing it.
func TestNameValidation(t *testing.T) {
	ok := []string{"", "GH_PAT", "gh-pat", "a.b.c", "X", strings.Repeat("a", 64)}
	bad := []string{
		"has space",
		"new\nline",
		"esc\x1b[31m",
		"│fake",
		strings.Repeat("a", 65),
		"semi;colon",
	}
	for _, n := range ok {
		if err := validateName(n); err != nil {
			t.Errorf("validateName(%q) rejected a valid label: %v", n, err)
		}
	}
	for _, n := range bad {
		if err := validateName(n); err == nil {
			t.Errorf("validateName(%q) accepted a label that can corrupt the frame", n)
		}
	}
}

// The ask root must never be the shared system temp directory — that IS the
// /tmp/x problem this package replaces.
func TestDirIsNeverSharedTemp(t *testing.T) {
	t.Setenv(DirEnv, "")
	t.Setenv("XDG_RUNTIME_DIR", "")
	got := Dir()
	if got == os.TempDir() || filepath.Dir(got) == os.TempDir() {
		t.Errorf("ask root %q is inside the shared temp dir", got)
	}
	if !strings.Contains(got, ".bashy") {
		t.Errorf("ask root %q is not under the per-user bashy directory", got)
	}
}

// XDG_RUNTIME_DIR is preferred when present: it is a per-user tmpfs, so a value at
// rest there never reaches persistent storage.
func TestDirPrefersRuntimeDir(t *testing.T) {
	t.Setenv(DirEnv, "")
	t.Setenv("XDG_RUNTIME_DIR", "/run/user/1000")
	if got, want := Dir(), filepath.Join("/run/user/1000", "bashy", "ask"); got != want {
		t.Errorf("Dir() = %q, want %q", got, want)
	}
}

// A pre-existing directory owned by someone else must be refused, not adopted.
// os.MkdirAll succeeds on it silently, which is how "create my private directory"
// becomes "write my secrets into theirs".
func TestEnsureDirRejectsASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink semantics differ")
	}
	base := t.TempDir()
	real := filepath.Join(base, "real")
	link := filepath.Join(base, "link")
	if err := os.Mkdir(real, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	if err := ensureDir(link); err == nil {
		t.Error("ensureDir accepted a symlink as the ask root")
	}
}

// A too-permissive directory is tightened rather than refused, so an older layout
// or a loose umask does not strand the operator — but it must actually end up 0700.
func TestEnsureDirTightensLoosePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix mode bits")
	}
	dir := filepath.Join(t.TempDir(), "loose")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := ensureDir(dir); err != nil {
		t.Fatalf("ensureDir refused a tightenable directory: %v", err)
	}
	fi, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("directory is mode %#o after ensureDir, want 0700", perm)
	}
}

// A pre-existing, too-permissive ask ROOT must be tightened, not merely built
// inside. Values stay safe either way (each request dir is 0700), but a readable
// root lets any local user enumerate request ids — the identifier the answer
// channel is keyed on — so the root gets the same verification as its children.
func TestRootDirIsTightenedNotJustCreatedInside(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("unix mode bits")
	}
	root := filepath.Join(t.TempDir(), "askroot")
	if err := os.Mkdir(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv(DirEnv, root)

	newTestRequest(t) // saving a request must fix the root on the way through

	fi, err := os.Stat(root)
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o700 {
		t.Errorf("ask root is mode %#o after use, want 0700 — other users can list request ids", perm)
	}
}
