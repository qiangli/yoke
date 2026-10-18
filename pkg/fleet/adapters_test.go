package fleet

import (
	"bytes"
	"strings"
	"testing"
)

func TestAdapterDeclarationAndKindCore(t *testing.T) {
	isolatedFleetRoot(t)
	body := []byte("kind: adapter\nname: sample\nmatch: [sample]\ndry-run: [--dry_run]\npreview-level: local-preview\njson: []\neffects: [fs.read]\n")
	a, e := ParseAdapter("", body, nil)
	if e != nil || a.DryRun[0] != "--dry_run" || a.JSON == nil {
		t.Fatalf("%+v %v", a, e)
	}
	c := New(WithoutCloudOverlay())
	if e = c.SaveAdapter(a); e != nil {
		t.Fatal(e)
	}
	if got, ok := c.Adapter("sample"); !ok || got.JSON == nil {
		t.Fatal(got, ok)
	}
	if _, ok := kindByName(KindAdapter); !ok {
		t.Fatal("kind missing")
	}
	cmd := NewAdaptersCmd(WithoutCloudOverlay())
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", "sample", "--json"})
	if e = cmd.Execute(); e != nil || !strings.Contains(out.String(), "sample") {
		t.Fatal(out.String(), e)
	}
	for _, bad := range []string{
		"kind: adapter\nname: x\nmatch: [x]\npreview_level: unsupported\nno_input: [-y]\n",
		"kind: adapter\nname: x\nmatch: [x]\npreview_level: unsupported\ndry_run: [-n]\n",
		"kind: adapter\nname: x\nmatch: [x]\npreview_level: unsupported\nunknown: x\n",
		"kind: adapter\nname: x\nmatch: [x]\npreview_level: unsupported\ndry_run: []\ndry-run: []\n",
	} {
		if _, e := ParseAdapter("", []byte(bad), nil); e == nil {
			t.Fatal("accepted invalid record")
		}
	}
}
func TestAdapterCuratedRingAndOverride(t *testing.T) {
	isolatedFleetRoot(t)
	c := New(WithoutCloudOverlay())
	a, ok := c.Adapter("git-clean")
	if !ok {
		t.Fatal("curated row missing")
	}
	a.DryRun = nil
	a.PreviewLevel = "unsupported"
	if e := c.SaveAdapter(a); e != nil {
		t.Fatal(e)
	}
	got, ok := c.Adapter("git-clean")
	if !ok || got.DryRun != nil {
		t.Fatal("local ring did not override embedded mapping")
	}
	rows, errs := c.Adapters()
	if len(errs) > 0 {
		t.Fatal(errs)
	}
	seen := map[string]bool{}
	for _, a := range rows {
		seen[a.Match[0]] = true
	}
	for _, name := range []string{"git", "tar", "unzip", "docker", "kubectl", "rsync", "make", "apt"} {
		if !seen[name] {
			t.Fatal(name)
		}
	}
}
func TestAdapterCRUD(t *testing.T) {
	isolatedFleetRoot(t)
	run := func(args ...string) string {
		t.Helper()
		c := NewAdaptersCmd(WithoutCloudOverlay())
		var b bytes.Buffer
		c.SetOut(&b)
		c.SetErr(&b)
		c.SetArgs(args)
		if e := c.Execute(); e != nil {
			t.Fatalf("%v: %v %s", args, e, b.String())
		}
		return b.String()
	}
	run("add", "mine", "--set", "kind=adapter", "--set", "match.0=mine", "--set", "preview_level=unsupported")
	run("set", "mine", "--set", "summary=changed")
	if got := run("show", "mine", "--json"); !strings.Contains(got, "changed") {
		t.Fatal(got)
	}
	run("schema")
	run("rm", "mine")
	if _, ok := New(WithoutCloudOverlay()).Adapter("mine"); ok {
		t.Fatal("rm retained local row")
	}
}
