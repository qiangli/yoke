package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// openRepo opens the git repository at or above dir.
func openRepo(dir string) (*gogit.Repository, error) {
	return gogit.PlainOpenWithOptions(dir, &gogit.PlainOpenOptions{
		DetectDotGit: true,
	})
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

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	switch args[0] {
	case "--is-inside-work-tree":
		return &ExecResult{Stdout: "true\n"}, nil

	case "--show-toplevel":
		wt, err := repo.Worktree()
		if err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{Stdout: wt.Filesystem.Root() + "\n"}, nil

	case "--abbrev-ref":
		if len(args) < 2 || args[1] != "HEAD" {
			return nil, ErrUnsupported
		}
		head, err := repo.Head()
		if err != nil {
			return nil, ErrUnsupported
		}
		if head.Name().IsBranch() {
			return &ExecResult{Stdout: head.Name().Short() + "\n"}, nil
		}
		return &ExecResult{Stdout: "HEAD\n"}, nil

	case "--short":
		if len(args) < 2 || args[1] != "HEAD" {
			return nil, ErrUnsupported
		}
		head, err := repo.Head()
		if err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{Stdout: head.Hash().String()[:7] + "\n"}, nil

	case "--verify":
		if len(args) < 2 {
			return nil, ErrUnsupported
		}
		ref := args[1]
		if ref == "HEAD" {
			head, err := repo.Head()
			if err != nil {
				return &ExecResult{Stderr: "fatal: not a git repository\n", ExitCode: 128}, nil
			}
			return &ExecResult{Stdout: head.Hash().String() + "\n"}, nil
		}
		// Try resolving as a reference
		hash, err := repo.ResolveRevision(plumbing.Revision(ref))
		if err != nil {
			return &ExecResult{Stderr: fmt.Sprintf("fatal: Needed a single revision\n"), ExitCode: 128}, nil
		}
		return &ExecResult{Stdout: hash.String() + "\n"}, nil

	default:
		return nil, ErrUnsupported
	}
}

