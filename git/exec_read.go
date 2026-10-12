package git

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/filemode"
	"github.com/go-git/go-git/v5/plumbing/format/index"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
	"github.com/go-git/go-git/v5/storage/filesystem"
)

// openRepo opens the git repository at or above dir. A ".git" FILE
// (linked worktree or submodule-style gitdir pointer) opens with
// go-git's commondir support so objects/refs resolve from the shared
// store; without it every read in a linked worktree fails with
// "object not found". Plain directories are unaffected.
func openRepo(dir string) (*gogit.Repository, error) {
	var (
		r   *gogit.Repository
		err error
	)
	if fi, serr := os.Stat(filepath.Join(dir, ".git")); serr == nil && !fi.IsDir() {
		r, err = gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{
			DetectDotGit:          true,
			EnableDotGitCommonDir: true,
		})
	} else {
		r, err = gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{
			DetectDotGit: true,
		})
	}
	if err != nil {
		return nil, err
	}
	return withAlternates(r), nil
}

// withAlternates reopens r with an unconfined alternates reader when its
// object store borrows objects through objects/info/alternates (clone
// --reference, submodules). Without it go-git cannot see those objects
// and reports every file as a staged addition; see openStatusRepository.
// Linked worktrees (commondir) keep the plain open.
func withAlternates(r *gogit.Repository) *gogit.Repository {
	wt, err := r.Worktree()
	if err != nil {
		return r
	}
	root := wt.Filesystem.Root()
	gitDir, err := resolveGitDir(root)
	if err != nil {
		return r
	}
	if _, err := os.Stat(filepath.Join(gitDir, "commondir")); err == nil {
		return r
	}
	if _, err := os.Stat(filepath.Join(gitDir, "objects", "info", "alternates")); err != nil {
		return r
	}
	if alt, err := openStatusRepository(root); err == nil {
		return alt
	}
	return r
}

// resolveAuthor reads the git user.name/user.email config from the repository.
// Returns nil if the config provides valid author info (letting go-git resolve it),
// or a default signature if no config is available.
func resolveAuthor(repo *gogit.Repository) *object.Signature {
	cfg, err := repo.ConfigScoped(config.GlobalScope)
	if err == nil && cfg.User.Name != "" && cfg.User.Email != "" {
		return nil // go-git will read from config
	}
	// No git config available — provide a default so commits don't fail.
	return &object.Signature{
		Name:  "ycode",
		Email: "ycode@localhost",
		When:  time.Now(),
	}
}

func nativeRevParse(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Identify supported flag shapes first. Unrecognized flags return ErrUnsupported
	// immediately so callers fall back to the host git binary.
	switch {
	case args[0] == "--show-toplevel" && len(args) == 1:
	case args[0] == "--show-prefix" && len(args) == 1:
	case args[0] == "--git-dir" && len(args) == 1:
	case args[0] == "--absolute-git-dir" && len(args) == 1:
	case args[0] == "--is-inside-work-tree" && len(args) == 1:
	case args[0] == "--is-inside-git-dir" && len(args) == 1:
	case args[0] == "--abbrev-ref" && len(args) == 2:
	case (args[0] == "--short" || strings.HasPrefix(args[0], "--short=")) && len(args) <= 2:
	case args[0] == "--verify" || (len(args) > 1 && (args[0] == "-q" || args[0] == "--quiet" || args[1] == "--verify")):
	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
	default:
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return &ExecResult{
			Stderr:   "fatal: not a git repository (or any of the parent directories): .git\n",
			ExitCode: 128,
		}, nil
	}

	absDir, aerr := filepath.Abs(dir)
	if aerr == nil {
		if r, rerr := filepath.EvalSymlinks(absDir); rerr == nil {
			absDir = r
		}
	} else {
		absDir = dir
	}
	absDir = filepath.Clean(absDir)

	var worktreeRoot string
	var wt *gogit.Worktree
	var wtErr error
	if repo != nil {
		wt, wtErr = repo.Worktree()
		if wtErr == nil && wt != nil && wt.Filesystem != nil {
			worktreeRoot = wt.Filesystem.Root()
			if r, rerr := filepath.EvalSymlinks(worktreeRoot); rerr == nil {
				worktreeRoot = r
			}
			worktreeRoot = filepath.Clean(worktreeRoot)
		}
	}

	var gitDir string
	if s, ok := repo.Storer.(*filesystem.Storage); ok && s.Filesystem() != nil {
		gitDir = s.Filesystem().Root()
	} else if wt != nil && wt.Filesystem != nil {
		gitDir = filepath.Join(wt.Filesystem.Root(), ".git")
	}
	if gitDir != "" {
		if r, rerr := filepath.EvalSymlinks(gitDir); rerr == nil {
			gitDir = r
		}
		gitDir = filepath.Clean(gitDir)
	}

	switch {
	case args[0] == "--show-toplevel":
		if wtErr != nil || worktreeRoot == "" {
			return &ExecResult{
				Stderr:   "fatal: this operation must be run in a work tree\n",
				ExitCode: 128,
			}, nil
		}
		return &ExecResult{Stdout: worktreeRoot + "\n"}, nil

	case args[0] == "--show-prefix":
		if wtErr != nil || worktreeRoot == "" {
			return &ExecResult{Stdout: "\n"}, nil
		}
		rel, relErr := filepath.Rel(worktreeRoot, absDir)
		if relErr != nil || rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
			return &ExecResult{Stdout: "\n"}, nil
		}
		prefix := filepath.ToSlash(rel)
		if !strings.HasSuffix(prefix, "/") {
			prefix += "/"
		}
		return &ExecResult{Stdout: prefix + "\n"}, nil

	case args[0] == "--git-dir":
		if absDir == worktreeRoot && filepath.Base(gitDir) == ".git" && filepath.Dir(gitDir) == worktreeRoot {
			return &ExecResult{Stdout: ".git\n"}, nil
		}
		return &ExecResult{Stdout: gitDir + "\n"}, nil

	case args[0] == "--absolute-git-dir":
		return &ExecResult{Stdout: gitDir + "\n"}, nil

	case args[0] == "--is-inside-work-tree":
		if wtErr != nil || worktreeRoot == "" {
			return &ExecResult{Stdout: "false\n"}, nil
		}
		if gitDir != "" && (absDir == gitDir || strings.HasPrefix(absDir, gitDir+string(filepath.Separator))) {
			return &ExecResult{Stdout: "false\n"}, nil
		}
		return &ExecResult{Stdout: "true\n"}, nil

	case args[0] == "--is-inside-git-dir":
		if gitDir != "" && (absDir == gitDir || strings.HasPrefix(absDir, gitDir+string(filepath.Separator))) {
			return &ExecResult{Stdout: "true\n"}, nil
		}
		return &ExecResult{Stdout: "false\n"}, nil

	case args[0] == "--abbrev-ref":
		refName := args[1]
		if refName == "HEAD" {
			head, herr := repo.Head()
			if herr != nil {
				return &ExecResult{Stderr: "fatal: HEAD not found\n", ExitCode: 128}, nil
			}
			if head.Name().IsBranch() {
				return &ExecResult{Stdout: head.Name().Short() + "\n"}, nil
			}
			return &ExecResult{Stdout: "HEAD\n"}, nil
		}
		ref, rerr := repo.Reference(plumbing.ReferenceName(refName), true)
		if rerr == nil {
			return &ExecResult{Stdout: ref.Name().Short() + "\n"}, nil
		}
		return &ExecResult{Stdout: refName + "\n"}, nil

	case args[0] == "--short" || strings.HasPrefix(args[0], "--short="):
		shortLen := 7
		if strings.HasPrefix(args[0], "--short=") {
			nStr := strings.TrimPrefix(args[0], "--short=")
			var n int
			if _, perr := fmt.Sscanf(nStr, "%d", &n); perr != nil || n <= 0 {
				return nil, ErrUnsupported
			}
			shortLen = n
		}
		if len(args) < 2 {
			return &ExecResult{Stderr: "fatal: Needed a single revision\n", ExitCode: 128}, nil
		}
		refName := args[1]
		var hash plumbing.Hash
		if refName == "HEAD" {
			head, herr := repo.Head()
			if herr != nil {
				return &ExecResult{Stderr: "fatal: Needed a single revision\n", ExitCode: 128}, nil
			}
			hash = head.Hash()
		} else {
			h, rerr := repo.ResolveRevision(plumbing.Revision(refName))
			if rerr != nil {
				return &ExecResult{Stderr: "fatal: Needed a single revision\n", ExitCode: 128}, nil
			}
			hash = *h
		}
		hStr := hash.String()
		if len(hStr) > shortLen {
			hStr = hStr[:shortLen]
		}
		return &ExecResult{Stdout: hStr + "\n"}, nil

	case args[0] == "--verify" || (len(args) > 1 && (args[0] == "-q" || args[0] == "--quiet" || args[1] == "--verify")):
		quiet := false
		var refName string
		for _, a := range args {
			switch a {
			case "--verify":
			case "-q", "--quiet":
				quiet = true
			default:
				if strings.HasPrefix(a, "-") {
					return nil, ErrUnsupported
				}
				if refName == "" {
					refName = a
				} else {
					return nil, ErrUnsupported
				}
			}
		}
		if refName == "" {
			return nil, ErrUnsupported
		}
		var hash plumbing.Hash
		if refName == "HEAD" {
			head, herr := repo.Head()
			if herr != nil {
				if quiet {
					return &ExecResult{ExitCode: 1}, nil
				}
				return &ExecResult{Stderr: "fatal: Needed a single revision\n", ExitCode: 128}, nil
			}
			hash = head.Hash()
		} else {
			h, rerr := repo.ResolveRevision(plumbing.Revision(refName))
			if rerr != nil {
				if quiet {
					return &ExecResult{ExitCode: 1}, nil
				}
				return &ExecResult{Stderr: "fatal: Needed a single revision\n", ExitCode: 128}, nil
			}
			hash = *h
		}
		return &ExecResult{Stdout: hash.String() + "\n"}, nil

	case len(args) == 1 && !strings.HasPrefix(args[0], "-"):
		refName := args[0]
		var hash plumbing.Hash
		if refName == "HEAD" {
			head, herr := repo.Head()
			if herr != nil {
				return &ExecResult{
					Stdout:   refName + "\n", // host git echoes an unresolved argument
					Stderr:   fmt.Sprintf("fatal: ambiguous argument '%s': unknown revision or path not in the working tree.\n", refName),
					ExitCode: 128,
				}, nil
			}
			hash = head.Hash()
		} else {
			h, rerr := repo.ResolveRevision(plumbing.Revision(refName))
			if rerr != nil {
				return &ExecResult{
					Stdout:   refName + "\n",
					Stderr:   fmt.Sprintf("fatal: ambiguous argument '%s': unknown revision or path not in the working tree.\n", refName),
					ExitCode: 128,
				}, nil
			}
			hash = *h
		}
		return &ExecResult{Stdout: hash.String() + "\n"}, nil

	default:
		return nil, ErrUnsupported
	}
}

