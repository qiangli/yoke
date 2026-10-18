package az

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeSecretFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "sp-secret.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearCredEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvSubscription, EnvSecretFile, EnvUseCLILogin, EnvTenant, EnvProfile} {
		t.Setenv(k, "")
	}
	os.Unsetenv(EnvSubscription)
	os.Unsetenv(EnvSecretFile)
	os.Unsetenv(EnvUseCLILogin)
	os.Unsetenv(EnvTenant)
	os.Unsetenv(EnvProfile)
}

// No ambient fallback: a missing subscription fails even when a login
// session is declared.
func TestResolveMissingSubscription(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvUseCLILogin, "1")
	_, err := ResolveCredentials()
	gerr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type = %T, want *Error", err)
	}
	if gerr.Code != CodeMissingCredentials {
		t.Errorf("code = %q, want missing-credentials", gerr.Code)
	}
}

// No credential material at all also fails.
func TestResolveMissingMaterial(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvSubscription, "s457-fixture-sub")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeMissingCredentials {
		t.Fatalf("err = %v, want missing-credentials", err)
	}
}

// Ambiguity (file + login flag) fails instead of picking one.
func TestResolveAmbiguousMaterial(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvSubscription, "s457-fixture-sub")
	t.Setenv(EnvSecretFile, "/tmp/sp.txt")
	t.Setenv(EnvUseCLILogin, "1")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeInvalidInput {
		t.Fatalf("err = %v, want invalid-input", err)
	}
}

// Missing file fails.
func TestResolveMissingFile(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvSubscription, "s457-fixture-sub")
	t.Setenv(EnvSecretFile, filepath.Join(t.TempDir(), "nope.txt"))
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeMissingCredentials {
		t.Fatalf("err = %v, want missing-credentials", err)
	}
}

// Whitespace in the subscription is rejected.
func TestResolveWhitespaceSubscription(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvSubscription, "sub one")
	t.Setenv(EnvUseCLILogin, "1")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeInvalidInput {
		t.Fatalf("err = %v, want invalid-input", err)
	}
}

func TestResolveSecretFile(t *testing.T) {
	clearCredEnv(t)
	f := writeSecretFile(t, "s457-canary-az-secret")
	t.Setenv(EnvSubscription, "s457-fixture-sub")
	t.Setenv(EnvSecretFile, f)
	t.Setenv(EnvTenant, "s457-fixture-tenant")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != AuthServicePrincipal || c.Subscription != "s457-fixture-sub" || c.SecretFile != f || c.Tenant != "s457-fixture-tenant" {
		t.Errorf("creds = %+v, wrong resolution", c)
	}
}

func TestResolveCLILogin(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvSubscription, "s457-fixture-sub")
	t.Setenv(EnvUseCLILogin, "1")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != AuthCLILogin {
		t.Errorf("kind = %q, want cli-login", c.Kind)
	}
}

// The redacted scope carries subscription + kind + source, never secrets.
func TestRedactedScope(t *testing.T) {
	clearCredEnv(t)
	const canary = "CANARY-AZ-SP-SECRET-9f8e7d6c"
	f := writeSecretFile(t, canary)
	t.Setenv(EnvSubscription, "s457-fixture-sub")
	t.Setenv(EnvSecretFile, f)
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	scope := c.RedactedScope()
	if strings.Contains(scope, canary) {
		t.Error("redacted scope leaks secret file contents")
	}
	if strings.Contains(scope, f) {
		t.Error("redacted scope leaks the full secret path (basename only)")
	}
	for _, want := range []string{"s457-fixture-sub", "service-principal", "sp-secret.txt"} {
		if !strings.Contains(scope, want) {
			t.Errorf("redacted scope %q missing %q", scope, want)
		}
	}
}

// Login never implies provisioning permission — even with valid credentials.
func TestProvisioningNeverInferredFromLogin(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvSubscription, "s457-fixture-sub")
	t.Setenv(EnvUseCLILogin, "1")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	perr := ProvisioningPermitted(c)
	if perr == nil {
		t.Fatal("login inferred provisioning permission")
	}
	if perr.Code != CodePermissionDenied {
		t.Errorf("code = %q, want permission-denied", perr.Code)
	}
	if !strings.Contains(perr.Message, "Sprint 465") {
		t.Errorf("denial must name the live-check home, got %q", perr.Message)
	}
}
