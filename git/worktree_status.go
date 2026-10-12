package git

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/gitignore"
)

// worktreeStatus is Worktree.Status with two divergences from host git
// closed; every native verb that reads status goes through it.
//
//   - Exclude files: go-git's worktree filesystem cannot open
//     .git/info/exclude (and never reads core.excludesFile), so excluded
//     files surfaced as untracked — and native `git clean -f` deleted them.
//     Those patterns are fed in through Worktree.Excludes.
//   - Submodules: go-git compares a gitlink only for submodules registered
//     in .git/config; an unregistered checkout gets its expected hash, so a
//     moved pin was invisible. Host git inspects any checkout at the path.
func worktreeStatus(w *gogit.Worktree) (gogit.Status, error) {
	root := w.Filesystem.Root()
	saved := w.Excludes
	w.Excludes = append(append([]gitignore.Pattern(nil), saved...), excludePatterns(root)...)
	st, err := w.Status()
	w.Excludes = saved
	if err != nil {
		return nil, err
	}
	markMovedSubmodules(root, st)
	if repo, err := openRepo(root); err == nil {
		mode := coreAutoCRLF(repo)
		autocrlf, fileMode := mode == "true" || mode == "input", coreFileMode(repo)
		if autocrlf || !fileMode {
			dropConfigClean(repo, root, st, autocrlf, fileMode)
		}
	}
	return st, nil
}

// excludePatterns returns core.excludesFile then $GIT_COMMON_DIR/info/exclude
// patterns (ascending priority, like host git).
func excludePatterns(root string) []gitignore.Pattern {
	var ps []gitignore.Pattern
	if global, err := gitignore.LoadGlobalPatterns(osfs.New(string(filepath.Separator))); err == nil {
		ps = append(ps, global...)
	}
	gitDir, err := resolveGitDir(root)
	if err != nil {
		return ps
	}
	common := gitDir
	if data, err := os.ReadFile(filepath.Join(gitDir, "commondir")); err == nil {
		if c := strings.TrimSpace(string(data)); filepath.IsAbs(c) {
			common = c
		} else {
			common = filepath.Join(gitDir, c)
		}
	}
	f, err := os.Open(filepath.Join(common, "info", "exclude"))
	if err != nil {
		return ps
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
			continue
		}
		ps = append(ps, gitignore.ParsePattern(line, nil))
	}
	return ps
}

// markMovedSubmodules flags every gitlink whose checked-out HEAD differs
// from the index as worktree-modified (" M path"). A path without a
// checkout (no .git) is left alone, as host git does.
func markMovedSubmodules(root string, st gogit.Status) {
	repo, err := openRepo(root)
	if err != nil {
		return
	}
	idx, err := repo.Storer.Index()
	if err != nil {
		return
	}
	for _, e := range idx.Entries {
		if e.Mode != filemode.Submodule {
			continue
		}
		subDir := filepath.Join(root, filepath.FromSlash(e.Name))
		if _, err := os.Stat(filepath.Join(subDir, ".git")); err != nil {
			continue
		}
		sub, err := openCheckout(subDir)
		if err != nil {
			continue
		}
		head, err := sub.Head()
		if err != nil || head.Hash() == e.Hash {
			continue
		}
		fs, ok := st[e.Name]
		if !ok {
			fs = &gogit.FileStatus{Staging: gogit.Unmodified}
			st[e.Name] = fs
		}
		fs.Worktree = gogit.Modified
	}
}

// openCheckout opens the repository whose worktree root is exactly dir
// (no upward search — a submodule path without a checkout must not
// resolve to the superproject), alternates-aware like openRepo.
func openCheckout(dir string) (*gogit.Repository, error) {
	r, err := gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{EnableDotGitCommonDir: true})
	if err != nil {
		return nil, err
	}
	return withAlternates(r), nil
}
