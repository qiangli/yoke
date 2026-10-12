package git

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/go-git/go-billy/v5/osfs"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/cache"
	"github.com/go-git/go-git/v5/storage/filesystem"
	"github.com/go-git/go-git/v5/storage/filesystem/dotgit"
)

// openLinked opens the repository whose worktree contains dir (searching
// upward when detect is set), resolving a linked worktree's commondir
// itself. go-git's EnableDotGitCommonDir reads commondir without closing
// the file (v5.19 dotGitCommonDirectory): a descriptor leak on Unix, and
// on Windows the open handle makes `worktree remove` fail to delete the
// admin directory.
func openLinked(dir string, detect bool) (*gogit.Repository, error) {
	root, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		if _, err := os.Lstat(filepath.Join(root, ".git")); err == nil {
			break
		}
		parent := filepath.Dir(root)
		if !detect || parent == root {
			return gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{DetectDotGit: detect})
		}
		root = parent
	}
	gitDir, err := resolveGitDir(root)
	if err != nil {
		return gogit.PlainOpenWithOptions(root, &gogit.PlainOpenOptions{})
	}
	data, err := os.ReadFile(filepath.Join(gitDir, "commondir"))
	if err != nil {
		// Not a linked worktree: go-git's own open has nothing to leak.
		return gogit.PlainOpenWithOptions(root, &gogit.PlainOpenOptions{})
	}
	common := strings.TrimSpace(string(data))
	if !filepath.IsAbs(common) {
		common = filepath.Join(gitDir, common)
	}
	if _, err := os.Stat(common); err != nil {
		return nil, gogit.ErrRepositoryIncomplete
	}
	fs := dotgit.NewRepositoryFilesystem(osfs.New(gitDir), osfs.New(common))
	return gogit.Open(filesystem.NewStorage(fs, cache.NewObjectLRUDefault()), osfs.New(root))
}
