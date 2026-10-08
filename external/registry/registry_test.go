package registry

import (
	"context"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

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
	if e.DefaultVersion != DoctlVersion {
		t.Errorf("doctl DefaultVersion = %q, want %q", e.DefaultVersion, DoctlVersion)
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

func TestTofuEntry(t *testing.T) {
	e, ok := Lookup("tofu")
	if !ok {
		t.Fatal("tofu not registered")
	}
	if e.Tier != 6 {
		t.Errorf("tofu tier = %d, want 6 (cloud)", e.Tier)
	}
	if e.License != "MPL-2.0" {
		t.Errorf("tofu license = %q, want MPL-2.0", e.License)
	}
	if e.DefaultVersion != TofuVersion {
		t.Errorf("tofu DefaultVersion = %q, want %q", e.DefaultVersion, TofuVersion)
	}
	if e.Synopsis == "" {
		t.Error("tofu has no synopsis")
	}
	if e.Resolve == nil {
		t.Error("tofu has no Resolve")
	}
	if cmd := e.NewCmd(); cmd.Name() != "tofu" || !cmd.DisableFlagParsing {
		t.Errorf("tofu NewCmd wrong: name=%q disableFlags=%v", cmd.Name(), cmd.DisableFlagParsing)
	}
}

func TestGiteaEntry(t *testing.T) {
	e, ok := Lookup("gitea")
	if !ok {
		t.Fatal("gitea not registered")
	}
	if e.Tier != 5 {
		t.Errorf("gitea tier = %d, want 5 (cluster)", e.Tier)
	}
	if e.License != "MIT" {
		t.Errorf("gitea license = %q, want MIT", e.License)
	}
	if e.DefaultVersion != GiteaVersion {
		t.Errorf("gitea DefaultVersion = %q, want %q", e.DefaultVersion, GiteaVersion)
	}
	if e.Synopsis == "" {
		t.Error("gitea has no synopsis")
	}
	if e.Resolve == nil {
		t.Error("gitea has no Resolve")
	}
	if cmd := e.NewCmd(); cmd.Name() != "gitea" || !cmd.DisableFlagParsing {
		t.Errorf("gitea NewCmd wrong: name=%q disableFlags=%v", cmd.Name(), cmd.DisableFlagParsing)
	}
}

func TestOllamaEntry(t *testing.T) {
	e, ok := Lookup("ollama")
	if !ok {
		t.Fatal("ollama not registered")
	}
	if e.Tier != 3 {
		t.Errorf("ollama tier = %d, want 3 (sandbox)", e.Tier)
	}
	if e.License != "MIT" {
		t.Errorf("ollama license = %q, want MIT", e.License)
	}
	if e.DefaultVersion != OllamaVersion {
		t.Errorf("ollama DefaultVersion = %q, want %q", e.DefaultVersion, OllamaVersion)
	}
	if e.Synopsis == "" {
		t.Error("ollama has no synopsis")
	}
	if e.Resolve == nil {
		t.Error("ollama has no Resolve")
	}
	if cmd := e.NewCmd(); cmd.Name() != "ollama" || !cmd.DisableFlagParsing {
		t.Errorf("ollama NewCmd wrong: name=%q disableFlags=%v", cmd.Name(), cmd.DisableFlagParsing)
	}
}

func TestTofuAssetMatch(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		goarch string
		want   bool
	}{
		{"tofu_1.13.1_windows_amd64.zip", "windows", "amd64", true},
		{"tofu_1.13.1_windows_arm64.zip", "windows", "arm64", true},
		{"tofu_1.13.1_darwin_amd64.tar.gz", "darwin", "amd64", true},
		{"tofu_1.13.1_darwin_arm64.tar.gz", "darwin", "arm64", true},
		{"tofu_1.13.1_linux_amd64.tar.gz", "linux", "amd64", true},
		{"tofu_1.13.1_linux_arm64.tar.gz", "linux", "arm64", true},
		{"tofu_1.13.1_darwin_amd64.zip", "darwin", "amd64", false},
		{"tofu_1.13.1_windows_amd64.tar.gz", "windows", "amd64", false},
	}
	for _, tt := range tests {
		if got := tofuAssetMatch(tt.name, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("tofuAssetMatch(%q, %q, %q) = %v, want %v", tt.name, tt.goos, tt.goarch, got, tt.want)
		}
	}
}

func TestOllamaAssetMatch(t *testing.T) {
	tests := []struct {
		name   string
		goos   string
		goarch string
		want   bool
	}{
		{"ollama-darwin.tgz", "darwin", "amd64", true},
		{"ollama-darwin.tgz", "darwin", "arm64", true},
		{"ollama-windows-amd64.zip", "windows", "amd64", true},
		{"ollama-windows-arm64.zip", "windows", "arm64", true},
		{"ollama-linux-amd64.tar.zst", "linux", "amd64", true},
		{"ollama-linux-arm64.tar.zst", "linux", "arm64", true},
		{"ollama-windows-amd64-rocm.zip", "windows", "amd64", false},
	}
	for _, tt := range tests {
		if got := ollamaAssetMatch(tt.name, tt.goos, tt.goarch); got != tt.want {
			t.Errorf("ollamaAssetMatch(%q, %q, %q) = %v, want %v", tt.name, tt.goos, tt.goarch, got, tt.want)
		}
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

func TestNoDefaultRegistryEntryLacksPin(t *testing.T) {
	supportedPlatforms := []string{
		"darwin/amd64",
		"darwin/arm64",
		"linux/amd64",
		"linux/arm64",
		"windows/amd64",
		"windows/arm64",
	}

	requiredTools := []string{
		"doctl",
		"rg",
		"tofu",
		"gitea",
		"ollama",
	}

	for _, name := range requiredTools {
		e, ok := Lookup(name)
		if !ok {
			t.Errorf("required default registry tool %q is not registered", name)
			continue
		}
		if e.PreferHost {
			continue
		}
		if e.DefaultVersion == "" {
			t.Errorf("default registry tool %q has no DefaultVersion pinned", name)
			continue
		}
		for _, p := range supportedPlatforms {
			sha, ok := binmgr.PinnedSHA256(e.Name, e.DefaultVersion, p)
			if !ok || sha == "" {
				t.Errorf("default registry tool %q version %q has no pin for supported platform %s", e.Name, e.DefaultVersion, p)
			}
			if len(sha) != 64 {
				t.Errorf("default registry tool %q pin for %s is not 64 hex chars: %q", e.Name, p, sha)
			}
		}
	}

	// Also verify that ALL downloadable entries in the default registry have pins.
	for _, e := range All() {
		if e.PreferHost {
			continue
		}
		if e.DefaultVersion == "" {
			t.Errorf("entry %q has no DefaultVersion", e.Name)
			continue
		}
		for _, p := range supportedPlatforms {
			sha, ok := binmgr.PinnedSHA256(e.Name, e.DefaultVersion, p)
			if !ok || sha == "" {
				t.Errorf("entry %q (%s) has no pin for %s", e.Name, e.DefaultVersion, p)
			}
		}
	}
}

func TestRegistryVerificationFailsClosedOnMissingPin(t *testing.T) {
	e := Entry{
		Name:           "unpinned-test-tool",
		DefaultVersion: "1.0.0",
		Resolve: func(ctx context.Context, version string) (binmgr.Tool, error) {
			return binmgr.Tool{
				Name:    "unpinned-test-tool",
				Version: version,
				Assets: map[string]binmgr.Asset{
					binmgr.Platform(): {URL: "http://127.0.0.1:0/bin"},
				},
			}, nil
		},
	}
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	if _, err := e.Ensure(context.Background()); err == nil {
		t.Fatal("expected Ensure to fail closed for unpinned tool, but succeeded")
	}
}
