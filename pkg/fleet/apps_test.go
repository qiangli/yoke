package fleet

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runApps(t *testing.T, root string, validate AppValidator, args ...string) (string, error) {
	t.Helper()
	cmds := NewAppCmds(WithRoot(root), WithAppValidate(validate))
	var out bytes.Buffer
	for _, c := range cmds {
		if c.Name() != args[0] {
			continue
		}
		c.SetArgs(args[1:])
		c.SetOut(&out)
		c.SetErr(&out)
		err := c.Execute()
		return out.String(), err
	}
	t.Fatalf("no app verb %q", args[0])
	return "", nil
}

// add → show → set → rm round-trips a record through the local store, and
// the file on disk is the canonical YAML record.
func TestAppsCRUDRoundTrip(t *testing.T) {
	root := t.TempDir()
	if _, err := runApps(t, root, nil, "add", "jup", "--port", "8888", "--label", "Jupyter", "--start", "jupyter", "--start", "lab"); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(root, "apps", "jup.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"name: jup", "kind: app", "port: 8888", "label: Jupyter"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("record missing %q:\n%s", want, body)
		}
	}
	if _, err := runApps(t, root, nil, "set", "jup", "--port", "9999", "--set", "tip=notebooks"); err != nil {
		t.Fatal(err)
	}
	a, ok := New(WithRoot(root), WithoutCloudOverlay()).App("jup")
	if !ok || a.Port != 9999 || a.Tip != "notebooks" || a.Label != "Jupyter" || len(a.Start) != 2 {
		t.Fatalf("after set: %+v ok=%v", a, ok)
	}
	out, err := runApps(t, root, nil, "show", "jup", "--field", "port")
	if err != nil || strings.TrimSpace(out) != "9999" {
		t.Fatalf("show --field port = %q, %v", out, err)
	}
	if _, err := runApps(t, root, nil, "rm", "jup"); err != nil {
		t.Fatal(err)
	}
	if _, ok := New(WithRoot(root), WithoutCloudOverlay()).App("jup"); ok {
		t.Fatal("rm left the record")
	}
}

// The store refuses what it can see on its own (no port, a path-like name)
// and whatever the console's injected validator refuses.
func TestAppsAddRefusals(t *testing.T) {
	root := t.TempDir()
	if _, err := runApps(t, root, nil, "add", "noport"); err == nil || !strings.Contains(err.Error(), "port") {
		t.Errorf("no port: err = %v", err)
	}
	if _, err := runApps(t, root, nil, "add", "a b", "--port", "1"); err == nil {
		t.Error("whitespace name accepted")
	}
	reject := func(a App) error {
		if a.Name == "term" {
			return os.ErrExist
		}
		return nil
	}
	if _, err := runApps(t, root, reject, "add", "term", "--port", "8080"); err == nil {
		t.Error("validator refusal ignored")
	}
	if _, err := os.Stat(filepath.Join(root, "apps", "term.yaml")); err == nil {
		t.Error("a refused record was written")
	}
}

// Registered apps have no embedded ring: a fresh catalog lists nothing.
func TestAppsNoEmbeddedRing(t *testing.T) {
	apps, errs := New(WithRoot(t.TempDir()), WithoutCloudOverlay()).Apps()
	if len(apps) != 0 || len(errs) != 0 {
		t.Fatalf("fresh ring = %v, %v", apps, errs)
	}
}
