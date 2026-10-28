package live

// The Chrome extension under ./extension/ is shipped in every bashy
// binary (~few KB of static assets) so `bashy browser setup live`
// works without a special build flag.
//
// Provenance: the extension bytes and the hub/service Go code were
// migrated verbatim from ycode's internal/runtime/mcpservers/live
// package (Apache-2.0). See THIRD_PARTY_LICENSES.md. The hub is
// transport-shaped and type-agnostic; only service.go was adapted to
// coreutils' pkg/browser/wire types.

import (
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

//go:embed all:extension
var extensionFS embed.FS

// ExtractExtension copies the embedded extension tree to dst, creating
// dst if needed. Existing files are overwritten. Returns the absolute
// dst path so the caller can print it for the user.
func ExtractExtension(dst string) (string, error) {
	if err := os.MkdirAll(dst, 0o755); err != nil {
		return "", err
	}
	root := "extension"
	err := fs.WalkDir(extensionFS, root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := extensionFS.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
	if err != nil {
		return "", fmt.Errorf("extract extension: %w", err)
	}
	abs, _ := filepath.Abs(dst)
	return abs, nil
}

// DefaultExtractDir is the canonical location for the unpacked live
// extension: ~/Downloads/ycode-chrome-ext.
//
// The directory basename is kept as ycode-chrome-ext (not renamed to
// bashy-*) deliberately: an extension already loaded from that path by
// a prior `ycode browser setup live` keeps working against the bashy
// hub — same WebSocket protocol, same port — so the migration does not
// force anyone to re-load the extension. ~/Downloads is used because
// Finder/Explorer hide ~/.cache and ~/Library, blocking Chrome's "Load
// unpacked" dialog from navigating there.
func DefaultExtractDir() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, "Downloads", "ycode-chrome-ext")
}

// EmbeddedVersion is the version of the extension shipped in this
// binary (its manifest.json).
func EmbeddedVersion() string {
	b, err := extensionFS.ReadFile("extension/manifest.json")
	if err != nil {
		return ""
	}
	return manifestVersion(b)
}

func manifestVersion(b []byte) string {
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &m) != nil {
		return ""
	}
	return m.Version
}

// RefreshIfStale re-extracts the embedded extension into dir when dir
// already holds an older copy (a folder the user loaded unpacked), so
// a chrome.runtime.reload picks up the new files. A missing folder is
// left alone: setup, not the hub, creates it. Reports whether it wrote.
func RefreshIfStale(dir string) (bool, error) {
	b, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return false, nil
	}
	have, want := manifestVersion(b), EmbeddedVersion()
	if have == "" || want == "" || !versionLess(have, want) {
		return false, nil
	}
	if _, err := ExtractExtension(dir); err != nil {
		return false, err
	}
	return true, nil
}

// GuardScript returns the extension's guard.js — the page-side field check of
// the sensitive-input guard — so probe/solo evaluate the very bytes the live
// extension injects.
func GuardScript() string {
	b, err := extensionFS.ReadFile("extension/guard.js")
	if err != nil {
		panic("live: embedded guard.js missing: " + err.Error())
	}
	return string(b)
}
