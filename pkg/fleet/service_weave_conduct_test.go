package fleet

import (
	"reflect"
	"testing"
)

func TestWeaveConductServiceUnit(t *testing.T) {
	s, ok := New(serviceOpts(t.TempDir())...).Service("weave-conduct")
	if !ok {
		t.Fatal("weave-conduct service seed missing")
	}
	if s.Enabled || s.ProviderName() != ServiceProviderProcess {
		t.Fatalf("unexpected default service state: %+v", s)
	}
	s.Settings["repo"] = "/work/repo"
	argv, err := s.ExpandExec("")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"bashy", "weave", "conduct", "--repo", "/work/repo"}
	if !reflect.DeepEqual(argv, want) {
		t.Fatalf("exec = %q, want %q", argv, want)
	}
}
