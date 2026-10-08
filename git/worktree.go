package git

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// This file implements `worktree add/remove/list` over real gitfile linked
// worktrees (the same layout host git uses), so checkouts created here are
// interoperable with host git and vice versa:
//
//	<wt>/.git                          -> "gitdir: <maindir>/worktrees/<name>\n"
//	<maindir>/worktrees/<name>/{HEAD,commondir,gitdir,index}
//
// commondir holds "../.." (the shared object store); gitdir holds the
// worktree .git file path; HEAD is "ref: ..." or a detached hash; index is
// a v2 index written with go-git's encoder so status reads clean. Branch
// double-checkout is refused without -f by scanning main + admin HEADs.

// mainGitDir resolves dir to its worktree root and main .git dir, following
// a ".git" gitfile (linked worktree or submodule-style) through commondir.
// Returns (workRoot, mainGitDir).
func mainGitDir(dir string) (string, string, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", "", err
	}
	dotgit := filepath.Join(abs, ".git")
	fi, err := os.Stat(dotgit)
	if err != nil {
		return "", "", err
	}
	if fi.IsDir() {
		return abs, dotgit, nil
	}
	raw, err := os.ReadFile(dotgit)
	if err != nil {
		return "", "", err
	}
	line := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(line, "gitdir: ") {
		return "", "", fmt.Errorf("worktree: malformed .git file")
	}
	admin := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(admin) {
		admin = filepath.Join(abs, admin)
	}
	commRaw, err := os.ReadFile(filepath.Join(admin, "commondir"))
	if err != nil {
		return "", "", fmt.Errorf("worktree: cannot read commondir: %w", err)
	}
	comm := strings.TrimSpace(string(commRaw))
	if !filepath.IsAbs(comm) {
		comm = filepath.Join(admin, comm)
	}
	return abs, comm, nil
}

func nativeWorktree(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return worktreeList(dir, false)
	}
	switch args[0] {
	case "add":
		return worktreeAdd(dir, args[1:])
	case "remove":
		return worktreeRemove(dir, args[1:])
	case "list":
		porcelain := false
		for _, a := range args[1:] {
			if a == "--porcelain" {
				porcelain = true
			} else {
				return nil, ErrUnsupported
			}
		}
		return worktreeList(dir, porcelain)
	default:
		if strings.HasPrefix(args[0], "-") {
			return nil, ErrUnsupported
		}
		return nil, ErrUnsupported
	}
}

// worktreeAdmins lists linked-worktree admin dirs under mainGit, sorted.
func worktreeAdmins(mainGit string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(mainGit, "worktrees"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out, nil
}

// resolveMainRef resolves refName against a main .git dir's loose refs
// (falling back to packed-refs), without opening a worktree.
func resolveMainRef(mainGit, refName string) (plumbing.Hash, error) {
	raw, err := os.ReadFile(filepath.Join(mainGit, filepath.FromSlash(refName)))
	if err == nil {
		if h := plumbing.NewHash(strings.TrimSpace(string(raw))); !h.IsZero() {
			return h, nil
		}
	}
	packed, err := os.ReadFile(filepath.Join(mainGit, "packed-refs"))
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf("worktree: cannot resolve %s", refName)
	}
	for _, line := range strings.Split(string(packed), "\n") {
		if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "^") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[1] == refName {
			return plumbing.NewHash(fields[0]), nil
		}
	}
	return plumbing.ZeroHash, fmt.Errorf("worktree: cannot resolve %s", refName)
}

// mainHEAD reads the main repo's HEAD: ref name ("" when detached) + hash.
func mainHEAD(mainGit string) (string, plumbing.Hash, error) {
	raw, err := os.ReadFile(filepath.Join(mainGit, "HEAD"))
	if err != nil {
		return "", plumbing.ZeroHash, err
	}
	head := strings.TrimSpace(string(raw))
	if rest, ok := strings.CutPrefix(head, "ref: "); ok {
		h, err := resolveMainRef(mainGit, rest)
		if err != nil {
			return "", plumbing.ZeroHash, err
		}
		return rest, h, nil
	}
	return "", plumbing.NewHash(head), nil
}

// worktreeAdminHEAD returns the admin HEAD: ref name ("" when detached),
// hash, and the worktree path recorded in the admin gitdir file.
func worktreeAdminHEAD(mainGit, name string) (ref string, hash plumbing.Hash, wtPath string, err error) {
	admin := filepath.Join(mainGit, "worktrees", name)
	raw, err := os.ReadFile(filepath.Join(admin, "HEAD"))
	if err != nil {
		return "", plumbing.ZeroHash, "", err
	}
	head := strings.TrimSpace(string(raw))
	if rest, ok := strings.CutPrefix(head, "ref: "); ok {
		ref = rest
		h, herr := resolveMainRef(mainGit, rest)
		if herr != nil {
			return "", plumbing.ZeroHash, "", herr
		}
		hash = h
	} else {
		hash = plumbing.NewHash(head)
	}
	gd, err := os.ReadFile(filepath.Join(admin, "gitdir"))
	if err != nil {
		return "", plumbing.ZeroHash, "", err
	}
	wtPath = filepath.Dir(strings.TrimSpace(string(gd)))
	return ref, hash, wtPath, nil
}

