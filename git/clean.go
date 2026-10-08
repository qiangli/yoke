package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	gogit "github.com/go-git/go-git/v5"
)

// This file implements `clean -fd/-n`: removal of untracked files and
// directories (workspace hygiene). Tracked content is never touched; only
// status-untracked paths are candidates. Embedded repositories (a directory
// containing .git) are skipped with a note, like host git without -ff.
// .gitignore handling rides on go-git status (ignored paths are not
// reported untracked — pinned by test).

func nativeClean(_ context.Context, dir string, args []string) (*ExecResult, error) {
	force := false
	dryRun := false
	includeDirs := false
	quiet := false
	var pathspecs []string
	dashDash := false
	for _, a := range args {
		// Single-dash bundles (-fd, -dn) expand letter by letter.
		// A lone "-" is never silently swallowed.
		if a == "-" {
			return nil, ErrUnsupported
		}
		if !dashDash && strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") {
			for _, c := range a[1:] {
				switch c {
				case 'f':
					force = true
				case 'n':
					dryRun = true
				case 'd':
					includeDirs = true
				case 'q':
					quiet = true
				default:
					// -x/-X/-e/-i and everything else: loud, never guessed.
					return nil, ErrUnsupported
				}
			}
			continue
		}
		switch {
		case a == "--":
			dashDash = true
		case a == "--force":
			force = true
		case a == "--dry-run":
			dryRun = true
		case a == "--quiet":
			quiet = true
		case strings.HasPrefix(a, "-"):
			return nil, ErrUnsupported
		default:
			pathspecs = append(pathspecs, filepath.ToSlash(filepath.Clean(a)))
		}
	}
	if !force && !dryRun {
		return &ExecResult{
			Stderr:   "fatal: clean.requireForce defaults to true and neither -i, -n, nor -f given; refusing to clean\n",
			ExitCode: 128,
		}, nil
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	w, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}
	root := w.Filesystem.Root()
	st, err := w.Status()
	if err != nil {
		return nil, ErrUnsupported
	}

	// Top-level dirs holding tracked files must never collapse: removing
	// them whole would take tracked content with the untracked.
	trackedDirs := map[string]bool{}
	if idx, ierr := repo.Storer.Index(); ierr == nil {
		for _, e := range idx.Entries {
			if top := topDir(e.Name); top != "" {
				trackedDirs[top] = true
			}
		}
	}

	// Collect untracked paths. Without -d, host git never recurses into
	// untracked directories: only top-level files (or files under tracked
	// dirs) are candidates. With -d, untracked top dirs collapse to one
	// unit — unless they hold tracked files (see trackedDirs above).
	var files []string
	dirs := map[string]bool{}
	for path, fs := range st {
		if fs.Staging != gogit.Untracked || fs.Worktree != gogit.Untracked {
			continue
		}
		if len(pathspecs) > 0 && !pathspecMatch(pathspecs, path) {
			continue
		}
		top := topDir(path)
		if !includeDirs {
			if top != "" && !trackedDirs[top] {
				continue
			}
			files = append(files, path)
			continue
		}
		if top != "" && !trackedDirs[top] {
			dirs[top] = true
			continue
		}
		files = append(files, path)
	}
	var targets []string
	for d := range dirs {
		targets = append(targets, d+"/")
	}
	for _, f := range files {
		if top := topDir(f); top != "" && dirs[top] {
			continue
		}
		targets = append(targets, f)
	}
	sort.Strings(targets)

	// Embedded repositories are invisible to go-git status, so the
	// removal loop below would never even consider them. Host git names
	// the refusal explicitly under -d; mirror that note for top-level
	// dirs that hold a .git and no tracked files.
	if includeDirs && !quiet {
		if topEntries, rerr := os.ReadDir(root); rerr == nil {
			for _, e := range topEntries {
				name := e.Name()
				if !e.IsDir() || name == ".git" || trackedDirs[name] || dirs[name] {
					continue
				}
				if _, serr := os.Stat(filepath.Join(root, name, ".git")); serr == nil {
					targets = append(targets, "Skipping repository "+name)
				}
			}
		}
		sort.Strings(targets)
	}

	var b strings.Builder
	verb := "Removing"
	if dryRun {
		verb = "Would remove"
	}
	for _, t := range targets {
		// A refusal note from the embedded-repo scan above.
		if name, ok := strings.CutPrefix(t, "Skipping repository "); ok {
			if dryRun {
				b.WriteString(fmt.Sprintf("Would skip repository %s\n", name))
			} else {
				b.WriteString(fmt.Sprintf("Skipping repository %s\n", name))
			}
			continue
		}
		isDir := strings.HasSuffix(t, "/")
		rel := strings.TrimSuffix(t, "/")
		full := filepath.Join(root, filepath.FromSlash(rel))
		if isDir && !dryRun {
			// Never descend into embedded repositories.
			if _, serr := os.Stat(filepath.Join(full, ".git")); serr == nil {
				if !quiet {
					b.WriteString(fmt.Sprintf("Skipping repository %s\n", rel))
				}
				continue
			}
		}
		if !quiet {
			b.WriteString(fmt.Sprintf("%s %s\n", verb, rel))
		}
		if dryRun {
			continue
		}
		if isDir {
			if err := os.RemoveAll(full); err != nil {
				return nil, ErrUnsupported
			}
			continue
		}
		if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
			return nil, ErrUnsupported
		}
	}
	return &ExecResult{Stdout: b.String()}, nil
}

// topDir returns the top-level directory of a slash path, or "" when the
// path is top-level itself.
func topDir(path string) string {
	if i := strings.Index(path, "/"); i >= 0 {
		return path[:i]
	}
	return ""
}

// pathspecMatch reports whether path falls under any of the specs (exact
// file or directory prefix).
func pathspecMatch(specs []string, path string) bool {
	for _, s := range specs {
		if path == s || strings.HasPrefix(path, s+"/") {
			return true
		}
	}
	return false
}
