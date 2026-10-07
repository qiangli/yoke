package binmgr

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type refuseOfflineHTTP struct{ calls int }

func (r *refuseOfflineHTTP) RoundTrip(*http.Request) (*http.Response, error) {
	r.calls++
	return nil, io.EOF
}

func TestOfflineWarmResolutionAndColdRefusal(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", root)
	t.Setenv("BASHY_OFFLINE", "1")
	old := http.DefaultClient
	transport := &refuseOfflineHTTP{}
	http.DefaultClient = &http.Client{Transport: transport}
	defer func() { http.DefaultClient = old }()
	target := filepath.Join(root, "fixture-tool", "v1", BinaryName("fixture-tool"))
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("cached"), 0755); err != nil {
		t.Fatal(err)
	}
	tool, err := ResolveGitHub(context.Background(), GitHubSpec{Name: "fixture-tool", Repo: "fixture/tool", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	got, err := Ensure(context.Background(), tool)
	if err != nil || got != target {
		t.Fatalf("warm=%q %v", got, err)
	}
	_, err = ResolveGitHub(context.Background(), GitHubSpec{Name: "fixture-tool", Repo: "fixture/tool", Version: "v2"})
	if err == nil || !strings.Contains(err.Error(), "v2") || !strings.Contains(err.Error(), "BASHY_OFFLINE") {
		t.Fatalf("cold error=%v", err)
	}
	_, err = ResolveGitHub(context.Background(), GitHubSpec{Name: "fixture-tool", Repo: "fixture/tool", Version: "v1", RequireArchive: true})
	if err == nil || !strings.Contains(err.Error(), "BASHY_OFFLINE") {
		t.Fatalf("legacy executable incorrectly satisfies archive: %v", err)
	}
	archiveTarget := filepath.Join(root, "fixture-tool-archive", "v1", BinaryName("fixture-tool-archive"))
	if err := os.MkdirAll(filepath.Dir(archiveTarget), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(archiveTarget, []byte("archive-content"), 0755); err != nil {
		t.Fatal(err)
	}
	rememberGitHubTool(GitHubSpec{Name: "fixture-tool", Repo: "fixture/tool", Version: "v1"}, Tool{
		Name:    "fixture-tool",
		Version: "v1",
		Assets: map[string]Asset{
			Platform(): {URL: "cache-only", SHA256: "dummy"},
		},
	})
	archiveTool, err := ResolveGitHub(context.Background(), GitHubSpec{Name: "fixture-tool", Repo: "fixture/tool", Version: "v1", RequireArchive: true})
	if err != nil {
		t.Fatalf("offline archive resolution failed: %v", err)
	}
	gotArchive, err := EnsureArchive(context.Background(), archiveTool)
	if err != nil || gotArchive != archiveTarget {
		t.Fatalf("warm archive=%q %v", gotArchive, err)
	}
	_, err = ProvisionManaged(context.Background(), ManagedSpec{Name: "missing-managed", Build: func(context.Context, string) error { t.Fatal("offline invoked source build"); return nil }})
	if err == nil {
		t.Fatal("offline provision succeeded")
	}
	if transport.calls != 0 {
		t.Fatalf("offline made %d HTTP requests", transport.calls)
	}
}
