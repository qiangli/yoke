package broker

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// brokerVaultFixture installs a vault-projected name with a rendered value in
// this process's environment; not credential-shaped, so only exact-value
// registration can mask it.
func brokerVaultFixture(t *testing.T, localName, value string) {
	t.Helper()
	xdg := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("XDG_CACHE_HOME", t.TempDir())
	path := filepath.Join(xdg, "bashy", "secrets.map")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(localName+"=@vault-item\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(localName, value)
}

// The door's capture log (turns.jsonl) is the LLM door's persisted record of
// requests and responses. A vault value with no recognizable shape embedded
// in prompt or response text survives shape and host masking; the exact-value
// layer is what catches it.
func TestCaptureRedactMasksVaultValues(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	brokerVaultFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")

	body := `{"messages":[{"role":"user","content":"rotate sycamore-lantern-42 tonight"}]}`
	got := captureRedact(body)
	if strings.Contains(got, "sycamore-lantern-42") {
		t.Fatalf("vault value survived capture redaction: %q", got)
	}
	if !strings.Contains(got, "[redacted:DEPLOY_TOKEN]") {
		t.Fatalf("capture body missing marker: %q", got)
	}
}

// The pre-existing shape and host-identity layers still run after the
// exact-value layer (a sk- token stays masked even when it is not a vault
// value).
func TestCaptureRedactKeepsShapeMasking(t *testing.T) {
	t.Setenv("BASHY_HOME", t.TempDir())
	brokerVaultFixture(t, "DEPLOY_TOKEN", "sycamore-lantern-42")

	key := "sk-" + strings.Repeat("0123456789abcdef", 3)
	got := captureRedact("Authorization: Bearer " + key)
	if strings.Contains(got, key) {
		t.Fatalf("shape-masked token survived: %q", got)
	}
}
