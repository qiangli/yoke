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

// A store install is updated by the store, not from the folder the hub
// refreshes, so only an unpacked ("development") copy is hot-reloaded.
func TestPlanReloadSkipsStoreInstalls(t *testing.T) {
	h := newHub(0)
	h.reloadDir = t.TempDir()
	if _, ok := h.planReload(extHello{Version: "0.7.9", Methods: []string{"reload"}, InstallType: "normal"}); ok {
		t.Fatal("a store install must not be hot-reloaded")
	}
	if _, ok := h.planReload(extHello{Version: "0.7.9", Methods: []string{"reload"}, InstallType: "development"}); !ok {
		t.Fatal("a stale unpacked copy must still be hot-reloaded")
	}
}

// Two profiles on the same stale version are two copies: each is reloaded
// once (before, the second was taken for a copy loaded from another folder).
func TestPlanReloadOncePerInstance(t *testing.T) {
	h := newHub(0)
	h.reloadDir = t.TempDir()
	a := extHello{Version: "0.7.9", Methods: []string{"reload"}, InstallType: "development", Instance: "profile-a"}
	b := a
	b.Instance = "profile-b"
	if _, ok := h.planReload(a); !ok {
		t.Fatal("first profile not reloaded")
	}
	if _, ok := h.planReload(b); !ok {
		t.Fatal("second profile on the same version not reloaded")
	}
	if _, ok := h.planReload(a); ok {
		t.Fatal("a copy still stale after its reload must not loop")
	}
}