// nativeStatus serves only the machine-readable forms (--short / -s /
// --porcelain[=v1], with -u{all,normal,no}); their output is
// byte-identical to host git. The long form, --branch, --ignored,
// pathspecs and any shape porcelainState cannot render exactly decline.
func nativeStatus(_ context.Context, dir string, args []string) (*ExecResult, error) {
	short, porcelain := false, false
	untracked := "normal"
	for _, a := range args {
		switch a {
		case "--short", "-s":
			short = true
		case "--porcelain", "--porcelain=v1":
			short, porcelain = true, true
		case "-uall", "--untracked-files=all", "-u", "--untracked-files":
			untracked = "all"
		case "-unormal", "--untracked-files=normal":
			untracked = "normal"
		case "-uno", "--untracked-files=no":
			untracked = "no"
		default:
			return nil, ErrUnsupported
		}
	}
	if !short {
		return nil, ErrUnsupported
	}
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}
	ps, err := loadPorcelainState(repo, wt)
	if err != nil || ps.unsupported != "" {
		return nil, ErrUnsupported
	}
	var b strings.Builder
	for _, p := range ps.sortedTracked() {
		xy := ps.tracked[p]
		if porcelain && (xy[1] == 'm' || xy[1] == '?') {
			xy[1] = 'M' // v1 has no submodule-content letters
		}
		b.WriteByte(xy[0])
		b.WriteByte(xy[1])
		b.WriteByte(' ')
		b.WriteString(quotePath(p, true))
		b.WriteByte('\n')
	}
	var others []string
	switch untracked {
	case "all":
		others = ps.untracked
	case "normal":
		others = ps.collapsedUntracked()
	}
	for _, p := range others {
		b.WriteString("?? ")
		b.WriteString(quotePath(p, true))
		b.WriteByte('\n')
	}
	return &ExecResult{Stdout: b.String()}, nil
}

func nativeLog(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Parse flags
	limit := 0
	oneline := false
	formatStr := ""
	var sinceTime, untilTime *time.Time
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--oneline":
			oneline = true
		case strings.HasPrefix(args[i], "-") && len(args[i]) > 1 && args[i][1] >= '0' && args[i][1] <= '9':
			fmt.Sscanf(args[i], "-%d", &limit)
		case strings.HasPrefix(args[i], "-n"):
			if args[i] == "-n" && i+1 < len(args) {
				i++
				fmt.Sscanf(args[i], "%d", &limit)
			} else {
				fmt.Sscanf(args[i][2:], "%d", &limit)
			}
		case strings.HasPrefix(args[i], "--format="):
			formatStr = strings.TrimPrefix(args[i], "--format=")
		case args[i] == "--format" && i+1 < len(args):
			i++
			formatStr = args[i]
		case strings.HasPrefix(args[i], "--since="):
			val := strings.TrimPrefix(args[i], "--since=")
			if t, err := parseGitDate(val); err == nil {
				sinceTime = &t
			} else {
				return nil, ErrUnsupported
			}
		case strings.HasPrefix(args[i], "--until="):
			val := strings.TrimPrefix(args[i], "--until=")
			if t, err := parseGitDate(val); err == nil {
				untilTime = &t
			} else {
				return nil, ErrUnsupported
			}
		case strings.HasPrefix(args[i], "--date="):
			// Accept --date= flag (formatting hint) — ignore for native
		case strings.HasPrefix(args[i], "--author="):
			// Author filter — fall through to host git for now
			return nil, ErrUnsupported
		default:
			// Unsupported flags (e.g., path specs)
			return nil, ErrUnsupported
		}
	}

	logOpts := &gogit.LogOptions{}
	if sinceTime != nil {
		logOpts.Since = sinceTime
	}
	if untilTime != nil {
		logOpts.Until = untilTime
	}
	iter, err := repo.Log(logOpts)
	if err != nil {
		return nil, ErrUnsupported
	}

	var b strings.Builder
	count := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if limit > 0 && count >= limit {
			return fmt.Errorf("stop")
		}
		count++

		if oneline {
			b.WriteString(c.Hash.String()[:7])
			b.WriteByte(' ')
			// First line of commit message
			msg := strings.SplitN(c.Message, "\n", 2)[0]
			b.WriteString(msg)
			b.WriteByte('\n')
		} else if formatStr != "" {
			line := formatCommit(formatStr, c)
			b.WriteString(line)
			b.WriteByte('\n')
		} else {
			b.WriteString("commit ")
			b.WriteString(c.Hash.String())
			b.WriteByte('\n')
			b.WriteString("Author: ")
			b.WriteString(c.Author.Name)
			b.WriteString(" <")
			b.WriteString(c.Author.Email)
			b.WriteString(">\n")
			b.WriteString("Date:   ")
			b.WriteString(c.Author.When.Format("Mon Jan 2 15:04:05 2006 -0700"))
			b.WriteString("\n\n    ")
			b.WriteString(strings.TrimRight(c.Message, "\n"))
			b.WriteString("\n\n")
		}
		return nil
	})
	// "stop" error is our sentinel, not a real error.
	if err != nil && err.Error() != "stop" {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: b.String()}, nil
}

// formatCommit handles basic --format placeholders.
func formatCommit(format string, c *object.Commit) string {
	r := strings.NewReplacer(
		"%H", c.Hash.String(),
		"%h", c.Hash.String()[:7],
		"%s", strings.SplitN(c.Message, "\n", 2)[0],
		"%an", c.Author.Name,
		"%ae", c.Author.Email,
		"%cn", c.Committer.Name,
		"%ce", c.Committer.Email,
		"%ad", c.Author.When.Format(time.RFC3339),
		"%cd", c.Committer.When.Format(time.RFC3339),
	)
	return r.Replace(format)
}

// diffStatusLetter maps a go-git status code to its git diff-filter
// letter. '?' covers Unmodified/Untracked, which plain diff never lists.
func diffStatusLetter(code gogit.StatusCode) byte {
	switch code {
	case gogit.Added:
		return 'A'
	case gogit.Modified:
		return 'M'
	case gogit.Deleted:
		return 'D'
	case gogit.Renamed:
		return 'R'
	case gogit.Copied:
		return 'C'
	case gogit.UpdatedButUnmerged:
		return 'U'
	default:
		return '?'
	}
}

