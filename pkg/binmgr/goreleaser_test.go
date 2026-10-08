package binmgr

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The spec names the fork, the pinned release and a raw-binary layout: the
// asset for a platform is itself the executable, so Member stays empty.
func TestGoreleaserSpec(t *testing.T) {
	spec := GoreleaserSpec()
	if spec.Name != "goreleaser" || spec.Repo != "qiangli/goreleaser" || spec.Version != GoreleaserVersion {
		t.Fatalf("spec=%+v", spec)
	}
	if spec.Member != "" || spec.Tree {
		t.Fatalf("goreleaser assets are raw binaries, spec=%+v", spec)
	}
}

// Every platform the fork release ships must have a committed pin, or
// EnsureGoreleaser refuses to provision there. This fails closed on the
// platform list so a new platform is a loud gap, never a silent hole.
func TestGoreleaserPinsCoverAllPlatforms(t *testing.T) {
	for _, plat := range GoreleaserPlatforms {
		if _, ok := PinnedSHA256(GoreleaserName, GoreleaserVersion, plat); !ok {
			t.Errorf("no committed sha256 for %s %s on %s", GoreleaserName, GoreleaserVersion, plat)
		}
	}
}

// Full path against a mocked fork release: asset match on the
// goreleaser-<goos>-<goarch> layout, sha from checksums.txt, then the
// committed pin overrides as the trust root and Ensure installs.
func TestEnsureGoreleaser_MockedRelease(t *testing.T) {
	bin := []byte("fake-pinned-goreleaser-bytes")
	sum := sha256hex(bin)
	goos, goarch := splitPlatform(Platform())
	asset := fmt.Sprintf("goreleaser-%s-%s", goos, goarch)

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()
	base := srv.URL

	mux.HandleFunc("/repos/qiangli/goreleaser/releases/tags/"+GoreleaserVersion, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(ghRelease{
			TagName: GoreleaserVersion,
			Assets: []ghAsset{
				{Name: asset, URL: base + "/dl/" + asset},
				{Name: "checksums.txt", URL: base + "/dl/checksums.txt"},
			},
		})
	})
	mux.HandleFunc("/dl/"+asset, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(bin) })
	mux.HandleFunc("/dl/checksums.txt", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprintf(w, "%s  %s\n", sum, asset)
	})

	old := githubAPI
	githubAPI = base
	defer func() { githubAPI = old }()
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	withPin(t, GoreleaserName+"@"+GoreleaserVersion+"/"+Platform(), sum)

	path, err := EnsureGoreleaser(context.Background())
	if err != nil {
		t.Fatalf("EnsureGoreleaser: %v", err)
	}
	if path == "" || !isExecutable(path) {
		t.Fatalf("not an executable: %q", path)
	}
}

// Without a committed pin for the platform there is no download at all.
func TestEnsureGoreleaser_RefusesUnpinned(t *testing.T) {
	key := GoreleaserName + "@" + GoreleaserVersion + "/" + Platform()
	prev, existed := pinnedDigests[key]
	delete(pinnedDigests, key)
	t.Cleanup(func() {
		if existed {
			pinnedDigests[key] = prev
		}
	})
	if _, err := EnsureGoreleaser(context.Background()); err == nil {
		t.Fatal("expected refusal without a committed pin")
	}
}
