package weave

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// weaveOrphanWorkspace is a directory under the queue's workspaces/ (or the
// legacy sandboxes/) that no queue item claims.
type weaveOrphanWorkspace struct {
	Path string
	// Hold is the reason this directory must not be swept, empty when it is
	// safe to remove. A held orphan is REPORTED, never removed.
	Hold string
}

// weaveOrphanWorkspaceTargets enumerates workspace directories that no queue
// item points at.
//
// prune's item loop structurally cannot reach these. It iterates q.Items, so a
// directory nothing claims is invisible to every flag, --stale included, since
// --stale also only widens which ITEMS qualify. Nothing ever enumerates the
// directory itself, so an unclaimed workspace is unreclaimable for the life of
// the queue: it is not a slow leak, it is a permanent one.
//
// Measured before the fix: two stores held sibling-dep mirror clones
// (coreutils, filebrowser, readline, sh) created for isolated builds and left
// behind when their runs ended. 266 MB across six directories, none of them
// named issue-N, none claimed by an item, none removable by any weave command.
//
// This is the same leak class weaveWorkspaceOwner already documents having
// produced 237 empty roots. That fix stopped new ROOTS from forking; orphan
// WORKSPACES under an existing root stayed unswept.
//
// Ownership is decided by PATH and never by name. An item's recorded workspace
// is the claim, so a directory called anything at all is protected while an
// item points at it, and the issue-N convention is not load-bearing here.
func weaveOrphanWorkspaceTargets(queueDir string, q *weaveQueue) ([]weaveOrphanWorkspace, error) {
	absQueue, err := filepath.Abs(queueDir)
	if err != nil {
		return nil, err
	}

	claimed := map[string]bool{}
	if q != nil {
		for _, it := range q.Items {
			if it == nil || it.Workspace == "" {
				continue
			}
			if abs, err := filepath.Abs(it.Workspace); err == nil {
				claimed[filepath.Clean(abs)] = true
			}
		}
	}

	var out []weaveOrphanWorkspace
	// Both containers safeRemoveWorkspace accepts, so a legacy clone created
	// before the sandbox->workspace rename is swept on the same terms.
	for _, parent := range []string{
		filepath.Join(absQueue, "workspaces"),
		filepath.Join(absQueue, "sandboxes"),
	} {
		entries, err := os.ReadDir(parent)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return nil, err
		}
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			path := filepath.Join(parent, entry.Name())
			if claimed[filepath.Clean(path)] {
				continue
			}
			out = append(out, weaveOrphanWorkspace{
				Path: path,
				Hold: weaveOrphanWorkspaceHold(path),
			})
		}
	}
	return out, nil
}

// weaveOrphanWorkspaceHold reports why an unclaimed workspace still holds work,
// or "" when it is safe to remove.
//
// The guard has to be more conservative than the item path's, not less. An item
// carries its clone-point BaseSHA, so weaveUnmergedAhead can count exactly what
// would be lost; an orphan carries nothing, and there is no record of what it
// was ever branched from. So the presence of an agent branch is treated as work
// outright rather than guessed at: a branch that turns out to be merged costs a
// --force, while a wrong guess in the other direction costs the commits, which
// exist ONLY inside this clone.
func weaveOrphanWorkspaceHold(path string) string {
	var parts []string
	if dirty, dirtyFiles, untracked := weaveMeasureDirtiness(path); dirty || untracked > 0 {
		if n := dirtyFiles + untracked; n > 0 {
			parts = append(parts, fmt.Sprintf("%d uncommitted file(s)", n))
		}
	}
	if n := weaveOrphanAgentBranches(path); n > 0 {
		parts = append(parts, fmt.Sprintf("%d agent branch(es) that exist only here", n))
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, "; ") + " (--force to delete anyway)"
}

// weaveForcedSalvageCommitMessage labels a tree committed only so that --force
// can preserve it.
//
// It must not read like the run's own conclusion. The commit is the operator's
// act, made at teardown to give uncommitted work a ref to hang from, and the
// next reader needs to know the work was never submitted or gated.
func weaveForcedSalvageCommitMessage(it *weaveItem) string {
	title := ""
	if it != nil {
		title = strings.TrimSpace(it.Title)
	}
	msg := "weave: preserve uncommitted tree before forced teardown"
	if title != "" {
		msg += "\n\n" + title
	}
	return msg + "\n\nThis tree was committed by `--force` so it could be preserved as a\n" +
		"salvage ref. It was NOT submitted, reviewed or gated: the run ended\n" +
		"without committing it. Treat it as work in progress that needs a\n" +
		"human decision, not as a finished change."
}

// weaveWorkspacePresent reports whether an item's workspace still exists on
// disk.
//
// An agent branch lives ONLY inside its workspace clone until `weave pull`
// fetches it, so once the workspace is gone the branch is gone with it. Every
// warning that offers to merge, inspect or diff that branch is therefore
// unactionable and must not be printed as if it were pending work.
func weaveWorkspacePresent(it *weaveItem) bool {
	if it == nil || it.Workspace == "" {
		return false
	}
	st, err := os.Stat(it.Workspace)
	return err == nil && st.IsDir()
}

// weaveOrphanAgentBranches counts agent/* branches in an unclaimed clone. A
// non-repository (or an unreadable one) counts zero: there is no branch to lose.
func weaveOrphanAgentBranches(path string) int {
	cmd := exec.Command("git", "-C", path, "for-each-ref", "--format=%(refname)", "refs/heads/agent/")
	out, err := cmd.Output()
	if err != nil {
		return 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n
}