// diffFilterKeep applies a --diff-filter string (host rules: uppercase
// selects, lowercase excludes) to one entry letter.
func diffFilterKeep(filter string, letter byte) bool {
	if letter == '?' {
		return false
	}
	if filter == "" {
		return true
	}
	included := false
	hasUpper := false
	for i := 0; i < len(filter); i++ {
		c := filter[i]
		if c >= 'a' && c <= 'z' {
			if c-'a'+'A' == letter {
				return false
			}
			continue
		}
		hasUpper = true
		if c == letter {
			included = true
		}
	}
	if !hasUpper {
		return true
	}
	return included
}

func nativeDiff(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	// Parse flags. Commit revisions are not supported (only worktree
	// forms); exact on-disk paths after -- (or bare) filter the listing.
	cached := false
	noRenames := false
	stat := false
	quiet := false
	nameOnly := false
	diffFilter := ""
	rev := ""
	var paths []string
	dashDash := false
	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch {
		case arg == "--":
			dashDash = true
		case arg == "--cached" || arg == "--staged":
			cached = true
		case arg == "--no-renames":
			noRenames = true
		case arg == "--binary" || arg == "-p" || arg == "--patch" || arg == "--no-color" || arg == "--no-ext-diff":
			// --binary only matters for binary files, which stagedPatch
			// refuses (ErrUnsupported) rather than emitting a stub.
		case arg == "--stat":
			stat = true
		case arg == "--quiet":
			quiet = true
		case arg == "--name-only":
			nameOnly = true
		case arg == "--diff-filter" && i+1 < len(args):
			i++
			diffFilter += args[i]
		case strings.HasPrefix(arg, "--diff-filter="):
			diffFilter += strings.TrimPrefix(arg, "--diff-filter=")
		case strings.HasPrefix(arg, "-"):
			return nil, ErrUnsupported
		case dashDash:
			paths = append(paths, filepath.ToSlash(filepath.Clean(arg)))
		default:
			// A bare positional is a pathspec only when it names a
			// file on disk or in the status; otherwise it is a commit
			// revision, which only the --cached form can diff.
			if _, serr := os.Stat(filepath.Join(dir, filepath.FromSlash(arg))); serr != nil {
				if cached && rev == "" {
					if _, rerr := repo.ResolveRevision(plumbing.Revision(arg)); rerr == nil {
						rev = arg
						continue
					}
				}
				st, serr := worktreeStatus(wt)
				if serr != nil {
					return nil, ErrUnsupported
				}
				if _, ok := st[filepath.ToSlash(filepath.Clean(arg))]; !ok {
					return nil, ErrUnsupported
				}
			}
			paths = append(paths, filepath.ToSlash(filepath.Clean(arg)))
		}
	}

	if rev != "" && (quiet || nameOnly || stat) {
		return nil, ErrUnsupported
	}
	if stat {
		return nil, ErrUnsupported
	}

	ps, err := loadPorcelainState(repo, wt)
	if err != nil || (ps.unsupported != "" && !(noRenames && ps.unsupported == unsupportedRenames)) {
		return nil, ErrUnsupported
	}
	// changed lists the paths this diff covers, with their letter in the
	// compared column: X (index vs HEAD) for --cached, Y (worktree vs
	// index) otherwise. A submodule with dirty content counts as a
	// worktree change, as in host git.
	changed := func() []string {
		var out []string
		for _, path := range ps.sortedTracked() {
			if len(paths) > 0 && !slices.Contains(paths, path) {
				continue
			}
			c := ps.tracked[path][1]
			if cached {
				c = ps.tracked[path][0]
			}
			switch c {
			case ' ':
				continue
			case 'm', '?':
				c = 'M'
			}
			if diffFilterKeep(diffFilter, c) {
				out = append(out, path)
			}
		}
		return out
	}

	// --quiet reports only via exit status: 0 = no differences, 1 =
	// differences. Combined with --cached this is the "are there staged
	// changes?" predicate loom uses to skip empty commits.
	if quiet {
		if len(changed()) > 0 {
			return &ExecResult{ExitCode: 1}, nil
		}
		return &ExecResult{ExitCode: 0}, nil
	}

	if nameOnly {
		var b strings.Builder
		for _, p := range changed() {
			b.WriteString(quotePath(p, false) + "\n")
		}
		return &ExecResult{Stdout: b.String()}, nil
	}

	// Patches: gitlink hunks ("Subproject commit …") are host-git only.
	for _, path := range changed() {
		for _, e := range ps.idx.Entries {
			if e.Name == path && e.Mode == filemode.Submodule {
				return nil, ErrUnsupported
			}
		}
	}

	if cached {
		patch, err := stagedPatch(repo, rev, paths, noRenames)
		if err != nil {
			return nil, err
		}
		return &ExecResult{Stdout: patch}, nil
	}
	if rev != "" {
		return nil, ErrUnsupported
	}

	// Unstaged changes: diff between index and working tree
	status, err := worktreeStatus(wt)
	if err != nil {
		return nil, ErrUnsupported
	}

	if status.IsClean() {
		return &ExecResult{Stdout: ""}, nil
	}

	// For full diff output, fall through to host git — go-git's patch generation
	// for worktree diffs requires manual file comparison.
	return nil, ErrUnsupported
}

func nativeMergeBase(_ context.Context, dir string, args []string) (*ExecResult, error) {
	isAncestorMode := false
	var pos []string
	for _, a := range args {
		switch a {
		case "--is-ancestor":
			isAncestorMode = true
		default:
			if strings.HasPrefix(a, "-") {
				return nil, ErrUnsupported
			}
			pos = append(pos, a)
		}
	}
	if len(pos) != 2 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	hash1, err := repo.ResolveRevision(plumbing.Revision(pos[0]))
	if err != nil {
		return nil, ErrUnsupported
	}

	hash2, err := repo.ResolveRevision(plumbing.Revision(pos[1]))
	if err != nil {
		return nil, ErrUnsupported
	}

	// --is-ancestor reports the answer through exit status only: 0 when
	// hash1 is an ancestor of (or equal to) hash2, 1 otherwise.
	if isAncestorMode {
		anc, err := isAncestor(repo, *hash1, *hash2)
		if err != nil {
			return nil, ErrUnsupported
		}
		if anc {
			return &ExecResult{ExitCode: 0}, nil
		}
		return &ExecResult{ExitCode: 1}, nil
	}

	commit1, err := repo.CommitObject(*hash1)
	if err != nil {
		return nil, ErrUnsupported
	}

	commit2, err := repo.CommitObject(*hash2)
	if err != nil {
		return nil, ErrUnsupported
	}

	bases, err := commit1.MergeBase(commit2)
	if err != nil || len(bases) == 0 {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: bases[0].Hash.String() + "\n"}, nil
}

func nativeRevList(_ context.Context, dir string, args []string) (*ExecResult, error) {
	// Handle --count ref1..ref2 or --count ref1...ref2
	countMode := false
	var rangeSpec string
	for _, arg := range args {
		if arg == "--count" {
			countMode = true
		} else if !strings.HasPrefix(arg, "-") {
			rangeSpec = arg
		}
	}

	if !countMode || rangeSpec == "" {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Parse ref1..ref2 or ref1...ref2
	var from, to string
	if parts := strings.SplitN(rangeSpec, "...", 2); len(parts) == 2 {
		from, to = parts[0], parts[1]
	} else if parts := strings.SplitN(rangeSpec, "..", 2); len(parts) == 2 {
		from, to = parts[0], parts[1]
	} else {
		return nil, ErrUnsupported
	}

	fromHash, err := repo.ResolveRevision(plumbing.Revision(from))
	if err != nil {
		return nil, ErrUnsupported
	}

	toHash, err := repo.ResolveRevision(plumbing.Revision(to))
	if err != nil {
		return nil, ErrUnsupported
	}

	// Count commits reachable from 'to' but not from 'from'
	iter, err := repo.Log(&gogit.LogOptions{From: *toHash})
	if err != nil {
		return nil, ErrUnsupported
	}

	count := 0
	err = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == *fromHash {
			return fmt.Errorf("stop")
		}
		count++
		return nil
	})
	if err != nil && err.Error() != "stop" {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: fmt.Sprintf("%d\n", count)}, nil
}

