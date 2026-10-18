package gws

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCredFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "creds.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearCredEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvAccount, EnvAuthKind, EnvCredentialsFile, EnvServiceAccountFile, EnvProfile} {
		t.Setenv(k, "")
	}
	os.Unsetenv(EnvAccount)
	os.Unsetenv(EnvAuthKind)
	os.Unsetenv(EnvCredentialsFile)
	os.Unsetenv(EnvServiceAccountFile)
	os.Unsetenv(EnvProfile)
}

// No ambient fallback: a missing account fails with missing-credentials.
func TestResolveMissingAccount(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvCredentialsFile, "/tmp/creds.json")
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
	t.Setenv(EnvAccount, "tester@example.com")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeMissingCredentials {
		t.Fatalf("err = %v, want missing-credentials", err)
	}
}

// Ambiguity (both files) fails instead of picking one.
func TestResolveAmbiguousMaterial(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvAccount, "tester@example.com")
	t.Setenv(EnvCredentialsFile, "/tmp/a.json")
	t.Setenv(EnvServiceAccountFile, "/tmp/b.json")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeInvalidInput {
		t.Fatalf("err = %v, want invalid-input", err)
	}
}

// Conflicting auth kind fails.
func TestResolveConflictingKind(t *testing.T) {
	clearCredEnv(t)
	f := writeCredFile(t, `{"type":"service_account"}`)
	t.Setenv(EnvAccount, "tester@example.com")
	t.Setenv(EnvServiceAccountFile, f)
	t.Setenv(EnvAuthKind, "oauth")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeInvalidInput {
		t.Fatalf("err = %v, want invalid-input", err)
	}
}

// Missing file fails.
func TestResolveMissingFile(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvAccount, "tester@example.com")
	t.Setenv(EnvCredentialsFile, filepath.Join(t.TempDir(), "nope.json"))
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeMissingCredentials {
		t.Fatalf("err = %v, want missing-credentials", err)
	}
}

func TestResolveOAuth(t *testing.T) {
	clearCredEnv(t)
	f := writeCredFile(t, `{"client_id":"x"}`)
	t.Setenv(EnvAccount, "tester@example.com")
	t.Setenv(EnvCredentialsFile, f)
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != AuthOAuth || c.Account != "tester@example.com" || c.CredentialsFile != f {
		t.Errorf("creds = %+v, wrong resolution", c)
	}
}

func TestResolveServiceAccount(t *testing.T) {
	clearCredEnv(t)
	f := writeCredFile(t, `{"type":"service_account"}`)
	t.Setenv(EnvAccount, "svc@project.iam.gserviceaccount.com")
	t.Setenv(EnvServiceAccountFile, f)
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != AuthServiceAccount {
		t.Errorf("kind = %q, want service-account", c.Kind)
	}
}

// The redacted scope carries account + kind + basename, never key material.
func TestRedactedScope(t *testing.T) {
	clearCredEnv(t)
	const canary = "CANARY-SERVICE-ACCOUNT-PRIVATE-KEY-9f8e7d6c"
	f := writeCredFile(t, canary)
	t.Setenv(EnvAccount, "svc@project.iam.gserviceaccount.com")
	t.Setenv(EnvServiceAccountFile, f)
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	scope := c.RedactedScope()
	if strings.Contains(scope, canary) {
		t.Error("redacted scope leaks credential file contents")
	}
	if strings.Contains(scope, f) {
		t.Error("redacted scope leaks the full credential path (basename only)")
	}
	for _, want := range []string{"svc@project.iam.gserviceaccount.com", "service-account", "creds.json"} {
		if !strings.Contains(scope, want) {
			t.Errorf("redacted scope %q missing %q", scope, want)
		}
	}
}
