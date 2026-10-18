package aws

import (
	"bytes"
	"encoding/json"
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
	err := Unsupported("write", "iam:write")
	if err.Code != CodeUnsupported {
		t.Fatalf("code = %q, want unsupported-capability", err.Code)
	}
	if !strings.Contains(err.Message, "UNCLAIMED") {
		t.Errorf("message %q must stay UNCLAIMED", err.Message)
	}
}

func TestEmitErrorEnvelope(t *testing.T) {
	err := NewError(CodePermissionDenied, "aws: denied for test-target", nil).WithOp("read", "ec2:describe-instances")
	var buf bytes.Buffer
	if code := err.EmitError(&buf, "aws ec2 describe-instances"); code != weavecli.ExitGenericFail {
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
	const canaryKey = "CANARY-AWS-SECRET-KEY-zz11yy22"
	argv := []string{"aws", "ec2", "run-instances", "--key", canaryKey, "AWS_SECRET_ACCESS_KEY=" + canaryKey}
	out, masked := RedactArgs(argv, canaryKey)
	joined := strings.Join(out, " ")
	if strings.Contains(joined, canaryKey) {
		t.Errorf("redacted argv leaks canary: %q", joined)
	}
	if masked == 0 {
		t.Error("nothing masked")
	}
	if out[0] != "aws" || out[1] != "ec2" {
		t.Errorf("redaction ate non-secret args: %q", out)
	}
}

func TestRedactTextCanary(t *testing.T) {
	const canary = "CANARY-PROVIDER-ERROR-SECRET-abc123"
	body := `provider failure: {"error": "AuthFailure ` + canary + `", "target": "ec2:run-instances"}`
	red := RedactText(body, canary)
	if strings.Contains(red, canary) {
		t.Errorf("provider error body leaks canary: %q", red)
	}
	if !strings.Contains(red, "ec2:run-instances") {
		t.Errorf("redaction ate safe context: %q", red)
	}
	if !strings.Contains(red, Redacted) {
		t.Error("no placeholder left in redacted text")
	}
	if got := RedactText("hello"); got != "hello" {
		t.Errorf("empty-secret redaction changed text: %q", got)
	}
}

func testCreds() Credentials {
	return Credentials{Profile: "fixture", Kind: AuthEnvKeys, Region: "us-west-2"}
}

// EC2/S3 dry runs validate through the provider flag; everything else
// refuses without execution.
func TestDryRun(t *testing.T) {
	// Unknown service fails.
	if err := (DryRun{Service: "bogus", Operation: "run", Credentials: testCreds()}).Check(); err == nil {
		t.Error("unknown service dry-run must fail")
	}
	// IAM has no dry run: refuses.
	if err := (DryRun{Service: "iam", Operation: "create-user", Credentials: testCreds()}).Check(); err == nil {
		t.Error("iam dry-run must refuse")
	} else if err.Code != CodeUnsupported {
		t.Errorf("code = %q, want unsupported-capability", err.Code)
	}
	// Missing credentials fail before preview.
	if err := (DryRun{Service: "ec2", Operation: "run-instances"}).Check(); err == nil {
		t.Error("credential-less dry-run must fail")
	}
	// Happy paths emit a loom-v2 preview envelope with the provider flag,
	// never execution evidence.
	for _, tc := range []struct{ service, flag string }{
		{"ec2", "--dry-run"},
		{"s3", "--dryrun"},
	} {
		var buf bytes.Buffer
		d := DryRun{Service: tc.service, Operation: "create", Target: tc.service + ":create", Credentials: testCreds()}
		if code := d.Emit(&buf, "aws "+tc.service+" create --dry-run"); code != weavecli.ExitOK {
			t.Fatalf("%s: exit = %d, want 0", tc.service, code)
		}
		var env map[string]any
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env["schema_version"] != weavecli.SchemaVersion || env["status"] != "ok" {
			t.Errorf("%s: envelope = %v, want loom-v2 ok", tc.service, env)
		}
	}
}
