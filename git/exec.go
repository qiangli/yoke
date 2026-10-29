package git

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ExecResult is the outcome of one Exec invocation, shaped like a
// process run so argv-style callers (shell builtins, tool executors)
// can relay it without translation.
type ExecResult struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// ErrUnsupported is returned by Exec (and the per-subcommand handlers)
// when the requested subcommand or flag combination is not implemented
// by the pure-Go layer. Callers with a richer fallback (a host git
// binary, a container) should treat it as "try the next tier"; callers
// without one should surface a clear not-supported error.
var ErrUnsupported = errors.New("git: not supported by the pure-Go implementation")

// execFunc handles one subcommand. args excludes the subcommand name.
type execFunc func(ctx context.Context, dir string, args []string) (*ExecResult, error)

// execHandlers maps subcommand → handler. The set mirrors what the
// handlers actually implement; unknown flags inside a handler return
// ErrUnsupported rather than guessing.
var execHandlers = map[string]execFunc{
	// read / inspect
	"rev-parse":    nativeRevParse,
	"status":       nativeStatus,
	"log":          nativeLog,
	"diff":         nativeDiff,
	"merge-base":   nativeMergeBase,
	"rev-list":     nativeRevList,
	"config":       nativeConfig,
	"grep":         nativeGrep,
	"ls-files":     nativeLsFiles,
	"blame":        nativeBlame,
	"for-each-ref": nativeForEachRef,
	"remote":       nativeRemote,
	"show":         nativeShow,
	// local writes
	"init":     nativeInit,
	"add":      nativeAdd,
	"clean":    nativeClean,
	"commit":   nativeCommit,
	"branch":   nativeBranch,
	"checkout": nativeCheckout,
	"reset":    nativeReset,
	"stash":    nativeStash,
	"worktree": nativeWorktree,
	"merge":    nativeMerge,
	"tag":      nativeTag,
	"rm":       nativeRm,
	// network
	"fetch": nativeFetch,
	"push":  nativePush,
	"pull":  nativePull,
	"clone": nativeClone,
	// history surgery (linear, conflict-free cases only)
	"cherry":       nativeCherry,
	"cherry-pick":  nativeCherryPick,
	"revert":       nativeRevert,
	"rebase":       nativeRebase,
	"apply":        nativeApply,
	"format-patch": nativeFormatPatch,
	// plumbing
	"cat-file":     nativeCatFile,
	"hash-object":  nativeHashObject,
	"read-tree":    nativeReadTree,
	"write-tree":   nativeWriteTree,
	"commit-tree":  nativeCommitTree,
	"symbolic-ref": nativeSymbolicRef,
	"update-ref":   nativeUpdateRef,
	"diff-tree":    nativeDiffTree,
	"ls-tree":      nativeLsTree,
	"show-ref":     nativeShowRef,
}

// ExecCommands returns the subcommand names Exec recognizes, sorted.
// Callers that register per-subcommand dispatch (ycode's tool
// executor) use this to stay in lockstep with the handler map.
func ExecCommands() []string {
	names := make([]string, 0, len(execHandlers))
	for name := range execHandlers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Exec runs a git command argv-style: args[0] is the subcommand, the
// rest its arguments, dir the working directory. Returns ErrUnsupported
// when the subcommand (or a flag combination inside it) is not
// implemented natively. Global options preceding the subcommand
// (-c key=value, -C dir, --no-pager, …) are parsed and applied.
func Exec(ctx context.Context, dir string, args []string) (*ExecResult, error) {
	var overrides map[string]string
	i := 0
	for i < len(args) {
		a := args[i]
		if a == "-c" {
			if i+1 >= len(args) {
				return &ExecResult{Stderr: "error: -c requires a string of the form 'name=value'\n", ExitCode: 128}, nil
			}
			eq := strings.IndexByte(args[i+1], '=')
			if eq < 0 {
				return &ExecResult{Stderr: "error: -c requires a string of the form 'name=value'\n", ExitCode: 128}, nil
			}
			if overrides == nil {
				overrides = make(map[string]string)
			}
			overrides[args[i+1][:eq]] = args[i+1][eq+1:]
			i += 2
			continue
		}
		if strings.HasPrefix(a, "-c") && len(a) > 2 {
			rest := a[2:]
			eq := strings.IndexByte(rest, '=')
			if eq < 0 {
				return &ExecResult{Stderr: "error: -c requires a string of the form 'name=value'\n", ExitCode: 128}, nil
			}
			if overrides == nil {
				overrides = make(map[string]string)
			}
			overrides[rest[:eq]] = rest[eq+1:]
			i++
			continue
		}
		if a == "-C" {
			if i+1 >= len(args) {
				return &ExecResult{Stderr: "error: -C requires a directory\n", ExitCode: 128}, nil
			}
			cdir := args[i+1]
			if filepath.IsAbs(cdir) {
				dir = cdir
			} else if dir != "" {
				dir = filepath.Join(dir, cdir)
			} else {
				dir = cdir
			}
			i += 2
			continue
		}
		if a == "--no-pager" || a == "-P" || a == "--paginate" || a == "-p" || a == "--no-optional-locks" || a == "--no-advice" {
			i++
			continue
		}
		break
	}
	args = args[i:]
	if len(overrides) > 0 {
		ctx = WithConfigOverrides(ctx, overrides)
	}
	if len(args) == 0 {
		return &ExecResult{Stderr: "usage: " + CLIName + " <subcommand> [args...]\n", ExitCode: 2}, nil
	}
	h, ok := execHandlers[args[0]]
	if !ok {
		return nil, ErrUnsupported
	}
	return h(ctx, dir, args[1:])
}

// nativeInit implements `git init [-q | --quiet] [--bare] [-b <branch> | --initial-branch=<branch>] [directory]`.
func nativeInit(_ context.Context, dir string, args []string) (*ExecResult, error) {
	quiet := false
	bare := false
	initialBranch := ""
	var positional []string

	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "-q" || a == "--quiet":
			quiet = true
		case a == "--bare":
			bare = true
		case (a == "-b" || a == "--initial-branch") && i+1 < len(args):
			i++
			initialBranch = args[i]
		case strings.HasPrefix(a, "--initial-branch="):
			initialBranch = strings.TrimPrefix(a, "--initial-branch=")
		case strings.HasPrefix(a, "-"):
			return nil, ErrUnsupported
		default:
			positional = append(positional, a)
		}
	}

	if len(positional) > 1 {
		return nil, ErrUnsupported
	}

	target := dir
	if len(positional) == 1 {
		target = positional[0]
		if !filepath.IsAbs(target) {
			if dir != "" {
				target = filepath.Join(dir, target)
			}
		}
	} else if target == "" {
		target = "."
	}

	res, err := Init(InitOptions{
		Path:          target,
		Quiet:         quiet,
		Bare:          bare,
		InitialBranch: initialBranch,
	})
	if err != nil {
		return nil, err
	}

	if quiet {
		return &ExecResult{Stdout: ""}, nil
	}
	return &ExecResult{Stdout: res.Message + "\n"}, nil
}