func nativeStatus(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	status, err := wt.Status()
	if err != nil {
		return nil, ErrUnsupported
	}

	// go-git's Status.String() already produces porcelain-style output.
	// Handle --short and --porcelain which produce the same format.
	return &ExecResult{Stdout: status.String()}, nil
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

func nativeDiff(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	// Parse flags
	cached := false
	stat := false
	quiet := false
	for _, arg := range args {
		switch arg {
		case "--cached", "--staged":
			cached = true
		case "--stat":
			stat = true
		case "--quiet":
			quiet = true
		default:
			// Path specs, commit ranges, etc. — fall through
			return nil, ErrUnsupported
		}
	}

	// --quiet reports only via exit status: 0 = no differences, 1 =
	// differences. Combined with --cached this is the "are there staged
	// changes?" predicate loom uses to skip empty commits.
	if quiet {
		status, err := wt.Status()
		if err != nil {
			return nil, ErrUnsupported
		}
		for _, fs := range status {
			if cached {
				// Staged column: index vs HEAD.
				if fs.Staging != gogit.Unmodified && fs.Staging != gogit.Untracked {
					return &ExecResult{ExitCode: 1}, nil
				}
			} else {
				// Unstaged column: worktree vs index.
				if fs.Worktree != gogit.Unmodified && fs.Worktree != gogit.Untracked {
					return &ExecResult{ExitCode: 1}, nil
				}
			}
		}
		return &ExecResult{ExitCode: 0}, nil
	}

	_ = stat // stat formatting is complex; fall through for now
	if stat {
		return nil, ErrUnsupported
	}

	if cached {
		// Staged changes: diff between HEAD and index
		return nil, ErrUnsupported // go-git staged diff is complex
	}

	// Unstaged changes: diff between index and working tree
	status, err := wt.Status()
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
			status, err := wt.Status()
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
	for i := 0; i < len(args); i++ {
		if args[i] == "-m" && i+1 < len(args) {
			i++
			msg = args[i]
		} else if strings.HasPrefix(args[i], "-m") {
			msg = args[i][2:]
		} else if args[i] == "--allow-empty" || args[i] == "--no-verify" {
			// Accepted flags, no-op
		} else {
			return nil, ErrUnsupported
		}
	}

	if msg == "" {
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

	opts := &gogit.CommitOptions{}
	// Provide an explicit author when git config is not available (e.g. in containers).
	// Try repo-level config first, then fall back to a default signature.
	if sig := resolveAuthor(repo); sig != nil {
		opts.Author = sig
	}

	hash, err := wt.Commit(msg, opts)
	if err != nil {
		return nil, ErrUnsupported
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

	// Parse list flags
	listRemote := false
	listAll := false
	verbose := false
	containsRef := ""
	nonFlagArgs := []string{}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-v":
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
		default:
			if strings.HasPrefix(args[i], "--contains=") {
				containsRef = strings.TrimPrefix(args[i], "--contains=")
			} else if strings.HasPrefix(args[i], "-") && args[i] != "-D" && args[i] != "-d" {
				nonFlagArgs = append(nonFlagArgs, args[i])
			} else {
				nonFlagArgs = append(nonFlagArgs, args[i])
			}
		}
	}
	_ = verbose

	// Listing mode: no non-flag args, or only -v/-r/-a/--contains
	isListMode := len(nonFlagArgs) == 0 || (len(args) > 0 && (args[0] == "-v" || args[0] == "-r" || args[0] == "-a"))

	if isListMode && (len(nonFlagArgs) == 0) {
		head, _ := repo.Head()

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

		var b strings.Builder

		// List local branches
		if !listRemote {
			refs, err := repo.Branches()
			if err != nil {
				return nil, ErrUnsupported
			}
			err = refs.ForEach(func(ref *plumbing.Reference) error {
				if containsHash != nil {
					if !branchContainsCommit(repo, ref.Hash(), *containsHash) {
						return nil
					}
				}
				name := ref.Name().Short()
				if head != nil && ref.Name() == head.Name() {
					b.WriteString("* ")
				} else {
					b.WriteString("  ")
				}
				b.WriteString(name)
				b.WriteByte('\n')
				return nil
			})
			if err != nil {
				return nil, ErrUnsupported
			}
		}

		// List remote branches
		if listRemote || listAll {
			allRefs, err := repo.References()
			if err != nil {
				return nil, ErrUnsupported
			}
			err = allRefs.ForEach(func(ref *plumbing.Reference) error {
				if !ref.Name().IsRemote() {
					return nil
				}
				if containsHash != nil {
					if !branchContainsCommit(repo, ref.Hash(), *containsHash) {
						return nil
					}
				}
				b.WriteString("  ")
				b.WriteString(ref.Name().Short())
				b.WriteByte('\n')
				return nil
			})
			if err != nil {
				return nil, ErrUnsupported
			}
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

	// Handle -b (create and checkout)
	if args[0] == "-b" {
		if len(args) < 2 {
			return nil, ErrUnsupported
		}
		branchName := args[1]
		err := wt.Checkout(&gogit.CheckoutOptions{
			Branch: plumbing.NewBranchReferenceName(branchName),
			Create: true,
		})
		if err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{
			Stdout: fmt.Sprintf("Switched to a new branch '%s'\n", branchName),
		}, nil
	}

	// Handle -B (create the branch, or reset it to HEAD if it exists,
	// then switch to it). This is what loom's reference-clone setup uses.
	if args[0] == "-B" {
		if len(args) < 2 {
			return nil, ErrUnsupported
		}
		branchName := args[1]
		head, err := repo.Head()
		if err != nil {
			return nil, ErrUnsupported
		}
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(branchName), head.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			return nil, ErrUnsupported
		}
		if err := wt.Checkout(&gogit.CheckoutOptions{Branch: plumbing.NewBranchReferenceName(branchName)}); err != nil {
			return nil, ErrUnsupported
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
		})
		if err != nil {
			return nil, ErrUnsupported
		}
		return &ExecResult{
			Stdout: fmt.Sprintf("Switched to branch '%s'\n", branchName),
		}, nil
	}

	return nil, ErrUnsupported
}

func nativeRemote(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) < 2 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}
	remoteName := args[1]

	switch args[0] {
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

	// Check for specific file paths after "--"
	var files []string
	dashDash := false
	for _, arg := range args {
		if arg == "--" {
			dashDash = true
			continue
		}
		if arg == "HEAD" {
			continue
		}
		if strings.HasPrefix(arg, "-") {
			return nil, ErrUnsupported
		}
		if dashDash {
			files = append(files, arg)
		}
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
		status, err := wt.Status()
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

	// No specific files: mixed reset to HEAD (unstage everything)
	head, err := repo.Head()
	if err != nil {
		return nil, ErrUnsupported
	}
	err = wt.Reset(&gogit.ResetOptions{
		Mode:   gogit.MixedReset,
		Commit: head.Hash(),
	})
	if err != nil {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: ""}, nil
}

func nativeShow(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	// Resolve revision
	revision := args[0]
	hash, err := repo.ResolveRevision(plumbing.Revision(revision))
	if err != nil {
		return nil, ErrUnsupported
	}

	commit, err := repo.CommitObject(*hash)
	if err != nil {
		return nil, ErrUnsupported
	}

	var b strings.Builder
	b.WriteString("commit ")
	b.WriteString(commit.Hash.String())
	b.WriteByte('\n')
	b.WriteString("Author: ")
	b.WriteString(commit.Author.Name)
	b.WriteString(" <")
	b.WriteString(commit.Author.Email)
	b.WriteString(">\n")
	b.WriteString("Date:   ")
	b.WriteString(commit.Author.When.Format("Mon Jan 2 15:04:05 2006 -0700"))
	b.WriteString("\n\n    ")
	b.WriteString(strings.TrimRight(commit.Message, "\n"))
	b.WriteString("\n\n")

	// Generate patch
	var parentTree *object.Tree
	if commit.NumParents() > 0 {
		parent, err := commit.Parent(0)
		if err == nil {
			parentTree, _ = parent.Tree()
		}
	}

	commitTree, err := commit.Tree()
	if err != nil {
		// Return header without patch
		return &ExecResult{Stdout: b.String()}, nil
	}

	if parentTree == nil {
		// Root commit: diff against empty tree
		parentTree = &object.Tree{}
	}

	changes, err := parentTree.Diff(commitTree)
	if err != nil {
		// Return header without patch
		return &ExecResult{Stdout: b.String()}, nil
	}

	patch, err := changes.Patch()
	if err != nil {
		return &ExecResult{Stdout: b.String()}, nil
	}

	b.WriteString(patch.String())

	return &ExecResult{Stdout: b.String()}, nil
}

func nativeStash(_ context.Context, _ string, _ []string) (*ExecResult, error) {
	return nil, ErrUnsupported
}

func nativeWorktree(_ context.Context, _ string, _ []string) (*ExecResult, error) {
	return nil, ErrUnsupported
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
	lineNumbers := true
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

	if pattern == "" {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
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
		isBinary, _ := f.IsBinary()
		if isBinary {
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

func nativeLsFiles(_ context.Context, dir string, args []string) (*ExecResult, error) {
	repo, err := openRepo(dir)
	if err != nil {
		return nil, ErrUnsupported
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, ErrUnsupported
	}

	// Parse flags
	nullTerminated := false
	showModified := false
	showOthers := false
	showIgnored := false

	for _, arg := range args {
		switch arg {
		case "--cached", "-c":
			// default behavior
		case "-z":
			nullTerminated = true
		case "--modified", "-m":
			showModified = true
		case "--others", "-o":
			showOthers = true
		case "--ignored":
			showIgnored = true
		case "--exclude-standard":
			// accept but ignore (go-git doesn't have full .gitignore support)
		default:
			if strings.HasPrefix(arg, "-") {
				return nil, ErrUnsupported
			}
		}
	}

	_ = showIgnored // go-git doesn't easily support this, return empty for now

	status, err := wt.Status()
	if err != nil {
		return nil, ErrUnsupported
	}

	separator := "\n"
	if nullTerminated {
		separator = "\x00"
	}

	var b strings.Builder

	if showModified {
		for path, s := range status {
			if s.Worktree == gogit.Modified || s.Staging == gogit.Modified {
				b.WriteString(path)
				b.WriteString(separator)
			}
		}
		return &ExecResult{Stdout: b.String()}, nil
	}

	if showOthers {
		for path, s := range status {
			if s.Worktree == gogit.Untracked {
				b.WriteString(path)
				b.WriteString(separator)
			}
		}
		return &ExecResult{Stdout: b.String()}, nil
	}

	// Default: list all tracked files from the index
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

	err = tree.Files().ForEach(func(f *object.File) error {
		b.WriteString(f.Name)
		b.WriteString(separator)
		return nil
	})
	if err != nil {
		return nil, ErrUnsupported
	}

	return &ExecResult{Stdout: b.String()}, nil
}

func nativeBlame(_ context.Context, dir string, args []string) (*ExecResult, error) {
	if len(args) == 0 {
		return nil, ErrUnsupported
	}

	// Parse flags
	var filePath string
	startLine, endLine := 0, 0
	afterDash := false

	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			afterDash = true
			continue
		}
		if afterDash {
			filePath = args[i]
			continue
		}
		if args[i] == "-L" {
			if i+1 >= len(args) {
				return nil, ErrUnsupported
			}
			i++
			parts := strings.SplitN(args[i], ",", 2)
			if len(parts) != 2 {
				return nil, ErrUnsupported
			}
			fmt.Sscanf(parts[0], "%d", &startLine)
			fmt.Sscanf(parts[1], "%d", &endLine)
		} else if strings.HasPrefix(args[i], "-L") {
			rangeStr := args[i][2:]
			parts := strings.SplitN(rangeStr, ",", 2)
			if len(parts) != 2 {
				return nil, ErrUnsupported
			}
			fmt.Sscanf(parts[0], "%d", &startLine)
			fmt.Sscanf(parts[1], "%d", &endLine)
		} else if strings.HasPrefix(args[i], "-") {
			return nil, ErrUnsupported
		} else {
			filePath = args[i]
		}
	}

	if filePath == "" {
		return nil, ErrUnsupported
	}

	repo, err := openRepo(dir)
	if err != nil {
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

	blame, err := gogit.Blame(commit, filePath)
	if err != nil {
		return &ExecResult{
			Stderr:   fmt.Sprintf("fatal: no such path '%s'\n", filePath),
			ExitCode: 128,
		}, nil
	}

	var b strings.Builder
	for i, line := range blame.Lines {
		lineNum := i + 1
		if startLine > 0 && lineNum < startLine {
			continue
		}
		if endLine > 0 && lineNum > endLine {
			break
		}

		shortHash := line.Hash.String()[:8]
		author := line.Author
		date := line.Date.Format("2006-01-02")
		b.WriteString(fmt.Sprintf("%s (%s %s %4d) %s\n", shortHash, author, date, lineNum, line.Text))
	}

	return &ExecResult{Stdout: b.String()}, nil
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
