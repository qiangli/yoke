package weave

// THE WRITE-TIME ISOLATION GUARD — say it before the damage, not after.
//
// weave already detects that the live checkout moved while a run held its
// workspace: `IsolationViolated` lands on the item and `weave pull` refuses the
// merge without --force. That detection is correct and it is also TOO LATE. It
// is computed on the READ path — during finalize, status, and list — so the
// person who caused it finds out when they next happen to run `weave list`, for
// some unrelated reason, possibly days later. The run is already unmergeable by
// then, and the only remaining choices are --force or salvage.
//
// This was found by doing it. An agent working the steward seat committed three
// files to the live bashy checkout while run #35 held a workspace; nothing said
// a word at commit time, and the violation surfaced later in an unrelated
// `weave list`. Detection after the fact is a diagnosis; what was wanted is a
// warning.
//
// bashy is the shell, so it is the one process that sees the mutating command
// before it runs. This file is the query that makes that possible; the
// middleware lives in bashy (internal/agentos/weaveguard.go), the same split as
// the advisor and the learn hook.
//
// # Why `working` and not `submitted`, by default
//
// A `submitted` run is also violated by a live-checkout change — but it can sit
// unmerged for days awaiting a steward, and on a busy tree there are usually
// several. Warning on those would fire on nearly every commit, and the
// advisor's own hard-won rule applies: hints that arrive when nothing is wrong
// are how people learn to ignore hints, and once they do the system is worse
// than one that never spoke.
//
// So the default is the RACE — a run that is executing right now, where the
// change genuinely collides with work in flight. `Strict` widens it to every
// unmerged holder for a caller that wants the full picture.
//
// # It reports; it never blocks
//
// Same posture as the advisor: this returns facts and the caller prints one
// line. Refusing the commit would be wrong — the operator may have every reason
// to edit the live checkout, and a guard that stops work gets disabled, at
// which point it protects nothing.

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Holder is one live run holding a repo.
type Holder struct {
	ID    int64  `json:"id"`
	Title string `json:"title,omitempty"`
	State string `json:"state"`
	Tool  string `json:"tool,omitempty"`
	// Workspace is the clone the run is working in. Reported so the warning
	// can point at where the work actually is.
	Workspace string `json:"workspace,omitempty"`
}

// HoldersQuery selects which holders count.
type HoldersQuery struct {
	// Strict includes runs awaiting `weave pull` as well as running ones. The
	// default is running-only; see the package comment for why.
	Strict bool
	// StateRoot overrides ~/.bashy/weave. Test seam.
	StateRoot string
}

// HoldersOf reports the live weave runs holding repoRoot.
//
// It returns nothing — never an error — when the state directory is absent,
// unreadable, or holds no queue for this repo. A guard that fails loudly
// because it could not read an optional store would turn an advisory into an
// obstacle, and this runs ahead of ordinary git commands.
//
// Cost matters: this sits on a path that runs before mutating git commands, so
// it stats one directory and parses only the queues whose Root matches.
func HoldersOf(repoRoot string, q HoldersQuery) []Holder {
	repoRoot = canonicalRepoRoot(repoRoot)
	if repoRoot == "" {
		return nil
	}
	root := q.StateRoot
	if root == "" {
		var err error
		if root, err = weaveStateRootReadOnly(); err != nil || root == "" {
			return nil
		}
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}

	var out []Holder
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		queue, err := loadWeaveQueue(filepath.Join(root, e.Name()))
		if err != nil || queue == nil {
			continue
		}
		if canonicalRepoRoot(queue.Root) != repoRoot {
			continue
		}
		for _, it := range queue.Items {
			if it == nil || !holderState(it.State, q.Strict) {
				continue
			}
			out = append(out, Holder{
				ID: it.ID, Title: it.Title, State: it.State,
				Tool: it.Tool, Workspace: it.Workspace,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// holderState reports whether a run in this state is holding the repo in a way
// a live-checkout edit would damage.
func holderState(state string, strict bool) bool {
	switch state {
	case "working":
		// Executing right now: an edit races it. Always reported.
		return true
	case "submitted":
		// Awaiting `weave pull`. Still violated by an edit, but common and
		// long-lived, so it is opt-in rather than the default noise floor.
		return strict
	default:
		return false
	}
}

// canonicalRepoRoot normalises a repo path for comparison. Symlinked temp dirs
// (/var vs /private/var on macOS) would otherwise make an identical repo look
// like two, and the guard would silently never fire — the worst failure mode
// available to it, because it looks exactly like "nothing is wrong".
func canonicalRepoRoot(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		p = resolved
	}
	return filepath.Clean(p)
}

// weaveStateRootReadOnly resolves the weave state root WITHOUT creating it.
//
// It deliberately delegates to weaveStateRoot rather than rebuilding the path:
// a guard that looked somewhere weave does not write would find no holders and
// report all-clear, which is the worst failure available to it — indistinguishable
// from "nothing is wrong". There is no env override here because weave itself
// has none; HoldersQuery.StateRoot is the test seam.
//
// The caller's write paths mkdir this; a read-only query must not bring a state
// directory into existence on a host that has never run weave.
func weaveStateRootReadOnly() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return weaveStateRoot(home), nil
}

// RepoRootOf walks up from dir to the nearest .git, returning "" outside a repo.
//
// Exported because the guard's caller is bashy, which must key on the REPO root
// rather than the project root: weave isolation is a property of one checkout,
// and a queue records the repo it serves. A project-wide key would warn about a
// sibling repo's runs — the kind of false positive that gets a guard disabled.
func RepoRootOf(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, ".git")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return ""
		}
		dir = parent
	}
}
