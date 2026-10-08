package git

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// This file implements `stash push/pop/list` with real snapshot semantics:
// push records the tracked worktree changes (full contents, binary-safe)
// plus the HEAD base, then restores the worktree to HEAD; pop re-applies
// file-by-file with an overlap check and drops the entry on success.
//
// Storage is a JSON stack under the MAIN .git dir
// (<gitdir>/bashy-stash/stack.json) — one stack per repo, like refs/stash.
// Known limitation, stated here and in GAPS.md: host git does not see our
// entries and we do not see host git's stash entries; the two stacks are
// independent. Index nuance is not preserved (pop stages what it restores;
// --index/--staged are rejected loudly).

// stashFile is one snapshotted path.
type stashFile struct {
	Path    string `json:"path"`
	Deleted bool   `json:"deleted"`
	Content string `json:"content"` // base64 worktree bytes
}

// stashEntry is one stack item.
type stashEntry struct {
	ID      string      `json:"id"`
	Base    string      `json:"base"` // HEAD hash at push time
	Message string      `json:"message"`
	Created time.Time   `json:"created"`
	Files   []stashFile `json:"files"`
}

func stashStackPath(mainGit string) string {
	return filepath.Join(mainGit, "bashy-stash", "stack.json")
}

func loadStashStack(mainGit string) ([]stashEntry, error) {
	raw, err := os.ReadFile(stashStackPath(mainGit))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []stashEntry
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out, nil
}

func saveStashStack(mainGit string, stack []stashEntry) error {
	if err := os.MkdirAll(filepath.Join(mainGit, "bashy-stash"), 0o755); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(stack, "", "  ")
	if err != nil {
		return err
	}
	tmp := stashStackPath(mainGit) + ".tmp"
	if err := os.WriteFile(tmp, append(raw, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, stashStackPath(mainGit))
}

func nativeStash(_ context.Context, dir string, args []string) (*ExecResult, error) {
	sub := "push"
	rest := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		sub, rest = args[0], args[1:]
	}
	switch sub {
	case "push":
		return stashPush(dir, rest)
	case "pop":
		return stashPop(dir, rest)
	case "list":
		if len(rest) != 0 {
			return nil, ErrUnsupported
		}
		return stashList(dir)
	default:
		// drop, apply, show, branch, clear, store, create: not implemented.
		return nil, ErrUnsupported
	}
}

// changedTrackedFiles returns worktree-relative paths with staged or
// unstaged changes, excluding untracked files (like default git stash).
func changedTrackedFiles(w *gogit.Worktree) ([]string, error) {
	st, err := w.Status()
	if err != nil {
		return nil, err
	}
	var out []string
	for path, fs := range st {
		if fs.Staging == gogit.Untracked && fs.Worktree == gogit.Untracked {
			continue
		}
		if fs.Staging == gogit.Unmodified && fs.Worktree == gogit.Unmodified {
			continue
		}
		out = append(out, path)
	}
	sort.Strings(out)
	return out, nil
}

// shelterUntracked snapshots untracked regular files so a hard reset
// (which deletes them — a go-git divergence from host git that real
// stash must not inherit) can be followed by an exact restore.
func shelterUntracked(w *gogit.Worktree) (map[string][]byte, error) {
	st, err := w.Status()
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	root := w.Filesystem.Root()
	for path, fs := range st {
		if fs.Staging != gogit.Untracked || fs.Worktree != gogit.Untracked {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			if os.IsNotExist(err) {
				continue
			}
			return nil, err
		}
		out[path] = raw
	}
	return out, nil
}

