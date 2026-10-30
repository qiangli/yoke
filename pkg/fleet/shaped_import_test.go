// Copyright (c) 2026 qiangli
// See LICENSE for licensing information

package fleet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestShapedAppServerManifest: `app add --type shaped` accepts an app that
// declares a server in shaped.json (no port: the console picks one per
// revision) and refuses, before anything is installed, a manifest the
// console could not serve.
func TestShapedAppServerManifest(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	cat := New(WithRoot(filepath.Join(home, "registry")), WithoutCloudOverlay())
	source := func(manifest string) string {
		t.Helper()
		dir := t.TempDir()
		files := map[string]string{
			"go.mod":  "module app\n\ngo 1.22\n",
			"main.go": "package main\n\nfunc main() {}\n",
		}
		if manifest != "" {
			files["shaped.json"] = manifest
		}
		for rel, body := range files {
			if err := os.WriteFile(filepath.Join(dir, rel), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		return dir
	}

	good := `{"server": {"main": ".", "args": ["serve", "-addr", "$ADDR", "-data", "$DATA"]}}`
	if err := cat.SaveApp(App{Name: "desk", Type: AppTypeShaped, Source: source(good)}); err != nil {
		t.Fatalf("app with a server manifest refused: %v", err)
	}
	a, ok := cat.App("desk")
	if !ok || a.Port != 0 || !a.IsShaped() {
		t.Fatalf("registered = %+v, %v; want a shaped app with no port", a, ok)
	}
	if b, err := os.ReadFile(filepath.Join(a.Source, "shaped.json")); err != nil || string(b) != good {
		t.Fatalf("managed checkout manifest = %q, %v", b, err)
	}
	if err := cat.SaveApp(App{Name: "desk2", Type: AppTypeShaped, Port: 18095, Source: source(good)}); err == nil {
		t.Fatal("a shaped app with a fixed port was accepted")
	}

	for name, bad := range map[string]string{
		"noaddr":  `{"server": {"args": ["serve"]}}`,
		"escape":  `{"server": {"main": "../kit", "args": ["$ADDR"]}}`,
		"unknown": `{"server": {"build": "make", "args": ["$ADDR"]}}`,
		"garbage": `{"server": `,
	} {
		err := cat.SaveApp(App{Name: name, Type: AppTypeShaped, Source: source(bad)})
		if err == nil || !strings.Contains(err.Error(), "shaped.json") {
			t.Errorf("%s manifest: err = %v, want a shaped.json refusal", name, err)
		}
		if _, statErr := os.Stat(filepath.Join(home, "src", name)); !os.IsNotExist(statErr) {
			t.Errorf("%s: refused import left a managed checkout behind", name)
		}
	}
}

// TestShapedAppServerCapturesOutOfTreeReplace: a server whose go.mod
// replaces a sibling directory (the desk tools' `replace desk-kit =>
// ../kit`) is imported with that module vendored into the managed
// checkout, so a revision builds from its own tree alone.
func TestShapedAppServerCapturesOutOfTreeReplace(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("kit/go.mod", "module desk-kit\n\ngo 1.22\n")
	write("kit/kit.go", "package kit\n\nconst Name = \"kit\"\n")
	write("tool/go.mod", "module deskapp\n\ngo 1.22\n\nreplace desk-kit => ../kit\n\nrequire desk-kit v0.0.0-00010101000000-000000000000\n")
	write("tool/main.go", "package main\n\nimport kit \"desk-kit\"\n\nfunc main() { println(kit.Name) }\n")
	write("tool/shaped.json", `{"server": {"args": ["-addr", "$ADDR"]}}`)

	cat := New(WithRoot(filepath.Join(home, "registry")), WithoutCloudOverlay())
	if err := cat.SaveApp(App{Name: "tool", Type: AppTypeShaped, Source: filepath.Join(root, "tool")}); err != nil {
		t.Fatalf("import: %v", err)
	}
	a, _ := cat.App("tool")
	if _, err := os.Stat(filepath.Join(a.Source, "vendor", "desk-kit", "kit.go")); err != nil {
		t.Fatalf("out-of-tree replace not captured: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "tool", "vendor")); !os.IsNotExist(err) {
		t.Fatal("import wrote vendor/ into the operator's source")
	}
	build := exec.Command("go", "build", "-o", filepath.Join(t.TempDir(), "bin"), ".")
	build.Dir = a.Source
	build.Env = append(os.Environ(), "GOWORK=off", "CGO_ENABLED=0")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("managed checkout does not build on its own: %v\n%s", err, out)
	}
}
