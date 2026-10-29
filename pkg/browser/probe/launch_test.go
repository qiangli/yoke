package probe

import (
	"context"
	"errors"
	"slices"
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
