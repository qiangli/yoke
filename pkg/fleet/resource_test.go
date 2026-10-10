package fleet

import (
	"strings"
	"testing"
)

func TestResourceRoundTrip(t *testing.T) {
	isolatedFleetRoot(t)
	cat := New()
	rec := Resource{
		Name: "stagedir", Kind: "path", Members: []string{"/w/stage"},
		Title: "Stage", Notes: "scratch", Aliases: []string{"stage"},
		TTL: "2h", Mode: "lease", Guard: []string{"weave add"},
	}
	if err := cat.SaveResource(rec); err != nil {
		t.Fatal(err)
	}
	got, ok := cat.Resource("stagedir")
	if !ok {
		t.Fatal("saved resource does not resolve")
	}
	if got.Kind != "path" || len(got.Members) != 1 || got.Members[0] != "/w/stage" {
		t.Fatalf("round trip = %+v", got)
	}
	if got.Title != "Stage" || got.TTL != "2h" || got.Mode != "lease" || len(got.Guard) != 1 {
		t.Fatalf("round trip dropped fields: %+v", got)
	}
	// Aliases resolve to the canonical holder.
	aliased, ok := cat.Resource("stage")
	if !ok || aliased.Name != "stagedir" {
		t.Fatalf("alias resolves to %+v ok=%v", aliased, ok)
	}
	all, errs := cat.Resources()
	if len(errs) != 0 || len(all) != 1 {
		t.Fatalf("list = %v %v", all, errs)
	}
	if err := cat.RemoveResource("stagedir"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.Resource("stagedir"); ok {
		t.Fatal("removed resource still resolves")
	}
}

func TestResourceValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  Resource
		want string
	}{
		{"missing kind", Resource{Name: "r"}, "kind is required"},
		{"bad ttl", Resource{Name: "r", Kind: "path", TTL: "forever"}, "not a Go duration"},
		{"bad mode", Resource{Name: "r", Kind: "path", Mode: "exclusive"}, "not one of"},
		{"bad name", Resource{Name: "has space", Kind: "path"}, "no whitespace"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedFleetRoot(t)
			if err := New().SaveResource(tc.rec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestResourceKindRoundTrip(t *testing.T) {
	isolatedFleetRoot(t)
	cat := New()
	rec := ResourceKind{
		Name: "gpunode", Match: "member", Domain: "gpu", TTL: "1h",
		Modes: []string{"lease"}, Resolve: "gpu-members", Probe: "gpu-probe",
	}
	if err := cat.SaveResourceKind(rec); err != nil {
		t.Fatal(err)
	}
	got, ok := cat.ResourceKind("gpunode")
	if !ok {
		t.Fatal("saved resourcekind does not resolve")
	}
	if got.Match != "member" || got.Domain != "gpu" || got.TTL != "1h" {
		t.Fatalf("round trip = %+v", got)
	}
	if len(got.Modes) != 1 || got.Resolve != "gpu-members" || got.Probe != "gpu-probe" {
		t.Fatalf("round trip dropped fields: %+v", got)
	}
	all, errs := cat.ResourceKinds()
	if len(errs) != 0 || len(all) != 1 {
		t.Fatalf("list = %v %v", all, errs)
	}
	if err := cat.RemoveResourceKind("gpunode"); err != nil {
		t.Fatal(err)
	}
	if _, ok := cat.ResourceKind("gpunode"); ok {
		t.Fatal("removed resourcekind still resolves")
	}
}

func TestResourceKindValidation(t *testing.T) {
	for _, tc := range []struct {
		name string
		rec  ResourceKind
		want string
	}{
		{"bad match", ResourceKind{Name: "k", Match: "fuzzy"}, "not one of"},
		{"bad ttl", ResourceKind{Name: "k", Match: "member", TTL: "soon"}, "not a Go duration"},
		{"bad mode", ResourceKind{Name: "k", Match: "member", Modes: []string{"forever"}}, "not one of"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isolatedFleetRoot(t)
			if err := New().SaveResourceKind(tc.rec); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// Both nouns ride the kind table, so name resolution, verify enumeration
// and the schema walk pick them up with no further wiring.
func TestResourceNounsAreInTheKindTable(t *testing.T) {
	isolatedFleetRoot(t)
	seen := map[string]bool{}
	for _, n := range RegistryNouns() {
		seen[n.Name] = true
	}
	for _, want := range []string{KindResource, KindResourceKind} {
		if !seen[want] {
			t.Fatalf("noun %q missing from the kind table", want)
		}
	}
	if canon, ok := New().Resource("nothing-here"); ok {
		t.Fatalf("unknown resource resolves: %+v", canon)
	}
	var paths []string
	for _, f := range schemaFields(KindResource) {
		paths = append(paths, f.Path)
	}
	for _, want := range []string{"name", "kind", "members", "title", "notes", "aliases", "ttl", "mode", "guard"} {
		if !strings.Contains(strings.Join(paths, " "), want) {
			t.Errorf("resource schema lacks %q: %v", want, paths)
		}
	}
	var kpaths []string
	for _, f := range schemaFields(KindResourceKind) {
		kpaths = append(kpaths, f.Path)
	}
	for _, want := range []string{"name", "match", "domain", "ttl", "modes", "resolve", "probe"} {
		if !strings.Contains(strings.Join(kpaths, " "), want) {
			t.Errorf("resourcekind schema lacks %q: %v", want, kpaths)
		}
	}
}