func nativeConfig(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Only handle reads (single key argument or --get key)
	key := ""
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--get" && i+1 < len(args):
			i++
			key = args[i]
		case !strings.HasPrefix(args[i], "-"):
			if key == "" {
				key = args[i]
			} else {
				// Two non-flag args = a write operation
				return nil, ErrUnsupported
			}
		default:
			return nil, ErrUnsupported
		}
	}

	if key == "" {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	cfg, err := repo.ConfigScoped(config.GlobalScope)
	if err != nil {
		return nil, ErrUnsupported
	}

	switch key {
	case "user.name":
		if cfg.User.Name == "" {
			return &ExecResult{ExitCode: 1}, nil
		}
		return &ExecResult{Stdout: cfg.User.Name + "\n"}, nil
	case "user.email":
		if cfg.User.Email == "" {
			return &ExecResult{ExitCode: 1}, nil
		}
		return &ExecResult{Stdout: cfg.User.Email + "\n"}, nil
	default:
		return nil, ErrUnsupported
	}
}

func nativeAdd(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	for _, arg := range args {
		if arg == "-A" || arg == "--all" || arg == "." {
			// Add all: get status and add each file
			status, err := worktreeStatus(wt)
			if err != nil {
				return nil, ErrUnsupported
			}
			for path := range status {
				if _, err := wt.Add(path); err != nil {
					return nil, ErrUnsupported
				}
			}
			return &ExecResult{Stdout: ""}, nil
		}
	}

	// Add specific paths
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			return nil, ErrUnsupported
		}
		relPath := relToRepoRoot(wt.Filesystem.Root(), dir, arg)
		if _, err := wt.Add(relPath); err != nil {
			return nil, ErrUnsupported
		}
	}

	return &ExecResult{Stdout: ""}, nil
}

func nativeCommit(_ context.Context, dir string, args []string) (*ExecResult, error) {
	// Extract -m message
	msg := ""
	amend := false
	quiet := false
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" && i+1 < len(args) {
			i++
			msg = args[i]
		} else if strings.HasPrefix(args[i], "-m") {
			msg = args[i][2:]
		} else if args[i] == "--amend" {
			amend = true
		} else if args[i] == "-q" {
			quiet = true
		} else if args[i] == "--allow-empty" || args[i] == "--no-verify" {
			// Accepted flags, no-op
		} else {
			return nil, ErrUnsupported
		}
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// --amend without -m keeps the HEAD message (typed Commit parity).
	if msg == "" {
		if !amend {
			return nil, ErrUnsupported
		}
		headRef, herr := repo.Head()
		if herr != nil {
			return nil, ErrUnsupported
		}
		headCommit, herr := repo.CommitObject(headRef.Hash())
		if herr != nil {
			return nil, ErrUnsupported
		}
		msg = headCommit.Message
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	opts := &gogit.CommitOptions{Amend: amend}
	// Provide an explicit author when git config is not available (e.g. in containers).
	// Try repo-level config first, then fall back to a default signature.
	if sig := resolveAuthor(repo); sig != nil {
		opts.Author = sig
	}

	hash, err := wt.Commit(msg, opts)
	if err != nil {
		return nil, ErrUnsupported
	}

	if quiet {
		return &ExecResult{Stdout: ""}, nil
	}
	// Format output similar to git commit
	head, _ := repo.Head()
	branchName := "HEAD"
	if head != nil && head.Name().IsBranch() {
		branchName = head.Name().Short()
	}

	firstLine := strings.SplitN(msg, "\n", 2)[0]
	out := fmt.Sprintf("[%s %s] %s\n", branchName, hash.String()[:7], firstLine)
	return &ExecResult{Stdout: out}, nil
}

func nativeBranch(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Parse list flags. Unknown flags fail here, structurally — never
	// fall into a positional slot (GAPS.md section D).
	listRemote := false
	listAll := false
	verbose := false
	containsRef := ""
	nonFlagArgs := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-v", "--verbose":
			verbose = true
		case "-r":
			listRemote = true
		case "-a":
			listAll = true
		case "--contains":
			if i+1 < len(args) {
				i++
				containsRef = args[i]
			} else {
				return nil, ErrUnsupported
			}
		case "-D", "-d":
			// Delete verbs: consumed positionally below (args[0]).
			nonFlagArgs = append(nonFlagArgs, args[i])
		default:
			if strings.HasPrefix(args[i], "--contains=") {
				containsRef = strings.TrimPrefix(args[i], "--contains=")
			} else if strings.HasPrefix(args[i], "-") {
				return nil, ErrUnsupported
			} else {
				nonFlagArgs = append(nonFlagArgs, args[i])
			}
		}
	}

	// branchVerbose decorates one branch line with tip hash + subject.
	// Resolution failures degrade to the plain line: verbosity must
	// never break listing.
	branchVerbose := func(ref *plumbing.Reference) string {
		if !verbose {
			return ""
		}
		c, err := repo.CommitObject(ref.Hash())
		if err != nil {
			return ""
		}
		subj, _, _ := strings.Cut(strings.TrimSpace(c.Message), "\n")
		return " " + shortHash(ref.Hash()) + " " + subj
	}

	// Listing mode: no non-flag args, or only -v/-r/-a/--contains
	isListMode := len(nonFlagArgs) == 0 || (len(args) > 0 && (args[0] == "-v" || args[0] == "-r" || args[0] == "-a"))

	if isListMode && (len(nonFlagArgs) == 0) {
		head, _ := repo.Head()
		if head != nil && !head.Name().IsBranch() && !listRemote {
			// Detached HEAD: host git leads with "* (HEAD detached at
			// <rev>)", where <rev> is how HEAD was reached; decline.
			return nil, ErrUnsupported
		}

		// Resolve --contains commit hash if specified
		var containsHash *plumbing.Hash
		if containsRef != "" {
			h, err := repo.ResolveRevision(plumbing.Revision(containsRef))
			if err != nil {
				return &ExecResult{
					Stderr:   fmt.Sprintf("error: no such commit '%s'\n", containsRef),
					ExitCode: 129,
				}, nil
			}
			containsHash = h
		}

		// Host git lists refs in refname order: locals, then remotes —
		// "remotes/"-prefixed under -a — with a symbolic remote ref as
		// "origin/HEAD -> origin/main".
		type row struct {
			mark, name, sym string
			ref             *plumbing.Reference
		}
		var rows []row
		contains := func(ref *plumbing.Reference) bool {
			return containsHash == nil || branchContainsCommit(repo, ref.Hash(), *containsHash)
		}

		if !listRemote {
			refs, err := repo.Branches()
			if err != nil {
				return nil, ErrUnsupported
			}
			var locals []*plumbing.Reference
			if err := refs.ForEach(func(ref *plumbing.Reference) error {
				locals = append(locals, ref)
				return nil
			}); err != nil {
				return nil, ErrUnsupported
			}
			sort.Slice(locals, func(i, j int) bool { return locals[i].Name() < locals[j].Name() })
			cfg, cerr := repo.Config()
			for _, ref := range locals {
				if !contains(ref) {
					continue
				}
				name := ref.Name().Short()
				if verbose {
					// -v adds "[ahead N, behind M]" for a tracked branch.
					if cerr != nil {
						return nil, ErrUnsupported
					}
					if bc, ok := cfg.Branches[name]; ok && bc.Merge != "" {
						return nil, ErrUnsupported
					}
				}
				mark := "  "
				if head != nil && ref.Name() == head.Name() {
					mark = "* "
				}
				rows = append(rows, row{mark: mark, name: name, ref: ref})
			}
		}

		if listRemote || listAll {
			allRefs, err := repo.References()
			if err != nil {
				return nil, ErrUnsupported
			}
			var remotes []*plumbing.Reference
			if err := allRefs.ForEach(func(ref *plumbing.Reference) error {
				if ref.Name().IsRemote() {
					remotes = append(remotes, ref)
				}
				return nil
			}); err != nil {
				return nil, ErrUnsupported
			}
			sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name() < remotes[j].Name() })
			prefix := ""
			if listAll {
				prefix = "remotes/"
			}
			for _, ref := range remotes {
				name := prefix + ref.Name().Short()
				if ref.Type() == plumbing.SymbolicReference {
					if containsHash != nil {
						return nil, ErrUnsupported
					}
					rows = append(rows, row{mark: "  ", name: name, sym: ref.Target().Short()})
					continue
				}
				if !contains(ref) {
					continue
				}
				rows = append(rows, row{mark: "  ", name: name, ref: ref})
			}
		}

		// -v pads names to the widest listed one (symbolic refs excluded).
		width := 0
		for _, r := range rows {
			if r.sym == "" && len(r.name) > width {
				width = len(r.name)
			}
		}
		var b strings.Builder
		for _, r := range rows {
			b.WriteString(r.mark)
			switch {
			case r.sym != "":
				fmt.Fprintf(&b, "%s -> %s", r.name, r.sym)
			case verbose:
				fmt.Fprintf(&b, "%-*s%s", width, r.name, branchVerbose(r.ref))
			default:
				b.WriteString(r.name)
			}
			b.WriteByte('\n')
		}

		return &ExecResult{Stdout: b.String()}, nil
	}

	// No args or -v: list branches (original simple path)
	if len(args) == 0 || (len(args) == 1 && args[0] == "-v") {
		head, _ := repo.Head()
		refs, err := repo.Branches()
		if err != nil {
			return nil, ErrUnsupported
		}

		var b strings.Builder
		err = refs.ForEach(func(ref *plumbing.Reference) error {
			name := ref.Name().Short()
			if head != nil && ref.Name() == head.Name() {
				b.WriteString("* ")
			} else {
				b.WriteString("  ")
			}
			b.WriteString(name)
			b.WriteString(branchVerbose(ref))
			b.WriteByte('\n')
			return nil
		})
		if err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{Stdout: b.String()}, nil
	}

	// Delete branch: -D forces, -d refuses an unmerged branch.
	if args[0] == "-D" || args[0] == "-d" {
		if len(args) < 2 {
			return nil, ErrUnsupported
		}
		branchName := args[1]
		force := args[0] == "-D"
		refName := plumbing.NewBranchReferenceName(branchName)
		ref, err := repo.Reference(refName, false)
		if err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("error: branch '%s' not found.\n", branchName),
				ExitCode: 1,
			}, nil
		}
		if !force {
			if head, herr := repo.Head(); herr == nil {
				if head.Name() == refName {
					return &ExecResult{
						Stderr:   fmt.Sprintf("error: cannot delete branch '%s' used by worktree\n", branchName),
						ExitCode: 1,
					}, nil
				}
				if merged, aerr := isAncestor(repo, ref.Hash(), head.Hash()); aerr == nil && !merged {
					return &ExecResult{
						Stderr:   fmt.Sprintf("error: the branch '%s' is not fully merged.\n", branchName),
						ExitCode: 1,
					}, nil
				}
			}
		}
		if err := repo.Storer.RemoveReference(refName); err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("error: branch '%s' not found.\n", branchName),
				ExitCode: 1,
			}, nil
		}
		_ = repo.DeleteBranch(branchName) // best-effort config cleanup
		return &ExecResult{Stdout: fmt.Sprintf("Deleted branch %s\n", branchName)}, nil
	}

	// Create branch (single name argument)
	if len(args) == 1 && !strings.HasPrefix(args[0], "-") {
		branchName := args[0]
		head, err := repo.Head()
		if err != nil {
			return nil, ErrUnsupported
		}
		refName := plumbing.NewBranchReferenceName(branchName)
		ref := plumbing.NewHashReference(refName, head.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{Stdout: ""}, nil
	}

	return nil, ErrUnsupported
}

