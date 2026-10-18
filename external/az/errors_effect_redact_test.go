package az

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
	err := Unsupported("assign", "role:assign")
	if err.Code != CodeUnsupported {
		t.Fatalf("code = %q, want unsupported-capability", err.Code)
	}
	if !strings.Contains(err.Message, "UNCLAIMED") {
		t.Errorf("message %q must stay UNCLAIMED", err.Message)
	}
}

func TestEmitErrorEnvelope(t *testing.T) {
	err := NewError(CodePermissionDenied, "az: denied for test-target", nil).WithOp("read", "group:list")
	var buf bytes.Buffer
	if code := err.EmitError(&buf, "az group list"); code != weavecli.ExitGenericFail {
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
	const canarySecret = "CANARY-AZ-SP-SECRET-zz11yy22"
	argv := []string{"az", "login", "--service-principal", "--password", canarySecret, "AZURE_SECRET=" + canarySecret}
	out, masked := RedactArgs(argv, canarySecret)
	joined := strings.Join(out, " ")
	if strings.Contains(joined, canarySecret) {
		t.Errorf("redacted argv leaks canary: %q", joined)
	}
	if masked == 0 {
		t.Error("nothing masked")
	}
	if out[0] != "az" || out[1] != "login" {
		t.Errorf("redaction ate non-secret args: %q", out)
	}
}

func TestRedactTextCanary(t *testing.T) {
	const canary = "CANARY-PROVIDER-ERROR-SECRET-abc123"
	body := `provider failure: {"error": "InvalidAuthenticationInfo ` + canary + `", "target": "group:show"}`
	red := RedactText(body, canary)
	if strings.Contains(red, canary) {
		t.Errorf("provider error body leaks canary: %q", red)
	}
	if !strings.Contains(red, "group:show") {
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
	return Credentials{Subscription: "s457-fixture-sub", Kind: AuthCLILogin}
}

// Deployment dry runs validate through --what-if; everything else refuses
// without execution.
func TestDryRun(t *testing.T) {
	// Unknown service fails.
	if err := (DryRun{Service: "bogus", Operation: "create", Credentials: testCreds()}).Check(); err == nil {
		t.Error("unknown service dry-run must fail")
	}
	// group/vm have no dry run: refuse.
	for _, svc := range []string{"group", "vm"} {
		if err := (DryRun{Service: svc, Operation: "create", Credentials: testCreds()}).Check(); err == nil {
			t.Errorf("%s dry-run must refuse", svc)
		} else if err.Code != CodeUnsupported {
			t.Errorf("%s code = %q, want unsupported-capability", svc, err.Code)
		}
	}
	// Missing credentials fail before preview.
	if err := (DryRun{Service: "deployment", Operation: "create"}).Check(); err == nil {
		t.Error("credential-less dry-run must fail")
	}
	// Happy path emits a loom-v2 preview envelope with --what-if, never
	// execution evidence.
	var buf bytes.Buffer
	d := DryRun{Service: "deployment", Operation: "create", Target: "deployment:group-create", Credentials: testCreds()}
	if code := d.Emit(&buf, "az deployment group create --what-if"); code != weavecli.ExitOK {
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
