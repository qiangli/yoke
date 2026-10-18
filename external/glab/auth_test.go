package glab

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTokenFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token.txt")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearCredEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvToken, EnvTokenFile, EnvHost, EnvProfile} {
		t.Setenv(k, "")
	}
	os.Unsetenv(EnvToken)
	os.Unsetenv(EnvTokenFile)
	os.Unsetenv(EnvHost)
	os.Unsetenv(EnvProfile)
}

// No ambient fallback: missing token material fails with missing-credentials.
func TestResolveMissingMaterial(t *testing.T) {
	clearCredEnv(t)
	_, err := ResolveCredentials()
	gerr, ok := err.(*Error)
	if !ok {
		t.Fatalf("err type = %T, want *Error", err)
	}
	if gerr.Code != CodeMissingCredentials {
		t.Errorf("code = %q, want missing-credentials", gerr.Code)
	}
}

// Ambiguity (both sources) fails instead of picking one.
func TestResolveAmbiguousMaterial(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvToken, "s457-canary-glab-token")
	t.Setenv(EnvTokenFile, "/tmp/tok.txt")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeInvalidInput {
		t.Fatalf("err = %v, want invalid-input", err)
	}
}

// Missing file fails.
func TestResolveMissingFile(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvTokenFile, filepath.Join(t.TempDir(), "nope.txt"))
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeMissingCredentials {
		t.Fatalf("err = %v, want missing-credentials", err)
	}
}

func TestResolveTokenEnv(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvToken, "s457-canary-glab-token")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != AuthToken || !c.HasTokenEnv {
		t.Errorf("creds = %+v, wrong resolution", c)
	}
	if c.Host != HostDefault {
		t.Errorf("host = %q, want default %q", c.Host, HostDefault)
	}
}

func TestResolveTokenFileAndHost(t *testing.T) {
	clearCredEnv(t)
	f := writeTokenFile(t, "s457-canary-glab-token")
	t.Setenv(EnvTokenFile, f)
	t.Setenv(EnvHost, "https://gitlab.example.com")
	t.Setenv(EnvProfile, "work")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if c.TokenFile != f || c.Host != "https://gitlab.example.com" || c.Profile != "work" {
		t.Errorf("creds = %+v, wrong resolution", c)
	}
}

// The redacted scope carries host + kind + source, never token material.
func TestRedactedScope(t *testing.T) {
	clearCredEnv(t)
	const canary = "CANARY-GLAB-TOKEN-9f8e7d6c5b4a"
	f := writeTokenFile(t, canary)
	t.Setenv(EnvTokenFile, f)
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	scope := c.RedactedScope()
	if strings.Contains(scope, canary) {
		t.Error("redacted scope leaks token file contents")
	}
	if strings.Contains(scope, f) {
		t.Error("redacted scope leaks the full token path (basename only)")
	}
	for _, want := range []string{"gitlab.com", "token", "token.txt"} {
		if !strings.Contains(scope, want) {
			t.Errorf("redacted scope %q missing %q", scope, want)
		}
	}
	// Env-sourced tokens report "env", never the value.
	clearCredEnv(t)
	t.Setenv(EnvToken, canary)
	c, err = ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if scope := c.RedactedScope(); strings.Contains(scope, canary) {
		t.Errorf("redacted scope leaks env token: %q", scope)
	}
}

// Login never implies provisioning permission — even with valid credentials.
func TestProvisioningNeverInferredFromLogin(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvToken, "s457-canary-glab-token")
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
	if strings.Contains(perr.Message, "s457-canary-glab-token") {
		t.Error("denial leaks the token")
	}
	if !strings.Contains(perr.Message, "Sprint 465") {
		t.Errorf("denial must name the live-check home, got %q", perr.Message)
	}
}
