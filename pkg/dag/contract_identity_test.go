package dag

import (
	"context"
	"strings"
	"testing"
)

// A target's receipt carries the identity and capability of the exact target
// that ran, so a receipt names its unit the way a skill receipt does.
func TestAttestStampsIdentity(t *testing.T) {
	doc, err := Parse(strings.NewReader("## build\nEffects: write\n```sh\ntrue\n```\n"), "dag.md")
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	task, ok := doc.Lookup("build")
	if !ok {
		t.Fatal("no build target")
	}
	if !strings.HasPrefix(task.Identity, "h") || !strings.HasPrefix(task.Capability, "k") {
		t.Fatalf("parse did not stamp identity/capability: %q %q", task.Identity, task.Capability)
	}
	a := attest(context.Background(), task, t.TempDir(), nil, true)
	if a == nil {
		t.Fatal("attest returned nil for a target with Effects")
	}
	if a.Identity != task.Identity || a.Capability != task.Capability {
		t.Fatalf("receipt identity = %q/%q, want %q/%q", a.Identity, a.Capability, task.Identity, task.Capability)
	}
}
