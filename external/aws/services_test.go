package aws

import (
	"strings"
	"testing"
)

func TestAdmittedServices(t *testing.T) {
	if len(Services) != 3 {
		t.Fatalf("services = %d, want 3 (ec2, s3, iam)", len(Services))
	}
	// EC2/S3: provider-validated dry run with their provider flag.
	for _, tc := range []struct{ service, flag string }{
		{"ec2", "--dry-run"},
		{"s3", "--dryrun"},
	} {
		svc, ok := LookupService(tc.service)
		if !ok {
			t.Fatalf("%s not admitted", tc.service)
		}
		if svc.DryRunPreview != PreviewProviderValidated {
			t.Errorf("%s preview = %q, want provider-validated-preview", tc.service, svc.DryRunPreview)
		}
		if svc.DryRunFlag != tc.flag {
			t.Errorf("%s flag = %q, want %q", tc.service, svc.DryRunFlag, tc.flag)
		}
		if !svc.Supports(OpRead) || !svc.Supports(OpWrite) || !svc.Supports(OpDryRun) {
			t.Errorf("%s must support read+write+dry-run", tc.service)
		}
	}
	// IAM is read-only with no dry run.
	iam, ok := LookupService("iam")
	if !ok {
		t.Fatal("iam not admitted")
	}
	if iam.Supports(OpWrite) || iam.Supports(OpDryRun) {
		t.Error("iam must be read-only here")
	}
	if iam.DryRunPreview != PreviewUnsupported {
		t.Errorf("iam preview = %q, want unsupported", iam.DryRunPreview)
	}
	if _, ok := LookupService("bogus"); ok {
		t.Error("unknown service looked up")
	}
}

func TestUnclaimed(t *testing.T) {
	for _, key := range []string{"iam:write", "account:set"} {
		if Deferral(key) == "" {
			t.Errorf("unclaimed %q missing", key)
		}
	}
	if Deferral("ec2:run-instances") != "" {
		t.Error("admitted capability must not be unclaimed")
	}
	if got := Deferral("iam:write"); !strings.HasPrefix(got, "UNCLAIMED") {
		t.Errorf("iam:write %q must stay UNCLAIMED", got)
	}
}
