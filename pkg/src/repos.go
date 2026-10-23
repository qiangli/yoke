package src

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// allowedRepos is the fetch allowlist: OSS sibling repos by short name to
// their canonical remote. Anything else is refused — proprietary or unknown
// origins never land under $BASHY_HOME/src.
var allowedRepos = map[string]string{
	"yoke":        "https://github.com/qiangli/yoke",
	"coreutils":   "https://github.com/qiangli/coreutils",
	"sh":          "https://github.com/qiangli/sh",
	"filebrowser": "https://github.com/qiangli/filebrowser",
}

// AllowedRepos returns the fetchable OSS repo names, sorted.
func AllowedRepos() []string {
	out := make([]string, 0, len(allowedRepos))
	for n := range allowedRepos {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// remoteFor resolves repo to its canonical remote, refusing anything off
// the allowlist.
func remoteFor(repo string) (string, error) {
	if u, ok := allowedRepos[repo]; ok {
		return u, nil
	}
	return "", fmt.Errorf("src: refusing proprietary or unknown repo %q (fetchable OSS siblings: %s)",
		repo, strings.Join(AllowedRepos(), ", "))
}

// Home returns the bashy home: $BASHY_HOME, defaulting to ~/.bashy/src's
// parent ~/.bashy.
func Home() string {
	if h := strings.TrimSpace(os.Getenv("BASHY_HOME")); h != "" {
		return h
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ".bashy"
	}
	return filepath.Join(home, ".bashy")
}

// SrcDir returns the checkout root: $BASHY_HOME/src (default ~/.bashy/src).
func SrcDir(home string) string {
	if home == "" {
		home = Home()
	}
	return filepath.Join(home, "src")
}

// SeedDir returns the Sprint 386 offline seed root: $BASHY_HOME/seed. When
// it holds <repo> (or <repo>.git), fetch clones from the seed instead of
// the network.
func SeedDir(home string) string {
	if home == "" {
		home = Home()
	}
	return filepath.Join(home, "seed")
}

// seedOrigin returns the seed path for repo when present, or "".
func seedOrigin(home, repo string) string {
	for _, cand := range []string{
		filepath.Join(SeedDir(home), repo),
		filepath.Join(SeedDir(home), repo+".git"),
	} {
		if st, err := os.Stat(cand); err == nil && st.IsDir() {
			return cand
		}
	}
	return ""
}

// Offline reports whether network acquisition is forbidden (BASHY_OFFLINE=1,
// the binmgr convention).
func Offline() bool { return strings.TrimSpace(os.Getenv("BASHY_OFFLINE")) == "1" }
