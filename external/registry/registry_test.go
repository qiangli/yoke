package registry

import "testing"

func TestDoctlEntry(t *testing.T) {
	e, ok := Lookup("doctl")
	if !ok {
		t.Fatal("doctl not registered")
	}
	if e.Tier != 6 {
		t.Errorf("doctl tier = %d, want 6 (cloud)", e.Tier)
	}
	if e.License != "Apache-2.0" {
		t.Errorf("doctl license = %q, want Apache-2.0", e.License)
	}
	if e.Synopsis == "" {
		t.Error("doctl has no synopsis")
	}
	if e.Resolve == nil {
		t.Error("doctl has no Resolve")
	}
	if cmd := e.NewCmd(); cmd.Name() != "doctl" || !cmd.DisableFlagParsing {
		t.Errorf("doctl NewCmd wrong: name=%q disableFlags=%v", cmd.Name(), cmd.DisableFlagParsing)
	}
}

func TestNamesAndAll(t *testing.T) {
	names := Names()
	if len(names) == 0 {
		t.Fatal("registry is empty")
	}
	// Names is sorted and matches All().
	all := All()
	if len(all) != len(names) {
		t.Fatalf("All()=%d, Names()=%d", len(all), len(names))
	}
	for i, e := range all {
		if e.Name != names[i] {
			t.Errorf("All()[%d]=%q, Names()[%d]=%q", i, e.Name, i, names[i])
		}
		if e.Synopsis == "" {
			t.Errorf("entry %q has no synopsis", e.Name)
		}
	}
}