func worktreeAdd(dir string, args []string) (*ExecResult, error) {
	force := false
	var positionals []string
	for _, a := range args {
		switch a {
		case "-f", "--force":
			force = true
		default:
			if strings.HasPrefix(a, "-") {
				return nil, ErrUnsupported
			}
			positionals = append(positionals, a)
		}
	}
	if len(positionals) < 1 || len(positionals) > 2 {
		return nil, ErrUnsupported
	}
	wtPath := positionals[0]
	if !filepath.IsAbs(wtPath) {
		wtPath = filepath.Join(dir, wtPath)
	}
	commitRev := "HEAD"
	if len(positionals) == 2 {
		commitRev = positionals[1]
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	_, mainGit, err := mainGitDir(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Resolve the target: an existing branch checks out that branch,
	// anything else must resolve to a commit (detached HEAD).
	branch := ""
	var target *object.Commit
	if h, rerr := repo.ResolveRevision(plumbing.Revision(plumbing.NewBranchReferenceName(commitRev))); rerr == nil {
		if c, cerr := repo.CommitObject(*h); cerr == nil {
			branch = commitRev
			target = c
		}
	}
	if target == nil {
		h, rerr := repo.ResolveRevision(plumbing.Revision(commitRev))
		if rerr != nil {
			return &ExecResult{Stderr: fmt.Sprintf("fatal: invalid reference: %s\n", commitRev), ExitCode: 128}, nil
		}
		c, cerr := repo.CommitObject(*h)
		if cerr != nil {
			return &ExecResult{Stderr: fmt.Sprintf("fatal: invalid reference: %s\n", commitRev), ExitCode: 128}, nil
		}
		target = c
	}

	// Refuse a branch already checked out elsewhere without -f: the
	// main worktree's HEAD plus every linked worktree's admin HEAD.
	if branch != "" && !force {
		want := plumbing.NewBranchReferenceName(branch).String()
		if ref, _, merr := mainHEAD(mainGit); merr == nil && ref == want {
			workRoot, _, _ := mainGitDir(dir)
			return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' is already used by worktree at '%s'\n", commitRev, workRoot), ExitCode: 128}, nil
		}
		admins, _ := worktreeAdmins(mainGit)
		for _, name := range admins {
			ref, _, wp, aerr := worktreeAdminHEAD(mainGit, name)
			if aerr != nil {
				continue
			}
			if ref == want {
				return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' is already used by worktree at '%s'\n", commitRev, wp), ExitCode: 128}, nil
			}
		}
	}

	// The path must be absent or an empty directory.
	if fi, serr := os.Stat(wtPath); serr == nil {
		if !fi.IsDir() {
			return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' already exists\n", positionals[0]), ExitCode: 128}, nil
		}
		entries, rerr := os.ReadDir(wtPath)
		if rerr != nil || len(entries) > 0 {
			return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' already exists\n", positionals[0]), ExitCode: 128}, nil
		}
	} else if err := os.MkdirAll(wtPath, 0o755); err != nil {
		return nil, ErrUnsupported
	}

	name := filepath.Base(wtPath)
	admin := filepath.Join(mainGit, "worktrees", name)
	if _, serr := os.Stat(admin); serr == nil {
		return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' already exists\n", name), ExitCode: 128}, nil
	}
	if err := os.MkdirAll(admin, 0o755); err != nil {
		return nil, ErrUnsupported
	}
	cleanup := func() {
		os.RemoveAll(admin)
		os.RemoveAll(wtPath)
	}

	// Populate the worktree from the target tree.
	tree, err := target.Tree()
	if err != nil {
		cleanup()
		return nil, ErrUnsupported
	}
	idx := &index.Index{Version: 2}
	failed := false
	_ = tree.Files().ForEach(func(f *object.File) error {
		dest := filepath.Join(wtPath, filepath.FromSlash(f.Name))
		contents, cerr := f.Contents()
		if cerr != nil {
			failed = true
			return cerr
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
			failed = true
			return err
		}
		if f.Mode == filemode.Symlink {
			if err := os.Symlink(contents, dest); err != nil {
				failed = true
				return err
			}
		} else {
			mode := os.FileMode(0o644)
			if f.Mode == filemode.Executable {
				mode = os.FileMode(0o755)
			}
			if err := os.WriteFile(dest, []byte(contents), mode); err != nil {
				failed = true
				return err
			}
		}
		idx.Entries = append(idx.Entries, &index.Entry{
			Name:       f.Name,
			Hash:       f.Blob.Hash,
			Mode:       f.Mode,
			CreatedAt:  time.Now(),
			ModifiedAt: time.Now(),
		})
		return nil
	})
	if failed {
		cleanup()
		return nil, ErrUnsupported
	}
	idxFile, err := os.Create(filepath.Join(admin, "index"))
	if err != nil {
		cleanup()
		return nil, ErrUnsupported
	}
	if err := index.NewEncoder(idxFile).Encode(idx); err != nil {
		idxFile.Close()
		cleanup()
		return nil, ErrUnsupported
	}
	idxFile.Close()

	head := target.Hash.String() + "\n"
	if branch != "" {
		head = "ref: " + plumbing.NewBranchReferenceName(branch).String() + "\n"
	}
	for file, content := range map[string]string{
		"HEAD":      head,
		"commondir": "../..\n",
		"gitdir":    filepath.Join(wtPath, ".git") + "\n",
	} {
		if err := os.WriteFile(filepath.Join(admin, file), []byte(content), 0o644); err != nil {
			cleanup()
			return nil, ErrUnsupported
		}
	}
	if err := os.WriteFile(filepath.Join(wtPath, ".git"), []byte("gitdir: "+admin+"\n"), 0o644); err != nil {
		cleanup()
		return nil, ErrUnsupported
	}
	return &ExecResult{Stdout: ""}, nil
}

func worktreeRemove(dir string, args []string) (*ExecResult, error) {
	force := false
	var positionals []string
	for _, a := range args {
		switch a {
		case "--force":
			force = true
		default:
			if strings.HasPrefix(a, "-") {
				return nil, ErrUnsupported
			}
			positionals = append(positionals, a)
		}
	}
	if len(positionals) != 1 {
		return nil, ErrUnsupported
	}
	wtPath := positionals[0]
	if !filepath.IsAbs(wtPath) {
		wtPath = filepath.Join(dir, wtPath)
	}

	// The path must be a registered linked worktree: its .git file must
	// point into a main repo's worktrees admin dir.
	dotgit, err := os.ReadFile(filepath.Join(wtPath, ".git"))
	if err != nil {
		return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' is not a working tree\n", positionals[0]), ExitCode: 128}, nil
	}
	line := strings.TrimSpace(string(dotgit))
	if !strings.HasPrefix(line, "gitdir: ") {
		return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' is a main working tree\n", positionals[0]), ExitCode: 128}, nil
	}
	admin := strings.TrimPrefix(line, "gitdir: ")
	if !filepath.IsAbs(admin) {
		admin = filepath.Join(wtPath, admin)
	}
	if _, serr := os.Stat(filepath.Join(admin, "HEAD")); serr != nil {
		return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' is not a working tree\n", positionals[0]), ExitCode: 128}, nil
	}

	if !force {
		wr, oerr := openRepo(wtPath)
		if oerr != nil {
			return nil, ErrUnsupported
		}
		w, werr := wr.Worktree()
		if werr != nil {
			return nil, ErrUnsupported
		}
		st, serr := w.Status()
		if serr != nil {
			return nil, ErrUnsupported
		}
		if !st.IsClean() {
			return &ExecResult{Stderr: fmt.Sprintf("fatal: '%s' contains modified or untracked files, use --force to delete it\n", positionals[0]), ExitCode: 128}, nil
		}
	}
	if err := os.RemoveAll(wtPath); err != nil {
		return nil, ErrUnsupported
	}
	if err := os.RemoveAll(admin); err != nil {
		return nil, ErrUnsupported
	}
	return &ExecResult{Stdout: ""}, nil
}

// worktreeEntry is one row of `worktree list`.
type worktreeEntry struct {
	path   string
	hash   string
	branch string // short branch name, "" when detached/bare
	bare   bool
}

func collectWorktrees(dir string) ([]worktreeEntry, error) {
	if _, err := openRepo(dir); err != nil {
		return nil, err
	}
	workRoot, mainGit, err := mainGitDir(dir)
	if err != nil {
		return nil, err
	}
	var out []worktreeEntry
	if ref, hash, merr := mainHEAD(mainGit); merr == nil {
		e := worktreeEntry{path: workRoot, hash: hash.String()}
		if ref != "" {
			e.branch = plumbing.ReferenceName(ref).Short()
		}
		out = append(out, e)
	}
	admins, _ := worktreeAdmins(mainGit)
	for _, name := range admins {
		ref, hash, wp, aerr := worktreeAdminHEAD(mainGit, name)
		if aerr != nil || hash == plumbing.ZeroHash {
			continue
		}
		e := worktreeEntry{path: wp, hash: hash.String()}
		if ref != "" {
			e.branch = plumbing.ReferenceName(ref).Short()
		}
		out = append(out, e)
	}
	return out, nil
}

func worktreeList(dir string, porcelain bool) (*ExecResult, error) {
	entries, err := collectWorktrees(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	var b strings.Builder
	for _, e := range entries {
		if porcelain {
			b.WriteString("worktree " + e.path + "\n")
			b.WriteString("HEAD " + e.hash + "\n")
			if e.branch != "" {
				b.WriteString("branch refs/heads/" + e.branch + "\n")
			} else {
				b.WriteString("detached\n")
			}
			b.WriteString("\n")
		} else {
			b.WriteString(e.path + "  " + e.hash[:7])
			if e.branch != "" {
				b.WriteString(" [" + e.branch + "]")
			} else {
				b.WriteString(" (detached)")
			}
			b.WriteString("\n")
		}
	}
	return &ExecResult{Stdout: b.String()}, nil
}
