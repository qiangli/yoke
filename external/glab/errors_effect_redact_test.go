package glab

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
	err := Unsupported("trigger", "pipelines:trigger")
	if err.Code != CodeUnsupported {
		t.Fatalf("code = %q, want unsupported-capability", err.Code)
	}
	if !strings.Contains(err.Message, "durable-event") {
		t.Errorf("message %q must name the event-story home", err.Message)
	}
	err = Unsupported("admin", "admin:instance")
	if !strings.Contains(err.Message, "UNCLAIMED") {
		t.Errorf("message %q must stay UNCLAIMED", err.Message)
	}
}

func TestEmitErrorEnvelope(t *testing.T) {
	err := NewError(CodePermissionDenied, "glab: denied for test-target", nil).WithOp("read", "issues:list")
	var buf bytes.Buffer
	if code := err.EmitError(&buf, "glab issues list"); code != weavecli.ExitGenericFail {
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
	const canaryToken = "CANARY-GLAB-TOKEN-7f3a9c2e1b5d"
	argv := []string{"glab", "issue", "create", "--token", canaryToken, "--title", "hello", "GITLAB_TOKEN=" + canaryToken}
	out, masked := RedactArgs(argv, canaryToken)
	joined := strings.Join(out, " ")
	if strings.Contains(joined, canaryToken) {
		t.Errorf("redacted argv leaks canary: %q", joined)
	}
	if masked == 0 {
		t.Error("nothing masked")
	}
	if out[0] != "glab" || out[1] != "issue" {
		t.Errorf("redaction ate non-secret args: %q", out)
	}
}

func TestRedactTextCanary(t *testing.T) {
	const canary = "CANARY-PROVIDER-ERROR-SECRET-abc123"
	body := `provider failure: {"error": "invalid_token ` + canary + `", "target": "issues:list"}`
	red := RedactText(body, canary)
	if strings.Contains(red, canary) {
		t.Errorf("provider error body leaks canary: %q", red)
	}
	if !strings.Contains(red, "issues:list") {
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
	return Credentials{Host: HostDefault, Kind: AuthToken, HasTokenEnv: true}
}

// glab ships no dry-run: every preview refuses without execution.
func TestDryRunAlwaysRefuses(t *testing.T) {
	for _, svc := range []string{"issues", "merge-requests", "pipelines", "bogus"} {
		d := DryRun{Service: svc, Operation: "create", Target: svc + ":create", Credentials: testCreds()}
		if err := d.Check(); err == nil {
			t.Errorf("service %q dry-run must refuse", svc)
		} else if svc != "bogus" && err.Code != CodeUnsupported {
			t.Errorf("service %q code = %q, want unsupported-capability", svc, err.Code)
		}
		var buf bytes.Buffer
		if code := d.Emit(&buf, "glab "+svc+" create --dry-run"); code != weavecli.ExitInvalidArg {
			t.Errorf("service %q emit exit = %d, want 2", svc, code)
		}
		var env map[string]any
		if err := json.Unmarshal(buf.Bytes(), &env); err != nil {
			t.Fatal(err)
		}
		if env["schema_version"] != weavecli.SchemaVersion || env["status"] != "error" {
			t.Errorf("service %q envelope = %v, want loom-v2 error", svc, env)
		}
	}
}