func nativeCheckout(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	idxPath := repoIndexPath(repo, wt)
	var origIndex []byte
	var origMode os.FileMode = 0o644
	var hadIndex bool
	if idxPath != "" {
		if fi, err := os.Stat(idxPath); err == nil {
			origMode = fi.Mode()
			if data, err := os.ReadFile(idxPath); err == nil {
				origIndex = data
				hadIndex = true
			}
		} else if errors.Is(err, os.ErrNotExist) {
			return nil, checkoutUnsupported("unborn index requires initial checkout by host git")
		}
	}
	restoreIndex := func() {
		if idxPath == "" {
			return
		}
		if hadIndex {
			_ = os.WriteFile(idxPath, origIndex, origMode)
		} else {
			_ = os.Remove(idxPath)
		}
	}

	// -f/--force discards local changes on branch switches (host parity
	// for the switch itself; pathspec checkouts stay unsupported).
	force := false
	for len(args) > 0 && (args[0] == "-f" || args[0] == "--force") {
		force = true
		args = args[1:]
	}
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Handle -b (create and checkout)
	if args[0] == "-b" {
		if len(args) < 2 || len(args) > 3 {
			return nil, checkoutUnsupported("-b accepts a branch and optional start point")
		}
		branchName := args[1]
		startPoint := "HEAD"
		if len(args) == 3 {
			startPoint = args[2]
		}
		commit, err := resolveCommit(repo, startPoint)
		if err != nil {
			return &ExecResult{Stderr: fmt.Sprintf("fatal: invalid reference: %s\n", startPoint), ExitCode: 128}, nil
		}

		// A clone made with --no-checkout has an index for HEAD but no
		// working-tree files. go-git regards those files as local deletions
		// and rejects its normal merge reset only after creating the branch.
		// It is safe to populate exactly this state with a hard reset; any
		// other dirty state remains unsupported before we mutate refs.
		populate, err := checkoutNeedsPopulate(wt)
		if err != nil {
			return nil, checkoutUnsupported("cannot inspect worktree status: %v", err)
		}
		if !force && !populate {
			status, err := worktreeStatus(wt)
			if err != nil {
				return nil, checkoutUnsupported("cannot inspect worktree status: %v", err)
			}
			if !status.IsClean() {
				return nil, checkoutUnsupported("dirty worktree is not safe for a branch-creating checkout")
			}
		}

		branchRef := plumbing.NewBranchReferenceName(branchName)
		oldHead, err := repo.Storer.Reference(plumbing.HEAD)
		if err != nil {
			return nil, checkoutUnsupported("cannot read HEAD before checkout: %v", err)
		}
		err = wt.Checkout(&gogit.CheckoutOptions{
			Branch: branchRef,
			Hash:   commit.Hash,
			Create: true,
			Force:  force || populate,
		})
		if err != nil {
			// Checkout creates the branch before changing HEAD/index. Do not
			// leave a partial native attempt for RunExternal to trip over.
			_ = repo.Storer.RemoveReference(branchRef)
			_ = repo.Storer.SetReference(oldHead)
			restoreIndex()
			return nil, checkoutUnsupported("worktree checkout failed: %v", err)
		}
		return &ExecResult{
			Stdout: fmt.Sprintf("Switched to a new branch '%s'\n", branchName),
		}, nil
	}

	// Handle -B (create the branch, or reset it to HEAD if it exists,
	// then switch to it). This is what loom's reference-clone setup uses.
	if args[0] == "-B" {
		if len(args) != 2 {
			return nil, checkoutUnsupported("-B accepts exactly one branch name")
		}
		branchName := args[1]
		head, err := repo.Head()
		if err != nil {
			return nil, checkoutUnsupported("cannot resolve HEAD: %v", err)
		}
		populate, err := checkoutNeedsPopulate(wt)
		if err != nil {
			return nil, checkoutUnsupported("cannot inspect worktree status: %v", err)
		}
		if !force && !populate {
			status, err := worktreeStatus(wt)
			if err != nil {
				return nil, checkoutUnsupported("cannot inspect worktree status: %v", err)
			}
			if !status.IsClean() {
				return nil, checkoutUnsupported("dirty worktree is not safe for a branch-resetting checkout")
			}
		}
		branchRef := plumbing.NewBranchReferenceName(branchName)
		oldBranch, branchErr := repo.Storer.Reference(branchRef)
		if branchErr != nil && !errors.Is(branchErr, plumbing.ErrReferenceNotFound) {
			return nil, checkoutUnsupported("cannot read branch %q: %v", branchName, branchErr)
		}
		oldHead, err := repo.Storer.Reference(plumbing.HEAD)
		if err != nil {
			return nil, checkoutUnsupported("cannot read HEAD before checkout: %v", err)
		}
		ref := plumbing.NewHashReference(branchRef, head.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			return nil, checkoutUnsupported("cannot reset branch %q: %v", branchName, err)
		}
		if err := wt.Checkout(&gogit.CheckoutOptions{Branch: branchRef, Force: force || populate}); err != nil {
			if oldBranch == nil {
				_ = repo.Storer.RemoveReference(branchRef)
			} else {
				_ = repo.Storer.SetReference(oldBranch)
			}
			_ = repo.Storer.SetReference(oldHead)
			restoreIndex()
			return nil, checkoutUnsupported("worktree checkout failed: %v", err)
		}
		return &ExecResult{
			Stdout: fmt.Sprintf("Reset branch '%s'\n", branchName),
		}, nil
	}

	// Checkout existing branch
	if len(args) == 1 && !strings.HasPrefix(args[0], "-") {
		branchName := args[0]
		err := wt.Checkout(&gogit.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(branchName),
			Force:  force,
		})
		if err != nil {
			restoreIndex()
			return nil, ErrUnsupported
		}
		return &ExecResult{
			Stdout: fmt.Sprintf("Switched to branch '%s'\n", branchName),
		}, nil
	}

	// checkout --theirs/--ours [--] <paths>: resolve unmerged index
	// stages back into the worktree (conflict triage). Stage 3 is
	// theirs, stage 2 ours; the index collapses to the chosen side.
	// Our own merges are all-or-nothing and never leave U state, so in
	// practice this serves host-created conflicts in shared repos.
	if len(args) > 0 && (args[0] == "--theirs" || args[0] == "--ours") {
		res, err := checkoutSide(repo, wt, wt.Filesystem.Root(), args[0] == "--theirs", args[1:])
		if err != nil {
			restoreIndex()
		}
		return res, err
	}

	return nil, ErrUnsupported
}

