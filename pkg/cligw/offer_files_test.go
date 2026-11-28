package cligw

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qiangli/yoke/pkg/sharedfiles"
	"github.com/qiangli/yoke/pkg/toolcmd"
)

func TestPeerFilesRefuseHostPathAndFetchURI(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	local := filepath.Join(home, "secret.json")
	if err := os.WriteFile(local, []byte(`{"secret":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, release, err := preparePeerFiles(context.Background(), local, toolcmd.Options{}); err == nil || !strings.Contains(err.Error(), "bashy://") || release != nil {
		t.Fatalf("host path: %v", err)
	}
	if _, _, _, err := preparePeerFiles(context.Background(), "--file="+local, toolcmd.Options{}); err == nil {
		t.Fatal("flag value opened host path")
	}
	hits := 0
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path != "/files/data/input.json" || r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("unexpected fetch request: %s %s", r.URL.Path, r.Header.Get("Authorization"))
			w.WriteHeader(403)
			return
		}
		fmt.Fprint(w, `{"ok":true}`)
	}))
	defer door.Close()
	peers := filepath.Join(home, "toolcmd", "peers.json")
	if err := os.MkdirAll(filepath.Dir(peers), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(peers, []byte(fmt.Sprintf(`{"peers":{"peer":{"url":%q,"credential":"token"}}}`, door.URL)), 0600); err != nil {
		t.Fatal(err)
	}
	args, opts, release, err := preparePeerFiles(context.Background(), "bashy://peer/files-data/input.json", toolcmd.Options{Argv: []string{"old"}})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if hits != 1 || args != opts.Argv[0] || !strings.HasPrefix(args, opts.Dir) {
		t.Fatalf("fetch result: hits=%d args=%q opts=%+v", hits, args, opts)
	}
	data, err := os.ReadFile(args)
	if err != nil || string(data) != `{"ok":true}` {
		t.Fatalf("fetched file: %q %v", data, err)
	}
}

func TestPublishPeerArtifacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("BASHY_HOME", home)
	source := filepath.Join(t.TempDir(), "note.txt")
	if err := os.WriteFile(source, []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	res, err := publishPeerArtifacts(toolcmd.Result{Artifacts: []string{source}, Dir: "/private", Argv: []string{"/private"}, Text: "done"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Artifacts) != 0 || res.Dir != "" || len(res.Argv) != 0 || len(res.ArtifactRefs) != 1 || res.Text != "done" {
		t.Fatalf("peer result: %+v", res)
	}
	ref := res.ArtifactRefs[0]
	sum := sha256.Sum256([]byte("hello"))
	if ref.Size != 5 || ref.SHA256 != hex.EncodeToString(sum[:]) || !strings.Contains(ref.URI, "/files-offers/") {
		t.Fatalf("artifact ref: %+v", ref)
	}
	target, err := sharedfiles.ParseTarget(ref.URI)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(home, "shared", "offers", filepath.FromSlash(target.Path)))
	if err != nil || string(data) != "hello" {
		t.Fatalf("published file: %q %v", data, err)
	}
	if _, err := os.Stat(filepath.Join(home, "shared", "offers.json")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(slashText(res), ref.URI) {
		t.Fatal("reply omitted artifact URI")
	}
}
