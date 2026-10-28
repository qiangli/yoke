package aws

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeCredFile(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "credentials")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func clearCredEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{EnvProfile, EnvSharedCredentials, EnvAccessKeyID, EnvSecretAccessKey, EnvRegion, EnvDefaultRegion} {
		t.Setenv(k, "")
	}
	os.Unsetenv(EnvProfile)
	os.Unsetenv(EnvSharedCredentials)
	os.Unsetenv(EnvAccessKeyID)
	os.Unsetenv(EnvSecretAccessKey)
	os.Unsetenv(EnvRegion)
	os.Unsetenv(EnvDefaultRegion)
	t.Setenv("HOME", t.TempDir())
}

// No ambient fallback: a missing profile fails with missing-credentials.
func TestResolveMissingProfile(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvAccessKeyID, "AKIAEXAMPLE")
	t.Setenv(EnvSecretAccessKey, "s457-canary")
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
	t.Setenv(EnvProfile, "fixture")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeMissingCredentials {
		t.Fatalf("err = %v, want missing-credentials", err)
	}
}

// Ambiguity (file + env keys) fails instead of picking one.
func TestResolveAmbiguousMaterial(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvSharedCredentials, writeCredFile(t, "[fixture]\naws_access_key_id = AKIAEXAMPLE"))
	t.Setenv(EnvAccessKeyID, "AKIAEXAMPLE")
	t.Setenv(EnvSecretAccessKey, "s457-canary")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeInvalidInput {
		t.Fatalf("err = %v, want invalid-input", err)
	}
}

// Half a key pair is not a credential source.
func TestResolveHalfKeyPairFails(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvAccessKeyID, "AKIAEXAMPLE")
	_, err := ResolveCredentials()
	if gerr, ok := err.(*Error); !ok || gerr.Code != CodeMissingCredentials {
		t.Fatalf("err = %v, want missing-credentials", err)
	}
}

func TestResolveSharedFile(t *testing.T) {
	clearCredEnv(t)
	f := writeCredFile(t, "[fixture]\naws_access_key_id = AKIAEXAMPLE")
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvSharedCredentials, f)
	t.Setenv(EnvRegion, "us-west-2")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != AuthSharedFile || c.Profile != "fixture" || c.CredentialsFile != f || c.Region != "us-west-2" {
		t.Errorf("creds = %+v, wrong resolution", c)
	}
}

func TestResolveEnvKeys(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvAccessKeyID, "AKIAEXAMPLE")
	t.Setenv(EnvSecretAccessKey, "s457-canary-secret")
	c, err := ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if c.Kind != AuthEnvKeys {
		t.Errorf("kind = %q, want env-keys", c.Kind)
	}
	if c.Region != "" {
		t.Errorf("region = %q, want empty (default-chain in scope)", c.Region)
	}
}

// The redacted scope carries profile + kind + source, never key material.
func TestRedactedScope(t *testing.T) {
	clearCredEnv(t)
	const canary = "CANARY-AWS-SECRET-KEY-9f8e7d6c"
	f := writeCredFile(t, "[fixture]\naws_secret_access_key = "+canary)
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvSharedCredentials, f)
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
	for _, want := range []string{"fixture", "shared-file", "credentials", "default-chain"} {
		if !strings.Contains(scope, want) {
			t.Errorf("redacted scope %q missing %q", scope, want)
		}
	}
	// Env-key secrets never appear in the scope either.
	clearCredEnv(t)
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvAccessKeyID, "AKIAEXAMPLE")
	t.Setenv(EnvSecretAccessKey, canary)
	c, err = ResolveCredentials()
	if err != nil {
		t.Fatal(err)
	}
	if scope := c.RedactedScope(); strings.Contains(scope, canary) {
		t.Errorf("redacted scope leaks env secret: %q", scope)
	}
}

// Login never implies provisioning permission — even with valid credentials.
func TestProvisioningNeverInferredFromLogin(t *testing.T) {
	clearCredEnv(t)
	t.Setenv(EnvProfile, "fixture")
	t.Setenv(EnvAccessKeyID, "AKIAEXAMPLE")
	t.Setenv(EnvSecretAccessKey, "s457-canary")
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
	if strings.Contains(perr.Message, "s457-canary") {
		t.Error("denial leaks the secret")
	}
	// The denial must point at the check that DOES answer, so a caller
	// knows what to call instead of being told only "no".
	if !strings.Contains(perr.Message, "ProvisioningAllowed") {
		t.Errorf("denial must name the live check, got %q", perr.Message)
	}
}