func repoIndexPath(repo *gogit.Repository, wt *gogit.Worktree) string {
	if s, ok := repo.Storer.(*filesystem.Storage); ok && s.Filesystem() != nil {
		if root := s.Filesystem().Root(); root != "" {
			return filepath.Join(root, "index")
		}
	}
	if wt != nil && wt.Filesystem != nil {
		root := wt.Filesystem.Root()
		dotgit := filepath.Join(root, ".git")
		if fi, err := os.Stat(dotgit); err == nil {
			if fi.IsDir() {
				return filepath.Join(dotgit, "index")
			}
			if raw, err := os.ReadFile(dotgit); err == nil {
				line := strings.TrimSpace(string(raw))
				if strings.HasPrefix(line, "gitdir: ") {
					admin := strings.TrimPrefix(line, "gitdir: ")
					if !filepath.IsAbs(admin) {
						admin = filepath.Join(root, admin)
					}
					return filepath.Join(admin, "index")
				}
			}
		}
	}
	return ""
}

func checkoutUnsupported(format string, args ...any) error {
	return fmt.Errorf("%w: checkout: %s", ErrUnsupported, fmt.Sprintf(format, args...))
}

// checkoutNeedsPopulate reports the one dirty state produced by `git clone
// --no-checkout`: depending on the index implementation, go-git reports each
// path as either deleted from the worktree or deleted from the index.
func checkoutNeedsPopulate(wt *gogit.Worktree) (bool, error) {
	status, err := worktreeStatus(wt)
	if err != nil {
		return false, err
	}
	if len(status) == 0 {
		return false, nil
	}
	for _, file := range status {
		missingFromWorktree := file.Staging == gogit.Unmodified && file.Worktree == gogit.Deleted
		missingFromIndex := file.Staging == gogit.Deleted && file.Worktree == gogit.Unmodified
		if !missingFromWorktree && !missingFromIndex {
			return false, nil
		}
	}
	return true, nil
}

// checkoutSide implements `checkout --theirs|--ours [--] <paths...>`.
func checkoutSide(repo *gogit.Repository, wt *gogit.Worktree, root string, theirs bool, args []string) (*ExecResult, error) {
	var paths []string
	for _, a := range args {
		if a == "--" {
			continue
		}
		if strings.HasPrefix(a, "-") {
			return nil, ErrUnsupported
		}
		paths = append(paths, filepath.ToSlash(filepath.Clean(a)))
	}
	if len(paths) == 0 {
		return nil, ErrUnsupported
	}
	want := index.OurMode
	if theirs {
		want = index.TheirMode
	}

	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, ErrUnsupported
	}
	for _, p := range paths {
		var pick *index.Entry
		unmerged := false
		for _, e := range idx.Entries {
			if e.Name != p {
				continue
			}
			if e.Stage != index.Merged {
				unmerged = true
			}
			if e.Stage == want {
				pick = e
			}
		}
		if !unmerged || pick == nil {
			return &ExecResult{Stderr: fmt.Sprintf("error: pathspec '%s' did not match any unmerged file(s)\n", p), ExitCode: 1}, nil
		}
		blob, err := repo.BlobObject(pick.Hash)
		if err != nil {
			return nil, ErrUnsupported
		}
		r, err := blob.Reader()
		if err != nil {
			return nil, ErrUnsupported
		}
		raw, err := io.ReadAll(r)
		r.Close()
		if err != nil {
			return nil, ErrUnsupported
		}
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return nil, ErrUnsupported
		}
		if err := os.WriteFile(full, raw, 0o644); err != nil {
			return nil, ErrUnsupported
		}
		// Collapse every stage of this path to the chosen blob: the
		// picked entry itself survives (re-staged merged), the other
		// stages for this path go.
		pick.Stage = index.Merged
		var rebuilt []*index.Entry
		for _, e := range idx.Entries {
			if e.Name == p && e != pick {
				continue
			}
			rebuilt = append(rebuilt, e)
		}
		idx.Entries = rebuilt
		if ist, ok := repo.Storer.(storer.IndexStorer); ok {
			if err := ist.SetIndex(idx); err != nil {
				return nil, ErrUnsupported
			}
		} else {
			return nil, ErrUnsupported
		}
	}
	_ = wt
	return &ExecResult{Stdout: ""}, nil
}

func nativeRemote(_ context.Context, dir string, args []string) (*ExecResult, error) {
	// Bare `remote` lists names; `remote -v` lists name + URL (both via
	// the typed Remotes helper — weave's template bootstrap uses these).
	if len(args) == 0 || (len(args) == 1 && args[0] == "-v") {
		_, entries, err := Remotes(dir)
		if err != nil {
			return nil, ErrUnsupported
		}
		var b strings.Builder
		for _, e := range entries {
			if len(args) == 1 {
				url := ""
				if len(e.URLs) > 0 {
					url = e.URLs[0]
				}
				fmt.Fprintf(&b, "%s\t%s (fetch)\n%s\t%s (push)\n", e.Name, url, e.Name, url)
				continue
			}
			b.WriteString(e.Name + "\n")
		}
		return &ExecResult{Stdout: b.String()}, nil
	}
	if len(args) < 2 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	remoteName := args[1]

	switch args[0] {
	case "add":
		// `remote add <name> <url>`: exactly one URL, like host git.
		if len(args) != 3 {
			return nil, ErrUnsupported
		}
		if _, err := repo.CreateRemote(&config.RemoteConfig{Name: remoteName, URLs: []string{args[2]}}); err != nil {
			return &ExecResult{Stderr: fmt.Sprintf("fatal: remote %s already exists\n", remoteName), ExitCode: 128}, nil
		}
		return &ExecResult{Stdout: ""}, nil
	case "get-url":
		remote, err := repo.Remote(remoteName)
		if err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("fatal: No such remote '%s'\n", remoteName),
				ExitCode: 2,
			}, nil
		}
		urls := remote.Config().URLs
		if len(urls) == 0 {
			return &ExecResult{ExitCode: 1}, nil
		}
		return &ExecResult{Stdout: urls[0] + "\n"}, nil

	case "remove", "rm":
		if err := repo.DeleteRemote(remoteName); err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("fatal: No such remote: '%s'\n", remoteName),
				ExitCode: 2,
			}, nil
		}
		return &ExecResult{Stdout: ""}, nil

	default:
		return nil, ErrUnsupported
	}
}

func nativeForEachRef(_ context.Context, _ string, _ []string) (*ExecResult, error) {
	// Complex format parsing — fall through for now
	return nil, ErrUnsupported
}

