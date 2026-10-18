package gws

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

func TestErrorExits(t *testing.T) {
	cases := map[Code]int{
		CodeInvalidInput:       weavecli.ExitInvalidArg, // 2 native
		CodeUnsupported:        weavecli.ExitInvalidArg, // 2 native
		CodeCancelled:          130,
		CodeTimeout:            124,
		CodeMissingCredentials: weavecli.ExitGenericFail,
		CodePermissionDenied:   weavecli.ExitGenericFail,
		CodeCursorMismatch:     weavecli.ExitGenericFail,
		CodeProviderFailure:    weavecli.ExitGenericFail,
	}
	for code, want := range cases {
		if got := NewError(code, "x", nil).Exit(); got != want {
			t.Errorf("code %q exit = %d, want %d", code, got, want)
		}
	}
}

func TestErrorRetryable(t *testing.T) {
	for _, code := range []Code{CodeRateLimit, CodeTimeout, CodeProviderFailure} {
		if !NewError(code, "x", nil).Retryable {
			t.Errorf("code %q should default retryable", code)
		}
	}
	if NewError(CodeInvalidInput, "x", nil).Retryable {
		t.Error("invalid-input must not default retryable")
	}
}

func TestUnsupportedNamesHome(t *testing.T) {
	err := Unsupported("subscribe", "events:subscribe")
	if err.Code != CodeUnsupported {
		t.Fatalf("code = %q, want unsupported-capability", err.Code)
	}
	if !strings.Contains(err.Message, "5548854717bc") {
		t.Errorf("message %q must name the event story home", err.Message)
	}
	err = Unsupported("delete", "admin:directory")
	if !strings.Contains(err.Message, "UNCLAIMED") {
		t.Errorf("message %q must stay UNCLAIMED", err.Message)
	}
}

func TestEmitErrorEnvelope(t *testing.T) {
	err := NewError(CodePermissionDenied, "gws: denied for test-target", nil).WithOp("read", "gmail:messages")
	var buf bytes.Buffer
	if code := err.EmitError(&buf, "gws gmail list"); code != weavecli.ExitGenericFail {
		t.Fatalf("exit = %d, want 1", code)
	}
	var env map[string]any
	if uerr := json.Unmarshal(buf.Bytes(), &env); uerr != nil {
		t.Fatal(uerr)
	}
	if env["schema_version"] != weavecli.SchemaVersion {
		t.Errorf("schema_version = %v, want %q", env["schema_version"], weavecli.SchemaVersion)
	}
	if env["status"] != "error" {
		t.Errorf("status = %v, want error", env["status"])
	}
}

func TestRedactArgsCanary(t *testing.T) {
	const canaryToken = "CANARY-GWS-TOKEN-7f3a9c2e1b5d"
	const canaryKey = "CANARY-GWS-API-KEY-zz11yy22"
	argv := []string{"gws", "gmail", "send", "--token", canaryToken, "--account", "tester@example.com", "APIKEY=" + canaryKey}
	out, masked := RedactArgs(argv, canaryToken, canaryKey)
	joined := strings.Join(out, " ")
	if strings.Contains(joined, canaryToken) || strings.Contains(joined, canaryKey) {
		t.Errorf("redacted argv leaks canary: %q", joined)
	}
	if masked == 0 {
		t.Error("nothing masked")
	}
	// Non-secret args survive.
	if out[0] != "gws" || out[1] != "gmail" {
		t.Errorf("redaction ate non-secret args: %q", out)
	}
}

func TestRedactTextCanary(t *testing.T) {
	const canary = "CANARY-PROVIDER-ERROR-SECRET-abc123"
	body := `provider failure: {"error": "invalid_grant ` + canary + `", "target": "gmail:messages"}`
	red := RedactText(body, canary)
	if strings.Contains(red, canary) {
		t.Errorf("provider error body leaks canary: %q", red)
	}
	if !strings.Contains(red, "gmail:messages") {
		t.Errorf("redaction ate safe context: %q", red)
	}
	if !strings.Contains(red, Redacted) {
		t.Error("no placeholder left in redacted text")
	}
	// Empty secret list masks nothing.
	if got := RedactText("hello"); got != "hello" {
		t.Errorf("empty-secret redaction changed text: %q", got)
	}
}

