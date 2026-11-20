package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/binmgr"
)

func TestPlatformChromeCandidatesIncludeEdge(t *testing.T) {
	tests := []struct {
		goos  string
		paths []string
		names []string
	}{
		{
			goos: "windows",
			paths: []string{
				`C:\Program Files\Microsoft\Edge\Application\msedge.exe`,
				`C:\Program Files (x86)\Microsoft\Edge\Application\msedge.exe`,
			},
		},
		{
			goos:  "darwin",
			paths: []string{"/Applications/Microsoft Edge.app/Contents/MacOS/Microsoft Edge"},
		},
		{
			goos:  "linux",
			names: []string{"microsoft-edge", "microsoft-edge-stable"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.goos, func(t *testing.T) {
			gotPaths := platformChromePaths(tt.goos)
			for _, want := range tt.paths {
				if !slices.Contains(gotPaths, want) {
					t.Errorf("paths for %s missing %q: %v", tt.goos, want, gotPaths)
				}
			}
			gotNames := platformChromeNames(tt.goos)
			for _, want := range tt.names {
				if !slices.Contains(gotNames, want) {
					t.Errorf("names for %s missing %q: %v", tt.goos, want, gotNames)
				}
			}
		})
	}
}

func TestResolverProvisionsPinnedHeadlessShell(t *testing.T) {
	ensured := false
	r := Resolver{
		Detect: func() string { return "" },
		Cached: func(binmgr.Tool) (string, bool) { return "", false },
		Ensure: func(_ context.Context, tool binmgr.Tool) (string, error) {
			ensured = true
			if tool.Name != "chrome-headless-shell" || tool.Version != binmgr.ChromeHeadlessShellVersion {
				t.Fatalf("unexpected managed tool: %#v", tool)
			}
			asset, ok := tool.Assets[binmgr.Platform()]
			if !ok || asset.SHA256 == "" || !asset.Tree || asset.Entrypoint == "" {
				t.Fatalf("platform asset is not a pinned tree: %#v", asset)
			}
			return "/managed/chrome-headless-shell", nil
		},
	}
	got, err := r.Resolve(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if !ensured || got != "/managed/chrome-headless-shell" {
		t.Fatalf("Resolve = %q, ensured=%t", got, ensured)
	}
}

func TestResolverReportsProvisionFailure(t *testing.T) {
	r := Resolver{
		Detect: func() string { return "" },
		Cached: func(binmgr.Tool) (string, bool) { return "", false },
		Ensure: func(context.Context, binmgr.Tool) (string, error) { return "", errors.New("offline") },
	}
	if _, err := r.Resolve(context.Background(), ""); err == nil {
		t.Fatal("expected provisioning error")
	}
}

func TestChromeCommandLinuxRootAndRuntime(t *testing.T) {
	t.Setenv("LD_LIBRARY_PATH", "/host/libs")
	cmd := chromeCommand("/managed/chrome", 9335, "/profile", "linux", 0, "/managed/runtime", true)
	if !slices.Contains(cmd.Args, "--no-sandbox") {
		t.Fatalf("root Linux args missing --no-sandbox: %v", cmd.Args)
	}
	want := "LD_LIBRARY_PATH=/managed/runtime" + string(os.PathListSeparator) + "/host/libs"
	if !slices.Contains(cmd.Env, want) {
		t.Fatalf("child env missing %q: %v", want, cmd.Env)
	}
	for _, inherited := range os.Environ() {
		if strings.HasPrefix(inherited, "LD_LIBRARY_PATH=") && inherited != want && slices.Contains(cmd.Env, inherited) {
			t.Fatalf("child retained duplicate LD_LIBRARY_PATH: %v", cmd.Env)
		}
	}
}

func TestChromeCommandDoesNotDisableSandboxForNonRoot(t *testing.T) {
	cmd := chromeCommand("/managed/chrome", 9335, "/profile", "linux", 1000, "", true)
	if slices.Contains(cmd.Args, "--no-sandbox") {
		t.Fatalf("non-root Linux args contain --no-sandbox: %v", cmd.Args)
	}
}

func TestChromeCommandKeychainIsolationEphemeralVsPersistent(t *testing.T) {
	ephemeral := chromeCommand("/managed/chrome", 9335, "/tmp/ephemeral", "darwin", 501, "", true)
	for _, want := range []string{"--use-mock-keychain", "--password-store=basic"} {
		if !slices.Contains(ephemeral.Args, want) {
			t.Fatalf("ephemeral probe args missing %q: %v", want, ephemeral.Args)
		}
	}

	persistent := chromeCommand("/managed/chrome", 9222, "/cache/browser-probe-9222", "darwin", 501, "", false)
	for _, unwanted := range []string{"--use-mock-keychain", "--password-store=basic"} {
		if slices.Contains(persistent.Args, unwanted) {
			t.Fatalf("initialized persistent probe profile must not pass %q (would invalidate keychain-encrypted cookies): %v", unwanted, persistent.Args)
		}
	}
}

func TestResolveLaunchProfileEphemeralPersistentAndRealProfileRefusal(t *testing.T) {
	fakeHome := t.TempDir()
	t.Setenv("HOME", fakeHome)
	t.Setenv("XDG_CACHE_HOME", filepath.Join(fakeHome, ".cache"))

	// 1. No existing DefaultUserDataDir on fresh port -> throwaway temp dir + ephemeral=true.
	dir, ephemeral, createdTemp, err := resolveLaunchProfile(19335, "", fakeHome, "darwin")
	if err != nil {
		t.Fatalf("resolveLaunchProfile ephemeral: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if !ephemeral || !createdTemp || dir == DefaultUserDataDir(19335) {
		t.Fatalf("got dir=%q ephemeral=%t createdTemp=%t, want throwaway temp dir", dir, ephemeral, createdTemp)
	}

	// 2. Initialized persistent DefaultUserDataDir (e.g. :9222 with Local State) -> preserved, ephemeral=false.
	persistDefault := DefaultUserDataDir(19222)
	if err := os.MkdirAll(persistDefault, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(persistDefault) })
	if err := os.WriteFile(filepath.Join(persistDefault, "Local State"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	dir2, ephemeral2, createdTemp2, err := resolveLaunchProfile(19222, "", fakeHome, "darwin")
	if err != nil {
		t.Fatalf("resolveLaunchProfile persistent default: %v", err)
	}
	if dir2 != persistDefault || ephemeral2 || createdTemp2 {
		t.Fatalf("got dir=%q ephemeral=%t createdTemp=%t, want %q false false", dir2, ephemeral2, createdTemp2, persistDefault)
	}

	// 3. Explicit uninitialized dir -> kept, ephemeral=true; once initialized ("Default" dir) -> ephemeral=false.
	explicit := filepath.Join(t.TempDir(), "custom-probe")
	dir3, ephemeral3, createdTemp3, err := resolveLaunchProfile(19336, explicit, fakeHome, "linux")
	if err != nil || dir3 != explicit || !ephemeral3 || createdTemp3 {
		t.Fatalf("fresh explicit: dir=%q ephemeral=%t temp=%t err=%v", dir3, ephemeral3, createdTemp3, err)
	}
	if err := os.MkdirAll(filepath.Join(explicit, "Default"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, ephemeral4, _, err := resolveLaunchProfile(19336, explicit, fakeHome, "linux")
	if err != nil || ephemeral4 {
		t.Fatalf("initialized explicit: ephemeral=%t err=%v, want false", ephemeral4, err)
	}

	// 4. Real OS browser profiles must be refused across macOS, Linux, and Windows.
	for _, tc := range []struct {
		goos string
		path string
	}{
		{"darwin", filepath.Join(fakeHome, "Library", "Application Support", "Google", "Chrome")},
		{"darwin", filepath.Join(fakeHome, "Library", "Application Support", "Google", "Chrome", "Default")},
		{"darwin", filepath.Join(fakeHome, "Library", "Application Support", "BraveSoftware", "Brave-Browser")},
		{"darwin", filepath.Join(fakeHome, "Library", "Application Support", "Microsoft Edge")},
		{"linux", filepath.Join(fakeHome, ".config", "google-chrome")},
		{"linux", filepath.Join(fakeHome, ".config", "chromium", "Default")},
		{"windows", filepath.Join(fakeHome, "AppData", "Local", "Google", "Chrome", "User Data")},
	} {
		if err := RejectRealBrowserProfile(tc.path, fakeHome, tc.goos); err == nil {
			t.Fatalf("RejectRealBrowserProfile(%q, goos=%s) succeeded, want refusal", tc.path, tc.goos)
		}
	}
}