// nativePull delegates to the typed Pull (fetch + fast-forward with
// git-like up-to-date/ahead semantics). Failures — divergence, auth,
// network — return ErrUnsupported so callers with a host git can let
// it take over (it can do real merges); pure-Go callers see Pull's
// clear error by calling the typed API directly.
func nativePull(_ context.Context, dir string, args []string) (*ExecResult, error) {
	var remote, branch string
	for _, a := range args {
		switch {
		case strings.HasPrefix(a, "-"):
			return nil, ErrUnsupported
		case remote == "":
			remote = a
		case branch == "":
			branch = a
		default:
			return nil, ErrUnsupported
		}
	}
	res, err := Pull(PullOptions{RepoPath: dir, Remote: remote, Branch: branch})
	if err != nil {
		return nil, ErrUnsupported
	}
	return &ExecResult{Stdout: res.Message + "\n"}, nil
}

// nativeClone delegates to the typed Clone. Same fallback contract as
// nativePull: any failure yields ErrUnsupported.
func nativeClone(_ context.Context, dir string, args []string) (*ExecResult, error) {
	opts := CloneOptions{}
	rest := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--depth" && i+1 < len(args):
			i++
			var n int
			if _, err := fmt.Sscanf(args[i], "%d", &n); err != nil || n <= 0 {
				return nil, ErrUnsupported
			}
			opts.Depth = n
		case args[i] == "--branch" || args[i] == "-b":
			if i+1 >= len(args) {
				return nil, ErrUnsupported
			}
			i++
			opts.Branch = args[i]
		case args[i] == "--single-branch":
			opts.SingleBranch = true
		case args[i] == "--local" || args[i] == "-l" || args[i] == "--no-hardlinks":
			// Local-path clone optimizations. go-git always copies objects
			// into the new repo's own store (no hardlinks, no shared
			// alternates), so these are effective no-ops — accept them so
			// `git clone --local --no-hardlinks <path>` (weave's workspace
			// allocation) doesn't fall back to host git.
		case strings.HasPrefix(args[i], "-"):
			return nil, ErrUnsupported
		default:
			rest = append(rest, args[i])
		}
	}
	if len(rest) == 0 || len(rest) > 2 {
		return nil, ErrUnsupported
	}
	opts.URL = rest[0]
	if len(rest) == 2 {
		opts.Path = rest[1]
	} else {
		opts.Path = strings.TrimSuffix(filepath.Base(rest[0]), ".git")
	}
	if !filepath.IsAbs(opts.Path) {
		opts.Path = filepath.Join(dir, opts.Path)
	}
	res, err := Clone(opts)
	if err != nil {
		return nil, ErrUnsupported
	}
	return &ExecResult{Stdout: res.Message + "\n"}, nil
}

// relToRepoRoot resolves arg (relative to dir) into a path relative to
// the repository root. Both sides are symlink-resolved first: go-git
// reports the physical root, while callers often pass logical paths
// (macOS tempdirs live under /var → /private/var), and a naive
// filepath.Rel across that mismatch fabricates ../../ paths.
func relToRepoRoot(root, dir, arg string) string {
	absDir, err := filepath.Abs(dir)
	if err != nil {
		return arg
	}
	if r, err := filepath.EvalSymlinks(absDir); err == nil {
		absDir = r
	}
	if r, err := filepath.EvalSymlinks(root); err == nil {
		root = r
	}
	rel, err := filepath.Rel(root, filepath.Join(absDir, arg))
	if err != nil {
		return arg
	}
	return rel
}