func nativeReset(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	// reset [--soft|--mixed|--hard] [<commit>] [-- <paths>]; default mixed.
	// Last mode flag wins, like host git.
	mode := gogit.MixedReset
	target := "HEAD"
	var files []string
	dashDash := false
	for _, arg := range args {
		switch {
		case arg == "--":
			dashDash = true
		case arg == "--soft" && !dashDash:
			mode = gogit.SoftReset
		case arg == "--mixed" && !dashDash:
			mode = gogit.MixedReset
		case arg == "--hard" && !dashDash:
			mode = gogit.HardReset
		case dashDash:
			files = append(files, arg)
		case strings.HasPrefix(arg, "-"):
			return nil, ErrUnsupported
		case target == "HEAD":
			target = arg
		default:
			return nil, ErrUnsupported
		}
	}

	// Only mixed reset takes paths; soft/hard with paths is a host fatal.
	if len(files) > 0 && mode != gogit.MixedReset {
		kind := "soft"
		if mode == gogit.HardReset {
			kind = "hard"
		}
		return &ExecResult{Stderr: fmt.Sprintf("fatal: Cannot do %s reset with paths.\n", kind), ExitCode: 128}, nil
	}

	if len(files) > 0 {
		// Reset specific files: re-read HEAD tree and update index entries
		head, err := repo.Head()
		if err != nil {
			return nil, ErrUnsupported
		}
		commit, err := repo.CommitObject(head.Hash())
		if err != nil {
			return nil, ErrUnsupported
		}
		tree, err := commit.Tree()
		if err != nil {
			return nil, ErrUnsupported
		}

		// Get status first to determine which files are staged
		status, err := worktreeStatus(wt)
		if err != nil {
			return nil, ErrUnsupported
		}

		for _, file := range files {
			fs := status.File(file)
			if fs.Staging == gogit.Unmodified {
				continue
			}
			// Check if file exists in HEAD tree
			_, err := tree.File(file)
			if err != nil {
				// File doesn't exist in HEAD — remove from index
				// This is equivalent to git reset HEAD -- <newfile>
				// go-git doesn't have a direct "unstage" API, fall through
				return nil, ErrUnsupported
			}
			// File exists in HEAD: re-add from worktree to reset staging
			// Actually, go-git's worktree doesn't have a reset-file API
			// Fall through to host git for this case
			return nil, ErrUnsupported
		}
		return &ExecResult{Stdout: ""}, nil
	}

	// No specific files: reset to the target in the requested mode.
	commit, err := resolveCommit(repo, target)
	if err != nil {
		return &ExecResult{Stderr: fmt.Sprintf("fatal: ambiguous argument '%s': unknown revision\n", target), ExitCode: 128}, nil
	}
	err = wt.Reset(&gogit.ResetOptions{
		Mode:   mode,
		Commit: commit.Hash,
	})
	if err != nil {
		return nil, ErrUnsupported
	}

	// Host git announces the landing commit on --hard; soft/mixed stay
	// quiet (mixed keeps its "" shape from before this story).
	if mode == gogit.HardReset {
		first := strings.SplitN(commit.Message, "\n", 2)[0]
		return &ExecResult{Stdout: fmt.Sprintf("HEAD is now at %s %s\n", shortHash(commit.Hash), first)}, nil
	}
	return &ExecResult{Stdout: ""}, nil
}

// nativeShow serves `show <commit>` for a commit with at most one parent:
// git's medium header and the patch from treePatch (renames, binary and
// C-quoted names decline there). Flags, rev:path, tags and merges decline.
func nativeShow(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) != 1 || strings.HasPrefix(args[0], "-") || strings.Contains(args[0], ":") {
		return nil, ErrUnsupported
	}
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	hash, err := repo.ResolveRevision(plumbing.Revision(args[0]))
	if err != nil {
		return nil, ErrUnsupported
	}
	commit, err := repo.CommitObject(*hash)
	if err != nil || commit.NumParents() > 1 {
		return nil, ErrUnsupported
	}
	var b strings.Builder
	fmt.Fprintf(&b, "commit %s\nAuthor: %s <%s>\nDate:   %s\n\n",
		commit.Hash, commit.Author.Name, commit.Author.Email,
		commit.Author.When.Format("Mon Jan 2 15:04:05 2006 -0700"))
	for _, line := range strings.Split(strings.TrimRight(commit.Message, "\n"), "\n") {
		if line != "" {
			b.WriteString("    ")
			b.WriteString(line)
		}
		b.WriteByte('\n')
	}
	parentTree := &object.Tree{}
	if commit.NumParents() == 1 {
		parent, err := commit.Parent(0)
		if err != nil {
			return nil, ErrUnsupported
		}
		if parentTree, err = parent.Tree(); err != nil {
			return nil, ErrUnsupported
		}
	}
	commitTree, err := commit.Tree()
	if err != nil {
		return nil, ErrUnsupported
	}
	patch, err := treePatch(parentTree, commitTree, nil, false)
	if err != nil {
		return nil, ErrUnsupported
	}
	if patch != "" {
		b.WriteByte('\n')
		b.WriteString(patch)
	}
	return &ExecResult{Stdout: b.String()}, nil
}

func nativeMerge(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Parse flags
	noFF := false
	var targetBranch string
	for _, arg := range args {
		switch arg {
		case "--no-ff":
			noFF = true
		case "--squash":
			return nil, ErrUnsupported
		case "--abort":
			return nil, ErrUnsupported
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, ErrUnsupported
			}
			targetBranch = arg
		}
	}

	if targetBranch == "" {
		return nil, ErrUnsupported
	}

	// An unresolvable target is a user error (exit 1, git's "not
	// something we can merge"), not a fall-back-to-host-git case —
	// resolve it up front so the message matches git.
	if repo, err := openRepo(dir); err == nil {
		if _, rerr := repo.ResolveRevision(plumbing.Revision(targetBranch)); rerr != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("merge: '%s' - not something we can merge\n", targetBranch),
				ExitCode: 1,
			}, nil
		}
	}

	// Delegate to the typed Merge: fast-forward, --no-ff merge commit,
	// and real diverged 3-way merge all share that one code path. A
	// conflict comes back as *ConflictError, which we render git-style
	// with exit 1 and no mutation (the all-or-nothing contract).
	res, err := Merge(MergeOptions{RepoPath: dir, Ref: targetBranch, NoFF: noFF})
	if err != nil {
		var ce *ConflictError
		if errors.As(err, &ce) {
			var b strings.Builder
			for _, f := range ce.Files {
				fmt.Fprintf(&b, "CONFLICT (content): Merge conflict in %s\n", f)
			}
			b.WriteString("Automatic merge failed; fix conflicts and then commit the result.\n")
			return &ExecResult{Stdout: b.String(), ExitCode: 1}, nil
		}
		// Unresolvable revision, dirty tree, missing identity, etc. —
		// fall through to host git, which has the full toolset.
		return nil, ErrUnsupported
	}
	return &ExecResult{Stdout: res.Message + "\n"}, nil
}

func nativeTag(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// No args or -l: list tags
	if len(args) == 0 {
		return listTags(repo, "")
	}

	// Parse flags
	annotated := false
	delete := false
	message := ""
	pattern := ""
	var tagName string

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-l":
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "-") {
				i++
				pattern = args[i]
			}
			return listTags(repo, pattern)
		case "-a":
			annotated = true
		case "-d":
			delete = true
			if i+1 < len(args) {
				i++
				tagName = args[i]
			}
		case "-m":
			if i+1 < len(args) {
				i++
				message = args[i]
			}
		default:
			if strings.HasPrefix(args[i], "-m") {
				message = args[i][2:]
			} else if !strings.HasPrefix(args[i], "-") {
				tagName = args[i]
			} else {
				return nil, ErrUnsupported
			}
		}
	}

	if delete {
		if tagName == "" {
			return nil, ErrUnsupported
		}
		err := repo.DeleteTag(tagName)
		if err != nil {
			return &ExecResult{
				Stderr:   fmt.Sprintf("error: tag '%s' not found.\n", tagName),
				ExitCode: 1,
			}, nil
		}
		return &ExecResult{
			Stdout: fmt.Sprintf("Deleted tag '%s'\n", tagName),
		}, nil
	}

	if tagName == "" {
		return listTags(repo, "")
	}

	head, err := repo.Head()
	if err != nil {
		return nil, ErrUnsupported
	}

	if annotated || message != "" {
		// Create annotated tag
		_, err = repo.CreateTag(tagName, head.Hash(), &gogit.CreateTagOptions{
			Message: message,
			Tagger: &object.Signature{
				Name:  "ycode",
				Email: "ycode@local",
				When:  time.Now(),
			},
		})
	} else {
		// Create lightweight tag
		_, err = repo.CreateTag(tagName, head.Hash(), nil)
	}
	if err != nil {
		return &ExecResult{
			Stderr:   fmt.Sprintf("fatal: tag '%s' already exists\n", tagName),
			ExitCode: 128,
		}, nil
	}

	return &ExecResult{Stdout: ""}, nil
}

func listTags(repo *gogit.Repository, pattern string) (*ExecResult, error) {
	iter, err := repo.Tags()
	if err != nil {
		return nil, ErrUnsupported
	}

	var b strings.Builder
	err = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		if pattern != "" {
			matched, _ := filepath.Match(pattern, name)
			if !matched {
				return nil
			}
		}
		b.WriteString(name)
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: b.String()}, nil
}

