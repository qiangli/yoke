package fleet

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// acpSeedTools is the S10 set: every one must reach the ACP rung through a
// pinned, verifiable release.
var acpSeedTools = []string{"claude", "codex", "gemini"}

func seedCatalog(t *testing.T) *Catalog {
	t.Helper()
	return New(WithBaselineFS(baselineFS), WithoutLocalStore(), WithoutCloudOverlay())
}

func TestACPAdapterSeedsArePinned(t *testing.T) {
	cat := seedCatalog(t)
	for _, name := range acpSeedTools {
		tool, ok := cat.Tool(name)
		if !ok {
			t.Fatalf("seed tool %q missing", name)
		}
		a := tool.CLI.Launch.ACPAdapter
		if a == nil {
			t.Errorf("%s: no acp_adapter pin", name)
			continue
		}
		if err := tool.ValidateACPAdapter(); err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if a.Tarball.Integrity == "" {
			t.Errorf("%s: no registry integrity to cross-check the sha256 against", name)
		}
		if len(a.Requires) == 0 || a.Requires[0] != "node" {
			t.Errorf("%s: an npm adapter must require node, got %v", name, a.Requires)
		}
	}
	// gemini is the native case: the adapter IS the tool's own binary.
	if g, _ := cat.Tool("gemini"); g.CLI.Launch.ACPAdapter.Bin != g.Binary() {
		t.Errorf("gemini adapter bin %q is not the tool binary %q", g.CLI.Launch.ACPAdapter.Bin, g.Binary())
	}
}

func TestACPAdapterValidateRefusesNonPins(t *testing.T) {
	good := func() Tool {
		return Tool{Name: "x", CLI: ToolCLI{Launch: ToolLaunch{
			ACPExec: "x-acp --stdio",
			ACPAdapter: &ToolACPAdapter{
				ID: "x-acp", Source: "npm", Package: "@v/x-acp", Version: "1.2.3",
				License: "MIT", LicenseURL: "https://example.com/LICENSE",
				Bin: "x-acp", Args: []string{"--stdio"},
				Tarball: ToolACPAdapterTarball{
					URL:    "https://registry.npmjs.org/@v/x-acp/-/x-acp-1.2.3.tgz",
					SHA256: strings.Repeat("a", 64),
				},
			},
		}}}
	}
	if err := good().ValidateACPAdapter(); err != nil {
		t.Fatalf("valid pin refused: %v", err)
	}
	cases := map[string]func(*Tool){
		"dist-tag version":    func(t *Tool) { t.CLI.Launch.ACPAdapter.Version = "latest" },
		"versioned package":   func(t *Tool) { t.CLI.Launch.ACPAdapter.Package = "@v/x-acp@1.2.3" },
		"bad id":              func(t *Tool) { t.CLI.Launch.ACPAdapter.ID = "X_acp" },
		"unknown source":      func(t *Tool) { t.CLI.Launch.ACPAdapter.Source = "curl" },
		"short digest":        func(t *Tool) { t.CLI.Launch.ACPAdapter.Tarball.SHA256 = "abc" },
		"url not the pin":     func(t *Tool) { t.CLI.Launch.ACPAdapter.Tarball.URL = "https://e/x-9.9.9.tgz" },
		"bad sri":             func(t *Tool) { t.CLI.Launch.ACPAdapter.Tarball.Integrity = "sha1-abc" },
		"no license":          func(t *Tool) { t.CLI.Launch.ACPAdapter.License = "" },
		"acp_exec drifted":    func(t *Tool) { t.CLI.Launch.ACPExec = "other-acp --stdio" },
		"acp_exec lost args":  func(t *Tool) { t.CLI.Launch.ACPExec = "x-acp" },
		"bin with whitespace": func(t *Tool) { t.CLI.Launch.ACPAdapter.Bin = "x acp" },
	}
	for name, mutate := range cases {
		tool := good()
		mutate(&tool)
		if err := tool.ValidateACPAdapter(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// TestACPRegistryManifestGolden pins the ACP-registry manifest bashy publishes
// (docs/acp-registry.json) to the seeds: the published file, the golden and
// the launch declarations cannot drift apart. Regenerate with
//
//	go test ./pkg/fleet -run TestACPRegistryManifestGolden -update-golden
func TestACPRegistryManifestGolden(t *testing.T) {
	tools, _ := seedCatalog(t).Tools(false)
	got, err := ACPRegistryManifest(tools)
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		filepath.Join("testdata", "golden", "acp-registry.json"),
		filepath.Join("..", "..", "docs", "acp-registry.json"),
	}
	if *updateGolden {
		for _, p := range paths {
			if err := os.WriteFile(p, got, 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, p := range paths {
		want, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("missing %s (run with -update-golden): %v", p, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("%s drifted from the seeds. If intended, rerun with -update-golden.\n--- want\n%s\n--- got\n%s", p, want, got)
		}
	}

	// The registry's agent.schema.json constraints, checked on the bytes.
	var idx struct {
		Version string           `json:"version"`
		Agents  []map[string]any `json:"agents"`
	}
	if err := json.Unmarshal(got, &idx); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, a := range idx.Agents {
		id, _ := a["id"].(string)
		ids[id] = true
		for _, k := range []string{"id", "name", "version", "description", "license_url"} {
			if s, _ := a[k].(string); s == "" {
				t.Errorf("%s: required %q empty", id, k)
			}
		}
		if !semverPin.MatchString(a["version"].(string)) || !acpRegistryID.MatchString(id) {
			t.Errorf("%s: id/version violate the registry schema", id)
		}
		npx, _ := a["distribution"].(map[string]any)["npx"].(map[string]any)
		if pkg, _ := npx["package"].(string); !strings.HasSuffix(pkg, "@"+a["version"].(string)) {
			t.Errorf("%s: npx package %q does not pin version %v", id, pkg, a["version"])
		}
	}
	for _, want := range []string{"claude-acp", "codex-acp", "gemini"} {
		if !ids[want] {
			t.Errorf("manifest lacks %s", want)
		}
	}
}
