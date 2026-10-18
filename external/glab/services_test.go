package glab

import (
	"strings"
	"testing"
)

func TestAdmittedServices(t *testing.T) {
	if len(Services) != 3 {
		t.Fatalf("services = %d, want 3 (issues, merge-requests, pipelines)", len(Services))
	}
	for _, s := range Services {
		if s.DryRunPreview != PreviewUnsupported {
			t.Errorf("service %q dry-run preview = %q, want unsupported (glab has no dry-run)", s.Name, s.DryRunPreview)
		}
		if !s.Supports(OpRead) {
			t.Errorf("service %q must support read", s.Name)
		}
	}
	// pipelines is read-only.
	svc, ok := LookupService("pipelines")
	if !ok {
		t.Fatal("pipelines not admitted")
	}
	if svc.Supports(OpWrite) {
		t.Error("pipelines must not support write here (trigger/cancel is event-story)")
	}
	if _, ok := LookupService("bogus"); ok {
		t.Error("unknown service looked up")
	}
}

func TestDeferred(t *testing.T) {
	for _, key := range []string{"pipelines:trigger", "pipelines:cancel", "admin:instance"} {
		if Deferral(key) == "" {
			t.Errorf("deferral %q missing", key)
		}
	}
	if Deferral("issues:create") != "" {
		t.Error("admitted capability must not be deferred")
	}
	if got := Deferral("admin:instance"); !strings.HasPrefix(got, "UNCLAIMED") {
		t.Errorf("admin deferral %q must stay UNCLAIMED", got)
	}
}
