package src

import (
	"os"
	"path/filepath"
	"sort"

	ygit "github.com/qiangli/yoke/git"
)

// Violation is one working-tree edit under a core or protected path.
type Violation struct {
	// Repo is the source repo (the $BASHY_HOME/src/<repo> directory name).
	Repo string
	// Path is the repo-relative file path.
	Path string
	// Kind is "core" or "protected".
	Kind string
}

// Check scans every fetched checkout named by the protected manifest and
// reports any working-tree edit (modified, staged, deleted, or untracked
// file) under a core or protected prefix. A clean tree returns no
// violations; repos never fetched are skipped, never an error.
func Check(home string) ([]Violation, error) {
	if home == "" {
		home = Home()
	}
	root := SrcDir(home)
	var out []Violation
	for _, repo := range manifestRepos() {
		dir := filepath.Join(root, repo)
		st, err := os.Stat(dir)
		if err != nil || !st.IsDir() {
			continue
		}
		_, entries, err := ygit.Status(dir)
		if err != nil {
			return nil, err
		}
		for _, e := range entries {
			m := matchProt(repo, e.File)
			if m == nil {
				continue
			}
			out = append(out, Violation{Repo: repo, Path: e.File, Kind: m.Kind()})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Repo != out[j].Repo {
			return out[i].Repo < out[j].Repo
		}
		return out[i].Path < out[j].Path
	})
	return out, nil
}
