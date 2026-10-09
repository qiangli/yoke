package agentlaunch

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/qiangli/yoke/pkg/binmgr"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/fleet/fleettest"
)

// The one live check of the managed-install path: resolve the baseline
// opencode pin, install it into the bashy cache from the vendor's release
// (network), and run `opencode --version` from THAT path — not from PATH or
// Homebrew. Opt-in, like every live test here: YOKE_LIVE_MANAGED=opencode.
func TestLiveManagedOpencodeRunsFromBashyCache(t *testing.T) {
	if os.Getenv("YOKE_LIVE_MANAGED") != "opencode" {
		t.Skip("set YOKE_LIVE_MANAGED=opencode to download the pinned opencode and run --version from the bashy cache")
	}
	fleettest.Ring(t)
	t.Setenv(UnsafeLaunchEnv, "1")
	t.Setenv(fleet.ToolBinaryOverrideEnv("opencode"), "")
	l, err := ResolveWithCatalog("opencode", Options{}, NewCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if l.ManagedTool == nil {
		t.Fatalf("opencode is not managed: %+v", l)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	path, err := EnsureManaged(ctx, l)
	if err != nil {
		t.Fatal(err)
	}
	cache, _ := binmgr.CacheDir()
	if !strings.HasPrefix(path, cache) {
		t.Fatalf("installed outside the bashy cache: %s (cache %s)", path, cache)
	}
	cmd := exec.CommandContext(ctx, path, "--version")
	cmd.Env = ApplyLaunchEnv(os.Environ(), l)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s --version: %v\n%s", path, err, out)
	}
	got := strings.TrimSpace(string(out))
	t.Logf("LIVE: %s --version -> %q (pin %s)", path, got, l.ManagedTool.Version)
	if !strings.Contains(got, l.ManagedTool.Version) {
		t.Fatalf("--version = %q, want the pinned %s", got, l.ManagedTool.Version)
	}
	if homebrew, lookErr := exec.LookPath("opencode"); lookErr == nil && homebrew == path {
		t.Fatalf("resolved to the PATH copy %s, not the cache", homebrew)
	}
}
