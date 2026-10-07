package binmgr

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

func TestSeedRoundtripOfflineCache(t *testing.T) {
	root := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", root)
	rel := filepath.Join("fixture-tool", "v1", BinaryName("fixture-tool"))
	target := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(target, []byte("verified cache"), 0755); err != nil {
		t.Fatal(err)
	}
	// A lexically later version is older; importing must preserve cache selection.
	older := filepath.Join(root, "fixture-tool", "v2", BinaryName("fixture-tool"))
	if err := os.MkdirAll(filepath.Dir(older), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(older, []byte("older cache"), 0755); err != nil {
		t.Fatal(err)
	}
	then := time.Now().Add(-time.Hour)
	if err := os.Chtimes(older, then, then); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "seed.tar")
	if err := ExportSeed(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", dest)
	t.Setenv("BASHY_OFFLINE", "1")
	if err := ImportSeed(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dest, rel))
	if err != nil || string(b) != "verified cache" {
		t.Fatalf("roundtrip: %q %v", b, err)
	}
	if got := CachedBinary("fixture-tool"); got != filepath.Join(dest, rel) {
		t.Fatalf("seed changed latest selection: %s", got)
	}
	tool, err := ResolveGitHub(context.Background(), GitHubSpec{Name: "fixture-tool", Repo: "fixture/tool", Version: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = Ensure(context.Background(), tool); err != nil {
		t.Fatal(err)
	}
}

func TestSeedRejectsCorruptionAndTraversalBeforePublish(t *testing.T) {
	for _, tc := range []struct {
		name, path, digest string
		duplicate          bool
	}{
		{name: "digest", path: "tool/v1/tool", digest: "0000000000000000000000000000000000000000000000000000000000000000"},
		{name: "traversal", path: "../escape"},
		{name: "duplicate", path: "tool/v1/tool", duplicate: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := t.TempDir()
			t.Setenv("BASHY_BIN_CACHE", root)
			keep := filepath.Join(root, "tool", "v1", "tool")
			os.MkdirAll(filepath.Dir(keep), 0755)
			os.WriteFile(keep, []byte("old"), 0755)
			data := []byte("new")
			sum := sha256.Sum256(data)
			digest := hex.EncodeToString(sum[:])
			if tc.digest != "" {
				digest = tc.digest
			}
			entry := seedEntry{Path: tc.path, Name: "tool", Version: "v1", SHA256: digest, Size: int64(len(data)), Mode: 0755}
			manifest := seedManifest{Schema: 1, Platform: Platform(), Entries: []seedEntry{entry}}
			if tc.duplicate {
				manifest.Entries = append(manifest.Entries, entry)
			}
			body, _ := json.Marshal(manifest)
			var buf bytes.Buffer
			tw := tar.NewWriter(&buf)
			tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(body))})
			tw.Write(body)
			tw.WriteHeader(&tar.Header{Name: "cache/" + tc.path, Mode: 0755, Size: int64(len(data))})
			tw.Write(data)
			tw.Close()
			archive := filepath.Join(t.TempDir(), "bad.tar")
			os.WriteFile(archive, buf.Bytes(), 0600)
			if err := ImportSeed(context.Background(), archive); err == nil {
				t.Fatal("invalid seed accepted")
			}
			b, err := os.ReadFile(keep)
			if err != nil || string(b) != "old" {
				t.Fatalf("cache changed on invalid seed: %q %v", b, err)
			}
		})
	}
}

func TestSeedCarriesInCacheSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	root := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", root)
	bin := filepath.Join(root, "podman-gvproxy", "v1", "gvproxy")
	if err := os.MkdirAll(filepath.Dir(bin), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("helper"), 0755); err != nil {
		t.Fatal(err)
	}
	helpers := filepath.Join(root, "podman-helpers")
	os.MkdirAll(helpers, 0755)
	// provisionDarwinHelpers writes absolute links; tree archives write relative ones.
	if err := os.Symlink(bin, filepath.Join(helpers, "gvproxy")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("gvproxy", filepath.Join(root, "podman-gvproxy", "v1", "alias")); err != nil {
		t.Fatal(err)
	}
	archive := filepath.Join(t.TempDir(), "seed.tar")
	if err := ExportSeed(context.Background(), archive); err != nil {
		t.Fatal(err)
	}
	dest := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", dest)
	for i := 0; i < 2; i++ { // reimport replaces existing links
		if err := ImportSeed(context.Background(), archive); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.Readlink(filepath.Join(dest, "podman-helpers", "gvproxy"))
	if err != nil || got != "../podman-gvproxy/v1/gvproxy" {
		t.Fatalf("helper link: %q %v", got, err)
	}
	for _, p := range []string{filepath.Join(dest, "podman-helpers", "gvproxy"), filepath.Join(dest, "podman-gvproxy", "v1", "alias")} {
		if b, err := os.ReadFile(p); err != nil || string(b) != "helper" {
			t.Fatalf("%s: %q %v", p, b, err)
		}
	}

	escape := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(escape, []byte("x"), 0755)
	t.Setenv("BASHY_BIN_CACHE", root)
	if err := os.Symlink(escape, filepath.Join(helpers, "vfkit")); err != nil {
		t.Fatal(err)
	}
	if err := ExportSeed(context.Background(), filepath.Join(t.TempDir(), "bad.tar")); err == nil {
		t.Fatal("exported a symlink leaving the cache")
	}
}

func TestSeedRejectsEscapingAndTraversedLinks(t *testing.T) {
	for _, entries := range [][]seedEntry{
		{{Path: "tool/v1/l", Name: "tool", Version: "v1", Link: "../../../escape"}},
		{{Path: "tool/v1/l", Name: "tool", Version: "v1", Link: "/etc"}},
		{{Path: "tool/l", Name: "tool", Version: "managed", Link: "v1"}, {Path: "tool/l/x", Name: "tool", Version: "l", SHA256: linkDigest("x"), Size: 1}},
		// Lexically "outside" in the cache; a/b/up -> a makes it ../outside.
		{{Path: "a/b/up", Name: "a", Version: "b", Link: ".."}, {Path: "a/e", Name: "a", Version: "managed", Link: "b/up/../../../outside"}},
		{{Path: "a/x", Name: "a", Version: "managed", Link: "y"}, {Path: "a/y", Name: "a", Version: "managed", Link: "x"}}, // cycle
		{{Path: "a/x", Name: "a", Version: "managed", Link: "missing"}},                                                    // dangling
	} {
		root := t.TempDir()
		t.Setenv("BASHY_BIN_CACHE", root)
		for i := range entries {
			if entries[i].Link != "" {
				entries[i].SHA256, entries[i].Size = linkDigest(entries[i].Link), int64(len(entries[i].Link))
			}
		}
		body, _ := json.Marshal(seedManifest{Schema: 1, Platform: Platform(), Entries: entries})
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(body))})
		tw.Write(body)
		for _, e := range entries {
			if e.Link != "" {
				tw.WriteHeader(&tar.Header{Name: "cache/" + e.Path, Typeflag: tar.TypeSymlink, Linkname: e.Link})
			} else {
				tw.WriteHeader(&tar.Header{Name: "cache/" + e.Path, Mode: 0755, Size: 1})
				tw.Write([]byte("x"))
			}
		}
		tw.Close()
		archive := filepath.Join(t.TempDir(), "bad.tar")
		os.WriteFile(archive, buf.Bytes(), 0600)
		if err := ImportSeed(context.Background(), archive); err == nil {
			t.Fatalf("invalid link seed accepted: %+v", entries)
		}
		if left, _ := os.ReadDir(root); len(left) != 0 {
			t.Fatalf("cache changed on invalid seed: %v", left)
		}
	}
}

// An archive link must not resolve through a link already in the target cache:
// the existing evil -> outside would make the lexically safe l escape.
func TestSeedImportIgnoresExistingCacheLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	root := t.TempDir()
	t.Setenv("BASHY_BIN_CACHE", root)
	outside := filepath.Join(t.TempDir(), "outside")
	os.WriteFile(outside, []byte("x"), 0755)
	if err := os.Symlink(outside, filepath.Join(root, "evil")); err != nil {
		t.Fatal(err)
	}
	link := "../../evil"
	body, _ := json.Marshal(seedManifest{Schema: 1, Platform: Platform(), Entries: []seedEntry{{Path: "tool/v1/l", Name: "tool", Version: "v1", SHA256: linkDigest(link), Size: int64(len(link)), Mode: 0777, Link: link}}})
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(body))})
	tw.Write(body)
	tw.WriteHeader(&tar.Header{Name: "cache/tool/v1/l", Typeflag: tar.TypeSymlink, Linkname: link})
	tw.Close()
	archive := filepath.Join(t.TempDir(), "bad.tar")
	os.WriteFile(archive, buf.Bytes(), 0600)
	if err := ImportSeed(context.Background(), archive); err == nil {
		t.Fatal("imported a link resolving through an existing cache link")
	}
	if _, err := os.Lstat(filepath.Join(root, "tool", "v1", "l")); !os.IsNotExist(err) {
		t.Fatalf("link published: %v", err)
	}
}

// Export must check the resolved chain, not only the lexical target.
func TestSeedExportRejectsResolvedEscape(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privilege on Windows")
	}
	for name, links := range map[string][][2]string{
		"dirdotdot": {{"a/b/up", ".."}, {"a/e", "b/up/../../../outside"}},
		"dangling":  {{"a/x", "missing"}},
		"cycle":     {{"a/x", "y"}, {"a/y", "x"}},
	} {
		t.Run(name, func(t *testing.T) {
			root := filepath.Join(t.TempDir(), "cache")
			os.WriteFile(filepath.Join(filepath.Dir(root), "outside"), []byte("x"), 0755)
			t.Setenv("BASHY_BIN_CACHE", root)
			for _, l := range links {
				p := filepath.Join(root, filepath.FromSlash(l[0]))
				os.MkdirAll(filepath.Dir(p), 0755)
				if err := os.Symlink(l[1], p); err != nil {
					t.Fatal(err)
				}
			}
			if err := ExportSeed(context.Background(), filepath.Join(t.TempDir(), "bad.tar")); err == nil {
				t.Fatal("exported a link that does not resolve inside the cache")
			}
		})
	}
}
