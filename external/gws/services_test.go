package gws

import (
	"testing"
)

// Exactly the nine admitted services, no more.
func TestAdmittedServices(t *testing.T) {
	want := []string{"drive", "gmail", "calendar", "sheets", "docs", "chat", "slides", "tasks", "contacts"}
	if len(Services) != len(want) {
		t.Fatalf("len(Services) = %d, want %d", len(Services), len(want))
	}
	for i, name := range want {
		if Services[i].Name != name {
			t.Errorf("Services[%d] = %q, want %q", i, Services[i].Name, name)
		}
		if !Services[i].Supports(OpRead) || !Services[i].Supports(OpDryRun) {
			t.Errorf("service %q must support read and dry-run", name)
		}
	}
	// Only Drive admits uploads.
	for _, s := range Services {
		if s.Name == "drive" {
			if !s.Supports(OpUpload) {
				t.Error("drive must support upload")
			}
			continue
		}
		if s.Supports(OpUpload) {
			t.Errorf("service %q must not admit upload", s.Name)
		}
	}
}

func TestLookupService(t *testing.T) {
	if _, ok := LookupService("drive"); !ok {
		t.Error("LookupService(drive) failed")
	}
	if _, ok := LookupService("admin"); ok {
		t.Error("LookupService(admin) must fail — Admin Directory is unclaimed")
	}
	if _, ok := LookupService("bogus"); ok {
		t.Error("LookupService(bogus) must fail")
	}
}

// Deferred, not absent: watch/subscribe/renew and admin CRUD have named homes.
func TestDeferred(t *testing.T) {
	for _, key := range []string{"gmail:watch", "events:subscribe", "events:renew", "admin:directory", "admin:users"} {
		if Deferral(key) == "" {
			t.Errorf("Deferral(%q) is empty — record the home, not absence", key)
		}
	}
	if Deferral("drive:read") != "" {
		t.Error("Deferral(drive:read) must be empty — it is admitted")
	}
	// Watch/subscription deferrals name the event story; admin names its owner.
	for _, key := range []string{"gmail:watch", "events:subscribe", "events:renew"} {
		if d := Deferral(key); !contains(d, "5548854717bc") {
			t.Errorf("Deferral(%q) = %q, must name event story 5548854717bc", key, d)
		}
	}
	if d := Deferral("admin:directory"); !contains(d, "UNCLAIMED") {
		t.Errorf("Deferral(admin:directory) = %q, must stay UNCLAIMED", d)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
