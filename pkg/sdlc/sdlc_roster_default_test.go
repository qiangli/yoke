package sdlc

import (
	"os"
	"path/filepath"
	"testing"
	"testing/fstest"

	"github.com/qiangli/yoke/pkg/fleet"
)

// isolatedSDLCCatalog pins conductor defaults to an embedded-baseline catalog
// rooted at an empty temp dir: no developer-local or cloud overlay.
func isolatedSDLCCatalog(t *testing.T) *fleet.Catalog {
	t.Helper()
	t.Setenv("BASHY_ROSTERS_PATH", "")
	t.Setenv("BASHY_TOOLS_PATH", "")
	return fleet.New(fleet.WithRoot(t.TempDir()), fleet.WithoutCloudOverlay())
}

func pinSDLCCatalog(t *testing.T, c *fleet.Catalog) {
	t.Helper()
	prev := sdlcCatalog
	sdlcCatalog = func() *fleet.Catalog { return c }
	t.Cleanup(func() { sdlcCatalog = prev })
}

func TestSDLCConductorDefaultFromSeededRoster(t *testing.T) {
	clearAgentDetectionEnv(t)
	pinSDLCCatalog(t, isolatedSDLCCatalog(t))
	if got := DefaultConductorAgent(); got != "codex" {
		t.Fatalf("DefaultConductorAgent=%q, want codex (seeded roles/sdlc-conductor-fallback)", got)
	}
}

func TestSDLCConductorDefaultSeedsOffFallsBack(t *testing.T) {
	clearAgentDetectionEnv(t)
	t.Setenv(fleet.SeedsEnv, "off")
	pinSDLCCatalog(t, isolatedSDLCCatalog(t))
	if got := DefaultConductorAgent(); got != sdlcConductorFallback {
		t.Fatalf("DefaultConductorAgent=%q, want fallback %q with seeds off", got, sdlcConductorFallback)
	}
}

func TestSDLCConductorDefaultReshapedRosterWins(t *testing.T) {
	clearAgentDetectionEnv(t)
	root := t.TempDir()
	t.Setenv("BASHY_ROSTERS_PATH", "")
	dir := filepath.Join(root, "rosters")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "roles.yaml"),
		[]byte("name: roles\nroles:\n  sdlc-conductor-fallback: opencode\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	pinSDLCCatalog(t, fleet.New(fleet.WithRoot(root), fleet.WithoutCloudOverlay()))
	if got := DefaultConductorAgent(); got != "opencode" {
		t.Fatalf("DefaultConductorAgent=%q, want opencode (reshaped local roster)", got)
	}
}

// normalizeAgentName resolves through the catalog's tool names and aliases,
// so newly seeded tools are recognized without a Go change.
func TestNormalizeAgentNameFromCatalog(t *testing.T) {
	pinSDLCCatalog(t, isolatedSDLCCatalog(t))
	cases := map[string]string{
		"claude":       "claude",
		"claude-code":  "claude",
		"CLAUDE_CODE":  "claude",
		"codex":        "codex",
		"opencode":     "opencode",
		"agy":          "agy",
		"antigravity":  "agy",
		"anti-gravity": "agy",
		"muse":         "muse",
		"muse-code":    "muse",
		"zcode":        "zcode",
		"bogus-agent":  "",
		"":             "",
	}
	for in, want := range cases {
		if got := normalizeAgentName(in); got != want {
			t.Errorf("normalizeAgentName(%q)=%q, want %q", in, got, want)
		}
	}
}

// The historical spellings still apply when the catalog is empty (seeds off,
// nothing mounted): a retired harness must stay recognizable.
func TestNormalizeAgentNameSeedsOffFallback(t *testing.T) {
	empty := fleet.New(fleet.WithRoot(t.TempDir()),
		fleet.WithoutCloudOverlay(), fleet.WithBaselineFS(fstest.MapFS{}))
	pinSDLCCatalog(t, empty)
	for in, want := range map[string]string{
		"claude-code":  "claude",
		"openai-codex": "codex",
		"open-code":    "opencode",
		"antigravity":  "agy",
		"muse":         "",
		"zcode":        "",
	} {
		if got := normalizeAgentName(in); got != want {
			t.Errorf("normalizeAgentName(%q)=%q, want %q", in, got, want)
		}
	}
}

func TestDefaultConductorAgentExplicitMuse(t *testing.T) {
	clearAgentDetectionEnv(t)
	pinSDLCCatalog(t, isolatedSDLCCatalog(t))
	t.Setenv("BASHY_AGENT", "muse")
	if got := DefaultConductorAgent(); got != "muse" {
		t.Fatalf("DefaultConductorAgent=%q, want muse (catalog name, not the old frozen list)", got)
	}
}
