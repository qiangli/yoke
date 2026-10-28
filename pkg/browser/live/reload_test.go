package live

import (
	"os"
	"path/filepath"
	"testing"
)

func writeManifest(t *testing.T, dir, version string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), []byte(`{"version":"`+version+`"}`), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestRefreshIfStale(t *testing.T) {
	old := t.TempDir()
	writeManifest(t, old, "0.7.2")
	wrote, err := RefreshIfStale(old)
	if err != nil || !wrote {
		t.Fatalf("an older unpacked copy must be refreshed: wrote=%v err=%v", wrote, err)
	}
	b, _ := os.ReadFile(filepath.Join(old, "manifest.json"))
	if manifestVersion(b) != EmbeddedVersion() {
		t.Fatalf("folder still holds v%s", manifestVersion(b))
	}
	newer := t.TempDir()
	writeManifest(t, newer, "99.0.0")
	if wrote, _ := RefreshIfStale(newer); wrote {
		t.Fatal("a newer copy must not be downgraded")
	}
	if wrote, _ := RefreshIfStale(filepath.Join(t.TempDir(), "missing")); wrote {
		t.Fatal("the hub must not create a folder the user never loaded")
	}
}

func TestPlanReloadOncePerVersion(t *testing.T) {
	h := newHub(0)
	h.reloadDir = t.TempDir()
	stale := extHello{Version: "0.7.9", Methods: []string{"reload"}}
	if _, ok := h.planReload(stale); !ok {
		t.Fatal("a stale extension that can reload must be reloaded")
	}
	if _, ok := h.planReload(stale); ok {
		t.Fatal("a second stale hello at the same version must not loop")
	}
	if _, ok := h.planReload(extHello{Version: "0.7.1", Methods: []string{"click"}}); ok {
		t.Fatal("an extension without the reload method cannot be asked to reload")
	}
	if _, ok := h.planReload(extHello{Version: EmbeddedVersion(), Methods: []string{"reload"}}); ok {
		t.Fatal("a current extension must not be reloaded")
	}
}
