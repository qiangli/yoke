package weave

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/agentpty"
	"github.com/qiangli/yoke/pkg/secrets"
)

func TestWeaveCaptureRedactionInactiveIsLoudAndPassesThrough(t *testing.T) {
	var diagnostics bytes.Buffer
	capture := newWeaveCaptureRedactionForNames(
		[]string{"PRESENT_SECRET=synthetic-present-value"},
		map[string]struct{}{"MISSING_SECRET": {}},
		&diagnostics,
	)

	var dst bytes.Buffer
	w := capture.Writer(&dst)
	const ordinary = "capture remains available"
	if _, err := w.Write([]byte(ordinary)); err != nil {
		t.Fatalf("pass-through Write failed: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Fatalf("pass-through Close failed: %v", err)
	}
	if dst.String() != ordinary {
		t.Fatal("inactive redaction did not preserve capture output")
	}
	const warning = "SECRET REDACTION INACTIVE"
	if strings.Count(diagnostics.String(), warning) != 1 {
		t.Fatalf("inactive diagnostic count = %d, want 1", strings.Count(diagnostics.String(), warning))
	}
}

func TestWeaveCaptureRedactsLogAndReturnedText(t *testing.T) {
	if !agentpty.Supported() {
		t.Skip("PTY capture is unavailable on this platform")
	}
	if _, err := exec.LookPath("bashy"); err != nil {
		t.Skip("bashy is not on PATH")
	}

	const (
		name    = "WEAVE_CAPTURE_SECRET"
		fixture = "synthetic-weave-capture-secret-0001"
	)

	configHome := t.TempDir()
	cacheHome := t.TempDir()
	configDir := filepath.Join(configHome, "bashy")
	cacheDir := filepath.Join(cacheHome, "bashy")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatalf("create test config dir: %v", err)
	}
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		t.Fatalf("create test cache dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "secrets.map"), []byte(name+"=@capture-fixture\n"), 0o600); err != nil {
		t.Fatalf("write test binding template: %v", err)
	}
	rendered := "export " + name + "='" + fixture + "'\n"
	if err := os.WriteFile(filepath.Join(cacheDir, "secrets-env.sh"), []byte(rendered), 0o600); err != nil {
		t.Fatalf("write test render cache: %v", err)
	}

	t.Setenv("XDG_CONFIG_HOME", configHome)
	t.Setenv("XDG_CACHE_HOME", cacheHome)
	t.Setenv(secrets.AllowAgentSecretsEnv, "1")
	t.Setenv(name, fixture)

	repo := t.TempDir()
	gitE2E(t, repo, "init", "-q", "-b", "main")
	gitE2E(t, repo, "config", "user.email", "test@test.local")
	gitE2E(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "README.md"), []byte("seed\n"), 0o644); err != nil {
		t.Fatalf("write seed: %v", err)
	}
	gitE2E(t, repo, "add", "README.md")
	gitE2E(t, repo, "commit", "-qm", "seed")
	t.Chdir(repo)

	if out, code := runWeave(t, "add", "PTY capture redaction"); code != 0 {
		t.Fatalf("add failed: code=%d output_bytes=%d", code, len(out))
	}
	// The command substitution is an inter-process shell mechanism, not a
	// capture sink. It must receive the unmodified render so eval can populate
	// the variable; only the later PTY output is redacted.
	script := `unset WEAVE_CAPTURE_SECRET; eval "$(bashy secret env)"; test -n "$WEAVE_CAPTURE_SECRET"; printf "%s final bytes" "$WEAVE_CAPTURE_SECRET"`
	startOut, code := runWeave(t, "start", "--run", "1", "--pty", "always", "--", "bashy", "-c", script)
	if code != 0 {
		t.Fatalf("PTY start failed: code=%d output_bytes=%d", code, len(startOut))
	}

	canonicalRoot, err := weaveRepoRoot(repo)
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	queueDir, err := weaveQueueDir(canonicalRoot)
	if err != nil {
		t.Fatalf("resolve queue dir: %v", err)
	}
	logPath := filepath.Join(queueDir, "logs", "issue-1.log")
	logData, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read capture log: %v", err)
	}
	assertNoCaptureValue(t, logPath, logData, fixture)
	if !bytes.HasSuffix(logData, []byte(" final bytes")) {
		t.Fatalf("redacted capture tail was truncated: path=%s bytes=%d", logPath, len(logData))
	}

	if out, code := runWeave(t, "add", "returned text redaction"); code != 0 {
		t.Fatalf("second add failed: code=%d output_bytes=%d", code, len(out))
	}
	resultScript := `printf "%s result-tail" "$WEAVE_CAPTURE_SECRET"`
	out, code := runWeave(t, "start", "--run", "2", "--pty", "never", "--", "bashy", "-c", resultScript)
	if code != 0 {
		t.Fatalf("non-PTY start failed: code=%d output_bytes=%d", code, len(out))
	}
	assertNoCaptureValue(t, "weave start result", []byte(out), fixture)
	if !strings.Contains(out, "result-tail") {
		t.Fatal("redacted run result lost its non-secret tail")
	}
}

func assertNoCaptureValue(t *testing.T, path string, captured []byte, value string) {
	t.Helper()
	if offset := bytes.Index(captured, []byte(value)); offset >= 0 {
		t.Fatalf("registered vault value reached capture: path=%s offset=%d", path, offset)
	}
}
