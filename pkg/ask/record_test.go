package ask

import (
	"bytes"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRecordDeliversTheUploadedTake drives `record video --script` end to end
// with the browser replaced by an HTTP client: the page carries the script as
// the teleprompter, one upload is accepted, a second is refused, and the take
// lands at --out with mode 0600 and the path on stdout.
func TestRecordDeliversTheUploadedTake(t *testing.T) {
	isolate(t)
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	dir := t.TempDir()
	script := filepath.Join(dir, "pitch.txt")
	if err := os.WriteFile(script, []byte("Hi, I'm the founder.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "take.mp4")
	take := []byte("\x00\x00\x00\x18ftypmp42 fake take")

	pageOK := make(chan error, 1)
	old := openBrowser
	openBrowser = func(url string) error {
		go func() {
			res, err := http.Get(url)
			if err != nil {
				pageOK <- err
				return
			}
			page, _ := io.ReadAll(res.Body)
			res.Body.Close()
			if !strings.Contains(string(page), `"Hi, I'm the founder."`) || !strings.Contains(string(page), `KIND = "video"`) {
				pageOK <- errString("page lacks the script or kind")
				return
			}
			r1, err := http.Post(url+"upload", "video/mp4", bytes.NewReader(take))
			if err != nil || r1.StatusCode != 200 {
				pageOK <- errString("first upload refused")
				return
			}
			r1.Body.Close()
			if r2, err := http.Post(url+"upload", "video/mp4", bytes.NewReader(take)); err == nil {
				r2.Body.Close() // server may already be gone; either refusal is fine
			}
			pageOK <- nil
		}()
		return nil
	}
	t.Cleanup(func() { openBrowser = old })

	cmd := NewRecordCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs([]string{"video", "--script", script, "--out", out, "--timeout", "20s"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("record: %v (stderr %q)", err, stderr.String())
	}
	if err := <-pageOK; err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(stdout.String()); got != out {
		t.Fatalf("stdout = %q, want the --out path %q", got, out)
	}
	b, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(b, take) {
		t.Fatalf("take not delivered intact: %v", err)
	}
	if fi, _ := os.Stat(out); fi.Mode().Perm() != 0o600 {
		t.Fatalf("take mode %v, want 0600", fi.Mode().Perm())
	}
}

func TestRecordRefusesUnknownKindAndRemoteSessions(t *testing.T) {
	isolate(t)
	cmd := NewRecordCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"hologram", "--timeout", "1s"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "photo, video or audio") {
		t.Fatalf("unknown kind: err = %v", err)
	}
	if runtimeIsLinuxWithDisplay() {
		t.Skip("a Linux session with a display may legitimately open a page")
	}
	t.Setenv("SSH_CONNECTION", "10.0.0.1 1 10.0.0.2 22")
	cmd = NewRecordCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"photo", "--timeout", "1s"})
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "remote") {
		t.Fatalf("remote session: err = %v", err)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func runtimeIsLinuxWithDisplay() bool {
	return os.Getenv("DISPLAY") != "" || os.Getenv("WAYLAND_DISPLAY") != ""
}