func nativeFetch(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	fetchAll := false
	noTags := false
	var positionals []string

	for _, arg := range args {
		switch arg {
		case "--all":
			fetchAll = true
		case "--no-tags":
			noTags = true
		case "--tags", "--prune":
			// Accept common flags
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, ErrUnsupported
			}
			positionals = append(positionals, arg)
		}
	}

	if fetchAll {
		// Fetch all remotes
		remotes, err := repo.Remotes()
		if err != nil {
			return nil, ErrUnsupported
		}
		for _, remote := range remotes {
			err := remote.Fetch(&gogit.FetchOptions{})
			if err != nil && err != gogit.NoErrAlreadyUpToDate {
				// Ignore auth errors etc — fall through
				return nil, ErrUnsupported
			}
		}
		return &ExecResult{Stdout: ""}, nil
	}

	// positionals: [<remote-or-path> [<refspec>...]]. A second-or-later
	// positional is an explicit refspec; the remote may be a configured
	// name or a local path / URL (typed Fetch routes accordingly).
	remote := "origin"
	var refSpecs []string
	if len(positionals) >= 1 {
		remote = positionals[0]
	}
	if len(positionals) >= 2 {
		refSpecs = positionals[1:]
	}

	res, err := Fetch(FetchOptions{RepoPath: dir, Remote: remote, RefSpecs: refSpecs, NoTags: noTags})
	if err != nil {
		// Auth errors, network issues, bad remote — fall through to host git.
		return nil, ErrUnsupported
	}
	_ = res
	return &ExecResult{Stdout: ""}, nil
}

func nativeGrep(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Parse flags
	caseInsensitive := false
	filesOnly := false
	lineNumbers := false
	var pathSpecs []string
	var pattern string
	afterDash := false

	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			afterDash = true
			continue
		}
		if afterDash {
			pathSpecs = append(pathSpecs, args[i])
			continue
		}
		switch args[i] {
		case "-i":
			caseInsensitive = true
		case "-l":
			filesOnly = true
		case "-n":
			lineNumbers = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return nil, ErrUnsupported
			}
			if pattern == "" {
				pattern = args[i]
			} else {
				// Additional non-flag args after pattern are pathspecs
				pathSpecs = append(pathSpecs, args[i])
			}
		}
	}

	// The search is a literal substring match over HEAD's tree. That is
	// host git's answer only when the pattern has no BRE metacharacter
	// and tracked files on disk equal HEAD (git greps the working tree).
	if pattern == "" || strings.ContainsAny(pattern, `\.^$*[]`) {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	if wt, werr := repo.Worktree(); werr != nil {
		return nil, ErrUnsupported
	} else if ps, perr := loadPorcelainState(repo, wt); perr != nil || ps.unsupported != "" || len(ps.tracked) > 0 {
		return nil, ErrUnsupported
	}

	head, err := repo.Head()
	if err != nil {
		return nil, ErrUnsupported
	}

	commit, err := repo.CommitObject(head.Hash())
	if err != nil {
		return nil, ErrUnsupported
	}

	tree, err := commit.Tree()
	if err != nil {
		return nil, ErrUnsupported
	}

	// Build search pattern
	searchPattern := pattern
	if caseInsensitive {
		searchPattern = strings.ToLower(pattern)
	}

	var b strings.Builder
	matchedFiles := make(map[string]bool)

	err = tree.Files().ForEach(func(f *object.File) error {
		// Check pathspec
		if len(pathSpecs) > 0 {
			matched := false
			for _, ps := range pathSpecs {
				if m, _ := filepath.Match(ps, f.Name); m {
					matched = true
					break
				}
				if strings.HasPrefix(f.Name, ps) {
					matched = true
					break
				}
			}
			if !matched {
				return nil
			}
		}

		// Skip binary files
		// git reports "Binary file X matches"; not rendered here.
		if isBinary, _ := f.IsBinary(); isBinary {
			if c, cerr := f.Contents(); cerr == nil {
				if caseInsensitive {
					c = strings.ToLower(c)
				}
				if strings.Contains(c, searchPattern) {
					return ErrUnsupported
				}
			}
			return nil
		}

		contents, err := f.Contents()
		if err != nil {
			return nil
		}

		lines := strings.Split(contents, "\n")
		for lineNum, line := range lines {
			searchLine := line
			if caseInsensitive {
				searchLine = strings.ToLower(line)
			}
			if strings.Contains(searchLine, searchPattern) {
				if quotePath(f.Name, false) != f.Name {
					return ErrUnsupported // host git C-quotes this name
				}
				if filesOnly {
					if !matchedFiles[f.Name] {
						matchedFiles[f.Name] = true
						b.WriteString(f.Name)
						b.WriteByte('\n')
					}
					return nil
				}
				if lineNumbers {
					b.WriteString(fmt.Sprintf("%s:%d:%s\n", f.Name, lineNum+1, line))
				} else {
					b.WriteString(fmt.Sprintf("%s:%s\n", f.Name, line))
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, ErrUnsupported
	}

	if b.Len() == 0 {
		return &ExecResult{Stdout: "", ExitCode: 1}, nil
	}

	return &ExecResult{Stdout: b.String()}, nil
}

// nativeLsFiles reads the index (not HEAD), in index order, like host
// git: the default/--cached listing, -s/--stage, -m/--modified,
// -d/--deleted and -o/--others with --exclude-standard, each with -z.
// Pathspecs, --ignored, a bare --others (which must list ignored files)
// and an index with merge conflicts decline.
func nativeLsFiles(_ context.Context, dir string, args []string) (*ExecResult, error) {
	z, stage, modified, deleted, others, excludeStd, cached := false, false, false, false, false, false, false
	for _, arg := range args {
		switch arg {
		case "--cached", "-c":
			cached = true
		case "-z":
			z = true
		case "--stage", "-s":
			stage = true
		case "--modified", "-m":
			modified = true
		case "--deleted", "-d":
			deleted = true
		case "--others", "-o":
			others = true
		case "--exclude-standard":
			excludeStd = true
		default:
			return nil, ErrUnsupported
		}
	}
	modes := 0
	for _, on := range []bool{cached || stage, modified, deleted, others} {
		if on {
			modes++
		}
	}
	if modes > 1 || (others && !excludeStd) {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}
	ps, err := loadPorcelainState(repo, wt)
	if err != nil || ps.unsupported == "unmerged index entries" {
		return nil, ErrUnsupported
	}
	if z && stage {
		return nil, ErrUnsupported
	}
	name := func(p string) string {
		if z {
			return p + "\x00"
		}
		return quotePath(p, false) + "\n"
	}

	var b strings.Builder
	switch {
	case others:
		for _, p := range ps.untracked {
			b.WriteString(name(p))
		}
	case modified, deleted:
		for _, e := range ps.idx.Entries {
			y := ps.tracked[e.Name][1]
			if (deleted && y == 'D') || (modified && (y == 'D' || y == 'M')) {
				b.WriteString(name(e.Name))
			}
		}
	default:
		for _, e := range ps.idx.Entries {
			if stage {
				fmt.Fprintf(&b, "%06o %s %d\t", uint32(e.Mode), e.Hash, e.Stage)
			}
			b.WriteString(name(e.Name))
		}
	}
	return &ExecResult{Stdout: b.String()}, nil
}

// nativeBlame declines: host git's default blame format (boundary "^",
// author name, full timestamp, column padding) is not reproduced, and a
// lookalike is worse than a loud decline. The typed Blame API serves
// structured lines.
func nativeBlame(_ context.Context, _ string, _ []string) (*ExecResult, error) {
	return nil, ErrUnsupported
}

// parseGitDate attempts to parse a date string in common git formats.
// Dates without explicit timezone are interpreted in the local timezone,
// matching git's default behavior.
func parseGitDate(s string) (time.Time, error) {
	formats := []string{
		"2006-01-02",
		"2006-01-02T15:04:05",
		"Jan 2 2006",
		"2 Jan 2006",
	}
	// Try local timezone first (matches git behavior)
	for _, f := range formats {
		if t, err := time.ParseInLocation(f, s, time.Local); err == nil {
			return t, nil
		}
	}
	// Try RFC3339 which includes timezone
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("unsupported date format: %s", s)
}

// branchContainsCommit checks if the branch tip is an ancestor of (or equal to)
// the given commit, meaning the branch contains that commit in its history.
func branchContainsCommit(repo *gogit.Repository, branchTip, target plumbing.Hash) bool {
	if branchTip == target {
		return true
	}
	// Walk backwards from branchTip looking for target
	iter, err := repo.Log(&gogit.LogOptions{From: branchTip})
	if err != nil {
		return false
	}
	found := false
	_ = iter.ForEach(func(c *object.Commit) error {
		if c.Hash == target {
			found = true
			return fmt.Errorf("stop")
		}
		return nil
	})
	return found
}
