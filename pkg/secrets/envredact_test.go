package secrets

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeVaultTemplate writes a binding template under the test's XDG config
// home so VaultEnvNames resolves exactly the names the test declares.
func writeVaultTemplate(t *testing.T, body string) {
	t.Helper()
	path := defaultTemplatePath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The exact value present in the environment under a vault-projected name is
// masked; nothing else is touched. The fixture is deliberately NOT
// credential-shaped, so only exact-value registration can mask it.
func TestEnvValueRedactorMasksRenderedValues(t *testing.T) {
	configureSecretsTestEnv(t)
	writeVaultTemplate(t, "DEPLOY_TOKEN=@deploy-token\nEDITOR=vim\n")
	t.Setenv("DEPLOY_TOKEN", "sycamore-lantern-42")

	r := EnvValueRedactor(os.Environ())
	got := string(r.Redact([]byte("the key is sycamore-lantern-42, rotated daily")))
	if strings.Contains(got, "sycamore-lantern-42") {
		t.Fatalf("vault value survived: %q", got)
	}
	if !strings.Contains(got, "[redacted:DEPLOY_TOKEN]") {
		t.Fatalf("no named marker: %q", got)
	}
	plain := string(r.Redact([]byte("ordinary words stay untouched")))
	if plain != "ordinary words stay untouched" {
		t.Fatalf("plain text altered: %q", plain)
	}
}

// A vault name whose value is not in the environment is not registered (there
// is nothing to mask), and a missing template registers nothing at all.
func TestEnvValueRedactorWithoutValuesRegistersNothing(t *testing.T) {
	configureSecretsTestEnv(t)
	writeVaultTemplate(t, "DEPLOY_TOKEN=@deploy-token\n")
	// DEPLOY_TOKEN deliberately absent from the environment.
	r := EnvValueRedactor([]string{"PATH=/bin"})
	got := string(r.Redact([]byte("sycamore-lantern-42")))
	if got != "sycamore-lantern-42" {
		t.Fatalf("masked without registration: %q", got)
	}

	r = EnvValueRedactor([]string{"PATH=/bin"})
	if got := string(r.Redact([]byte("anything"))); got != "anything" {
		t.Fatalf("no-template redactor altered text: %q", got)
	}
}

// RedactKnownValues is the one-string convenience over EnvValueRedactor.
func TestRedactKnownValues(t *testing.T) {
	configureSecretsTestEnv(t)
	writeVaultTemplate(t, "DEPLOY_TOKEN=@deploy-token\n")
	t.Setenv("DEPLOY_TOKEN", "sycamore-lantern-42")
	if got := RedactKnownValues("token: sycamore-lantern-42"); got != "token: [redacted:DEPLOY_TOKEN]" {
		t.Fatalf("RedactKnownValues = %q", got)
	}
}