func unshelterUntracked(root string, shelter map[string][]byte) error {
	for path, raw := range shelter {
		full := filepath.Join(root, filepath.FromSlash(path))
		if _, err := os.Stat(full); err == nil {
			continue // reset kept it; never overwrite
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(full, raw, 0o644); err != nil {
			return err
		}
	}
	return nil
}

// unindexPath removes path from the index, whether or not it exists on disk.
func unindexPath(repo *gogit.Repository, path string) error {
	idx, err := repo.Storer.Index()
	if err != nil {
		return err
	}
	_, _ = idx.Remove(path)
	ist, ok := repo.Storer.(storer.IndexStorer)
	if !ok {
		return fmt.Errorf("stash: index not writable")
	}
	return ist.SetIndex(idx)
}

// headFileContent returns the HEAD blob bytes for path, or nil when path
// is not in HEAD.
func headFileContent(head *object.Commit, path string) ([]byte, error) {
	f, err := head.File(path)
	if err != nil {
		return nil, nil
	}
	s, err := f.Contents()
	if err != nil {
		return nil, err
	}
	return []byte(s), nil
}

func stashPush(dir string, args []string) (*ExecResult, error) {
	message := ""
	var pathspecs []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "-m" || args[i] == "--message":
			if i+1 >= len(args) {
				return nil, ErrUnsupported
			}
			i++
			message = args[i]
		case args[i] == "--":
			// Everything after -- is a pathspec; it must be last-flag.
			for _, p := range args[i+1:] {
				pathspecs = append(pathspecs, filepath.ToSlash(filepath.Clean(p)))
			}
			i = len(args)
		case args[i] == "--index" || args[i] == "--staged" || args[i] == "-u" || args[i] == "--include-untracked":
			return nil, ErrUnsupported
		case strings.HasPrefix(args[i], "-"):
			return nil, ErrUnsupported
		default:
			pathspecs = append(pathspecs, filepath.ToSlash(filepath.Clean(args[i])))
		}
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	_, mainGit, err := mainGitDir(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	w, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}
	headRef, herr := repo.Head()
	if herr != nil {
		return nil, ErrUnsupported
	}
	head, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, ErrUnsupported
	}

	changed, err := changedTrackedFiles(w)
	if err != nil {
		return nil, ErrUnsupported
	}
	if len(pathspecs) > 0 {
		known := map[string]bool{}
		for _, c := range changed {
			known[c] = true
		}
		for _, p := range pathspecs {
			if !known[p] {
				// Real git errors on unmatched pathspecs; without a
				// matching tracked change there is nothing real to do.
				return &ExecResult{Stderr: fmt.Sprintf("error: pathspec '%s' did not match any changed tracked file(s)\n", p), ExitCode: 1}, nil
			}
		}
		changed = pathspecs
	}
	if len(changed) == 0 {
		return &ExecResult{Stdout: "No local changes to save\n"}, nil
	}

	root := w.Filesystem.Root()
	entry := stashEntry{Base: head.Hash.String(), Created: time.Now()}
	for _, p := range changed {
		full := filepath.Join(root, filepath.FromSlash(p))
		raw, rerr := os.ReadFile(full)
		if rerr != nil {
			if os.IsNotExist(rerr) {
				entry.Files = append(entry.Files, stashFile{Path: p, Deleted: true})
				continue
			}
			return nil, ErrUnsupported
		}
		entry.Files = append(entry.Files, stashFile{Path: p, Content: base64.StdEncoding.EncodeToString(raw)})
	}
	if message == "" {
		branch := "(no branch)"
		if headRef.Name().IsBranch() {
			branch = headRef.Name().Short()
		}
		subj, _, _ := strings.Cut(strings.TrimSpace(head.Message), "\n")
		message = fmt.Sprintf("WIP on %s: %s %s", branch, shortHash(head.Hash), subj)
	}
	entry.Message = message
	sum := sha256.Sum256([]byte(entry.Base + entry.Created.UTC().Format(time.RFC3339Nano) + message))
	entry.ID = fmt.Sprintf("%x", sum)[:12]

	stack, err := loadStashStack(mainGit)
	if err != nil {
		return nil, ErrUnsupported
	}
	stack = append(stack, entry)
	if err := saveStashStack(mainGit, stack); err != nil {
		return nil, ErrUnsupported
	}

	// Restore the stashed paths to HEAD. A full push resets everything;
	// a pathspec push restores just those files (new files leave the
	// worktree, HEAD-tracked files regain their HEAD bytes).
	if len(pathspecs) == 0 {
		// The typed hard reset also deletes untracked files (go-git);
		// real stash keeps them, so shelter them across the reset.
		shelter, serr := shelterUntracked(w)
		if serr != nil {
			return nil, ErrUnsupported
		}
		if _, rerr := Reset(ResetOpts{RepoPath: dir, Mode: ResetHard}); rerr != nil {
			return nil, ErrUnsupported
		}
		if serr := unshelterUntracked(root, shelter); serr != nil {
			return nil, ErrUnsupported
		}
	} else {
		for _, p := range changed {
			want, herr := headFileContent(head, p)
			if herr != nil {
				return nil, ErrUnsupported
			}
			full := filepath.Join(root, filepath.FromSlash(p))
			if want == nil {
				if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
					return nil, ErrUnsupported
				}
				if err := unindexPath(repo, p); err != nil {
					return nil, ErrUnsupported
				}
				continue
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				return nil, ErrUnsupported
			}
			if err := os.WriteFile(full, want, 0o644); err != nil {
				return nil, ErrUnsupported
			}
			if _, err := w.Add(p); err != nil {
				return nil, ErrUnsupported
			}
		}
	}
	return &ExecResult{Stdout: fmt.Sprintf("Saved working directory and index state %s\n", message)}, nil
}

