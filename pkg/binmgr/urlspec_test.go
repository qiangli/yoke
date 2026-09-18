package binmgr

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestResolveURL_TemplatedAssetAndSidecar(t *testing.T) {
	bin := []byte("#!/bin/sh\necho hi\n")
	sum := sha256.Sum256(bin)
	want := hex.EncodeToString(sum[:])

	mux := http.NewServeMux()
	// Mirror dl.gitea.com's shape: a raw per-platform binary + a .sha256 sidecar.
	mux.HandleFunc("/act_runner/0.2.13/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, ".sha256") {
			w.Write([]byte(want + "  act_runner\n"))
			return
		}
		w.Write(bin)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	spec := URLSpec{
		Name:        "act_runner",
		Version:     "0.2.13",
		URLTemplate: srv.URL + "/act_runner/{version}/act_runner-{version}-{goos}-{goarch}{ext}",
	}
	tool, err := ResolveURL(context.Background(), spec)
	if err != nil {
		t.Fatalf("ResolveURL: %v", err)
	}
	asset, ok := tool.Assets[Platform()]
	if !ok {
		t.Fatalf("no asset for %s", Platform())
	}
	if asset.SHA256 != want {
		t.Fatalf("sha256 = %s, want %s", asset.SHA256, want)
	}
	if !strings.Contains(asset.URL, "act_runner-0.2.13-") {
		t.Fatalf("url not expanded: %s", asset.URL)
	}

	// Round-trip through Ensure to prove the verify path accepts it.
	t.Setenv("BASHY_BIN_CACHE", t.TempDir())
	path, err := Ensure(context.Background(), tool)
	if err != nil {
		t.Fatalf("Ensure: %v", err)
	}
	if !isExecutable(path) {
		t.Fatalf("not executable: %s", path)
	}
}

func TestResolveURL_RequiresFields(t *testing.T) {
	if _, err := ResolveURL(context.Background(), URLSpec{Name: "x"}); err == nil {
		t.Fatal("expected error for missing version/template")
	}
}

// A pinned resolution never reaches the network: the record's digest is the
// trust root, and the URL is expanded for this platform verbatim.
func TestResolveURLPinnedIsOffline(t *testing.T) {
	tool := ResolveURLPinned(URLSpec{Name: "x", Version: "v1", URLTemplate: "http://127.0.0.1:1/{version}/x-{goos}-{goarch}{ext}", Member: "m"}, "ABCD")
	a, ok := tool.Assets[Platform()]
	if !ok {
		t.Fatalf("no asset for %s", Platform())
	}
	if a.SHA256 != "abcd" || a.Binary != "m" || !strings.Contains(a.URL, "/v1/x-") {
		t.Errorf("asset = %+v", a)
	}
	if tool.Name != "x" || tool.Version != "v1" {
		t.Errorf("tool = %+v", tool)
	}
}
