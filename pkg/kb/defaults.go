package kb

import (
	"os"
	"path/filepath"
	"strings"
)

// Default pages: generic knowledge that ships with Bashy's SOURCE, not its
// binary. Every install can fetch the release-pinned source
// (`bashy src fetch bashy` → $BASHY_HOME/src/bashy), and the curated store
// there (kb/defaults) is read as a read-only ring: it fills in what the
// writable rings (agent, repo, host) do not have, a local page with the same
// slug always wins, and no write ever lands in it. Scripts, skills and kb
// pages stay external to the binary; only the bootstrap lives inside.

// DefaultsDirEnv overrides where the default pages are read from.
const DefaultsDirEnv = "BASHY_KB_DEFAULTS_DIR"

// DefaultsSub is the store's path inside the bashy source tree.
const DefaultsSub = "kb/defaults"

// DefaultsDir resolves the default-page store: $BASHY_KB_DEFAULTS_DIR, else
// ${BASHY_HOME:-$HOME/.bashy}/src/bashy/kb/defaults.
func DefaultsDir() string {
	if d := strings.TrimSpace(os.Getenv(DefaultsDirEnv)); d != "" {
		return d
	}
	home := strings.TrimSpace(os.Getenv("BASHY_HOME"))
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		home = filepath.Join(h, ".bashy")
	}
	return filepath.Join(home, "src", "bashy", filepath.FromSlash(DefaultsSub))
}

// OpenDefaults opens the default-page store for reading, or returns nil when
// it is absent (the source pin was never fetched). It records no host, like a
// committed repo store; callers must only read from it.
func OpenDefaults() *Store {
	dir := DefaultsDir()
	if dir == "" {
		return nil
	}
	if fi, err := os.Stat(filepath.Join(dir, "pages")); err != nil || !fi.IsDir() {
		return nil
	}
	return OpenRepo(dir)
}

// DefaultPages lists the default pages, each marked Default, or nil when the
// store is absent or unreadable (defaults never break a read).
func DefaultPages() []*Page {
	s := OpenDefaults()
	if s == nil {
		return nil
	}
	pages, err := s.List()
	if err != nil {
		return nil
	}
	for _, p := range pages {
		p.Default = true
	}
	return pages
}

// MergeDefaults appends the default pages whose slug no local page already
// has: a local page with the same slug wins.
func MergeDefaults(local, defaults []*Page) []*Page {
	if len(defaults) == 0 {
		return local
	}
	seen := make(map[string]bool, len(local))
	for _, p := range local {
		seen[p.Slug] = true
	}
	out := local
	for _, d := range defaults {
		if !seen[d.Slug] {
			out = append(out, d)
		}
	}
	return out
}

// WithDefaults is MergeDefaults over DefaultPages.
func WithDefaults(local []*Page) []*Page { return MergeDefaults(local, DefaultPages()) }