// parseStashRef parses "" (latest) or "stash@{n}".
func parseStashRef(args []string) (int, error) {
	if len(args) == 0 {
		return 0, nil
	}
	if len(args) != 1 {
		return 0, fmt.Errorf("bad ref")
	}
	s := args[0]
	if rest, ok := strings.CutPrefix(s, "stash@{"); ok {
		if !strings.HasSuffix(rest, "}") {
			return 0, fmt.Errorf("bad ref")
		}
		n, err := strconv.Atoi(strings.TrimSuffix(rest, "}"))
		if err != nil || n < 0 {
			return 0, fmt.Errorf("bad ref")
		}
		return n, nil
	}
	return 0, fmt.Errorf("bad ref")
}

func stashPop(dir string, args []string) (*ExecResult, error) {
	for _, a := range args {
		if strings.HasPrefix(a, "-") {
			return nil, ErrUnsupported
		}
	}
	n, err := parseStashRef(args)
	if err != nil {
		return nil, ErrUnsupported
	}
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	_, mainGit, err := mainGitDir(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	stack, err := loadStashStack(mainGit)
	if err != nil {
		return nil, ErrUnsupported
	}
	if n >= len(stack) {
		return &ExecResult{Stderr: fmt.Sprintf("error: log for 'stash' only has %d entries\n", len(stack)), ExitCode: 1}, nil
	}
	// Stack file is oldest-first; stash@{0} is the latest.
	entry := stack[len(stack)-1-n]

	w, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}
	headRef, herr := repo.Head()
	if herr != nil {
		return nil, ErrUnsupported
	}
	head, err := repo.CommitObject(headRef.Hash())
	if err != nil {
		return nil, ErrUnsupported
	}
	root := w.Filesystem.Root()

	// Two-phase: check every file for overlap first so a conflict
	// applies nothing (real git keeps the worktree usable + the entry).
	var conflicts []string
	for _, f := range entry.Files {
		full := filepath.Join(root, filepath.FromSlash(f.Path))
		current, rerr := os.ReadFile(full)
		currentMissing := os.IsNotExist(rerr)
		if rerr != nil && !currentMissing {
			return nil, ErrUnsupported
		}
		headBytes, herr := headFileContent(head, f.Path)
		if herr != nil {
			return nil, ErrUnsupported
		}
		var stashed []byte
		if !f.Deleted {
			stashed, err = base64.StdEncoding.DecodeString(f.Content)
			if err != nil {
				return nil, ErrUnsupported
			}
		}
		pristine := currentMissing && headBytes == nil ||
			!currentMissing && headBytes != nil && string(current) == string(headBytes)
		already := f.Deleted && currentMissing ||
			!f.Deleted && !currentMissing && string(current) == string(stashed)
		if !pristine && !already {
			conflicts = append(conflicts, f.Path)
		}
	}
	if len(conflicts) > 0 {
		var b strings.Builder
		b.WriteString("error: Your local changes to the following files would be overwritten by merge:\n")
		for _, c := range conflicts {
			b.WriteString("\t" + c + "\n")
		}
		b.WriteString("Please commit your changes or stash them before you merge.\nAborting\n")
		return &ExecResult{Stderr: b.String(), ExitCode: 1}, nil
	}

	for _, f := range entry.Files {
		full := filepath.Join(root, filepath.FromSlash(f.Path))
		if f.Deleted {
			if err := os.Remove(full); err != nil && !os.IsNotExist(err) {
				return nil, ErrUnsupported
			}
			if err := unindexPath(repo, f.Path); err != nil {
				return nil, ErrUnsupported
			}
			continue
		}
		stashed, _ := base64.StdEncoding.DecodeString(f.Content)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, ErrUnsupported
		}
		if err := os.WriteFile(full, stashed, 0o644); err != nil {
			return nil, ErrUnsupported
		}
		if _, err := w.Add(f.Path); err != nil {
			return nil, ErrUnsupported
		}
	}

	stack = append(stack[:len(stack)-1-n], stack[len(stack)-n:]...)
	if err := saveStashStack(mainGit, stack); err != nil {
		return nil, ErrUnsupported
	}
	ref := "stash@{0}"
	if n > 0 {
		ref = fmt.Sprintf("stash@{%d}", n)
	}
	return &ExecResult{Stdout: fmt.Sprintf("Dropped %s (%s)\n", ref, entry.ID)}, nil
}

func stashList(dir string) (*ExecResult, error) {
	_, mainGit, err := mainGitDir(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	stack, err := loadStashStack(mainGit)
	if err != nil {
		return nil, ErrUnsupported
	}
	var b strings.Builder
	for i := len(stack) - 1; i >= 0; i-- {
		fmt.Fprintf(&b, "stash@{%d}: %s\n", len(stack)-1-i, stack[i].Message)
	}
	return &ExecResult{Stdout: b.String()}, nil
}