func TestUploadValidation(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "small.bin")
	if err := os.WriteFile(small, []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	big := filepath.Join(dir, "big.bin")
	fh, err := os.Create(big)
	if err != nil {
		t.Fatal(err)
	}
	if err := fh.Truncate(MaxUploadBytes + 1); err != nil {
		t.Fatal(err)
	}
	fh.Close()

	// Happy path: drive + explicit file + media type.
	if err := (Upload{Service: "drive", LocalPath: small, MediaType: "application/octet-stream"}).Validate(); err != nil {
		t.Errorf("valid upload rejected: %v", err)
	}
	// Unknown service.
	if err := (Upload{Service: "bogus", LocalPath: small, MediaType: "text/plain"}).Validate(); err == nil {
		t.Error("unknown service must fail")
	}
	// Non-upload service.
	if err := (Upload{Service: "gmail", LocalPath: small, MediaType: "text/plain"}).Validate(); err == nil {
		t.Error("gmail upload must fail (unsupported)")
	} else if err.Code != CodeUnsupported {
		t.Errorf("code = %q, want unsupported-capability", err.Code)
	}
	// No input source.
	if err := (Upload{Service: "drive", MediaType: "text/plain"}).Validate(); err == nil {
		t.Error("missing input must fail")
	}
	// Both input sources.
	if err := (Upload{Service: "drive", LocalPath: small, Stdin: true, MediaType: "text/plain"}).Validate(); err == nil {
		t.Error("dual input sources must fail")
	}
	// Missing media type.
	if err := (Upload{Service: "drive", LocalPath: small}).Validate(); err == nil {
		t.Error("missing media type must fail")
	}
	// Oversize file.
	if err := (Upload{Service: "drive", LocalPath: big, MediaType: "application/octet-stream"}).Validate(); err == nil {
		t.Error("oversize upload must fail")
	} else if err.Code != CodeOutputLimit {
		t.Errorf("code = %q, want output-limit", err.Code)
	}
	// Directory.
	if err := (Upload{Service: "drive", LocalPath: dir, MediaType: "text/plain"}).Validate(); err == nil {
		t.Error("directory upload must fail")
	}
}

func TestReadBounded(t *testing.T) {
	if _, err := ReadBounded(strings.NewReader("abc"), 1024); err != nil {
		t.Errorf("small payload rejected: %v", err)
	}
	big := strings.NewReader(strings.Repeat("x", int(MaxPayloadBytes)+1))
	if _, err := ReadBounded(big, MaxPayloadBytes); err == nil {
		t.Error("over-limit payload must fail explicitly")
	} else if err.Code != CodeOutputLimit {
		t.Errorf("code = %q, want output-limit", err.Code)
	}
}

func testCreds() Credentials {
	return Credentials{Account: "tester@example.com", Kind: AuthOAuth, CredentialsFile: "/tmp/creds.json"}
}

func TestDryRun(t *testing.T) {
	// Unknown service fails.
	if err := (DryRun{Service: "bogus", Operation: "create", Credentials: testCreds()}).Check(); err == nil {
		t.Error("unknown service dry-run must fail")
	}
	// Deferred capability refuses without execution.
	if err := (DryRun{Service: "events", Operation: "subscribe", Credentials: testCreds()}).Check(); err == nil {
		t.Error("deferred events subscribe must refuse")
	} else if err.Code != CodeUnsupported {
		t.Errorf("code = %q, want unsupported-capability", err.Code)
	}
	// Missing credentials fail before preview.
	if err := (DryRun{Service: "calendar", Operation: "create"}).Check(); err == nil {
		t.Error("credential-less dry-run must fail")
	}
	// Happy path emits a loom-v2 preview envelope, never execution evidence.
	var buf bytes.Buffer
	d := DryRun{Service: "calendar", Operation: "create", Target: "calendar:events", Credentials: testCreds()}
	if code := d.Emit(&buf, "gws calendar create --dry-run"); code != weavecli.ExitOK {
		t.Fatalf("exit = %d, want 0", code)
	}
	var env map[string]any
	if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
		t.Fatal(err)
	}
	if env["schema_version"] != weavecli.SchemaVersion || env["status"] != "ok" {
		t.Errorf("envelope = %v, want loom-v2 ok", env)
	}
}
