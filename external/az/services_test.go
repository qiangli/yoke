package az

import (
	"strings"
	"testing"
)

func TestAdmittedServices(t *testing.T) {
	if len(Services) != 3 {
		t.Fatalf("services = %d, want 3 (group, vm, deployment)", len(Services))
	}
	// Deployment: provider-validated what-if preview.
	dep, ok := LookupService("deployment")
	if !ok {
		t.Fatal("deployment not admitted")
	}
	if dep.DryRunPreview != PreviewProviderValidated {
		t.Errorf("deployment preview = %q, want provider-validated-preview", dep.DryRunPreview)
	}
	if dep.DryRunFlag != "--what-if" {
		t.Errorf("deployment flag = %q, want --what-if", dep.DryRunFlag)
	}
	if !dep.Supports(OpRead) || !dep.Supports(OpWrite) || !dep.Supports(OpDryRun) {
		t.Error("deployment must support read+write+dry-run")
	}
	// group/vm: no dry run.
	for _, name := range []string{"group", "vm"} {
		svc, ok := LookupService(name)
		if !ok {
			t.Fatalf("%s not admitted", name)
		}
		if svc.Supports(OpDryRun) {
			t.Errorf("%s must not support dry-run here", name)
		}
		if svc.DryRunPreview != PreviewUnsupported {
			t.Errorf("%s preview = %q, want unsupported", name, svc.DryRunPreview)
		}
	}
	if _, ok := LookupService("bogus"); ok {
		t.Error("unknown service looked up")
	}
}

func TestUnclaimed(t *testing.T) {
	for _, key := range []string{"subscription:create", "role:assign"} {
		if Deferral(key) == "" {
			t.Errorf("unclaimed %q missing", key)
		}
	}
	if Deferral("group:create") != "" {
		t.Error("admitted capability must not be unclaimed")
	}
	if got := Deferral("role:assign"); !strings.HasPrefix(got, "UNCLAIMED") {
		t.Errorf("role:assign %q must stay UNCLAIMED", got)
	}
}
