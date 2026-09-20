package weave

// `weave gc` — machine-wide garbage collection of weave state (Sprint 224 S5).
//
// Every other cleanup verb works on ONE queue, found from the current checkout.
// gc walks every queue under ~/.bashy/weave (and the legacy roots) and applies
// the same guarded rules to each, plus the one case only a walk can see: a
// queue whose repository no longer exists (deleted, or renamed so its
// path-hashed queue name no longer matches). Report is the default; --apply
// acts. It refuses on doubt and says so by path.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

type weaveGCRoot struct {
	Queue    string              `json:"queue"`
	Repo     string              `json:"repo,omitempty"`
	RepoGone bool                `json:"repo_gone"`
	Removed  []sprintPruneAction `json:"removed,omitempty"`
	Retained []string            `json:"retained,omitempty"`
	Refused  []string            `json:"refused,omitempty"`
}

var weaveRunBranchRE = regexp.MustCompile(`^agent/weave-issue-(\d+)(-reviewed)?$`)

func newWeaveGCCmd() *cobra.Command {
	var flags weaveOutputFlags
	var apply bool
	cmd := &cobra.Command{
		Use:   "gc",
		Short: "Report (or, with --apply, reclaim) orphan and legacy weave state across every queue on this machine",
		Long: `gc walks every weave queue under ~/.bashy/weave and the legacy roots.

Without --apply it CHANGES NOTHING. With --apply, for a queue whose repository
still exists it reclaims what the settled runs own (the same guarded teardown
` + "`sprint end`" + ` uses), orphan managed caches, unclaimed clean workspaces, and
agent/weave-issue-N[-reviewed] branches whose run is gone and whose every patch
is upstream. For a queue whose repository is GONE (deleted or renamed) it removes
the whole state root — but only when no wrapper is alive, no lifecycle lock is
held, and no workspace carries uncommitted files or an agent branch that exists
only there. Such a workspace is REFUSED by path: with the repository gone there
is nowhere to preserve the work, so gc keeps it and tells you.`,
		Example: "  bashy weave gc\n  bashy weave gc --apply\n  bashy weave gc --json",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveGC(cmd, apply, &flags)
		},
	}
	cmd.Flags().BoolVar(&apply, "apply", false, "reclaim; the default only reports")
	flags.attach(cmd)
	return cmd
}

func weaveGCQueueDirs(home string) []string {
	var out []string
	for _, root := range append([]string{weaveStateRoot(home)}, weaveLegacyStateRoots(home)...) {
		entries, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			dir := filepath.Join(root, e.Name())
			if _, err := os.Stat(filepath.Join(dir, "queue.json")); err == nil {
				out = append(out, dir)
			}
		}
	}
	sort.Strings(out)
	return out
}

func runWeaveGC(cmd *cobra.Command, apply bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	home, err := os.UserHomeDir()
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave gc", weavecli.ExitGenericFail, err))
	}
	var roots []weaveGCRoot
	for _, dir := range weaveGCQueueDirs(home) {
		roots = append(roots, weaveGCQueue(dir, apply))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave gc", map[string]any{"applied": apply, "roots": roots}))
	}
	out := cmd.OutOrStdout()
	removed, refused := 0, 0
	for _, r := range roots {
		if len(r.Removed) == 0 && len(r.Retained) == 0 && len(r.Refused) == 0 {
			continue
		}
		state := r.Repo
		if r.RepoGone {
			state = "repository gone"
		}
		fmt.Fprintf(out, "%s (%s)\n", r.Queue, state)
		for _, a := range r.Removed {
			verb := "would remove"
			if apply {
				verb = "removed"
			}
			if a.Err != "" {
				fmt.Fprintf(out, "  failed %s %s: %s\n", a.Kind, a.Target, a.Err)
				continue
			}
			removed++
			bytes := a.ExpectedBytes
			if a.Done {
				bytes = a.ActualBytes
			}
			if bytes > 0 {
				fmt.Fprintf(out, "  %s %s %s (%d apparent bytes)\n", verb, a.Kind, a.Target, bytes)
			} else {
				fmt.Fprintf(out, "  %s %s %s\n", verb, a.Kind, a.Target)
			}
		}
		for _, s := range r.Retained {
			fmt.Fprintf(out, "  retained %s\n", s)
		}
		for _, s := range r.Refused {
			refused++
			fmt.Fprintf(out, "  REFUSED %s\n", s)
		}
	}
	switch {
	case removed == 0 && refused == 0:
		fmt.Fprintln(out, "weave gc: nothing to reclaim")
	case !apply:
		fmt.Fprintf(out, "weave gc: %d item(s) reclaimable, %d refused — `bashy weave gc --apply`\n", removed, refused)
	default:
		fmt.Fprintf(out, "weave gc: %d item(s) removed, %d refused\n", removed, refused)
	}
	return nil
}

func weaveGCQueue(dir string, apply bool) weaveGCRoot {
	r := weaveGCRoot{Queue: dir}
	q, err := loadWeaveQueue(dir)
	if err != nil {
		r.Refused = append(r.Refused, "queue unreadable: "+err.Error())
		return r
	}
	repo := filepath.Base(dir)
	if root, ok := weaveRepoRootForQueue(dir); ok {
		r.Repo = root
		weaveGCLiveRepo(&r, dir, root, repo, q, apply)
		return r
	}
	r.RepoGone = true
	weaveGCMissingRepo(&r, dir, q, apply)
	return r
}

// weaveGCLiveRepo: the repository exists, so every rule is the per-run one.
func weaveGCLiveRepo(r *weaveGCRoot, dir, root, repo string, q *weaveQueue, apply bool) {
	live := map[int64]bool{}
	for _, it := range q.Items {
		if it == nil {
			continue
		}
		if !isTerminalState(it.State) {
			live[it.ID] = true
			continue
		}
		if apply {
			r.Removed = append(r.Removed, weavePruneOwnedRun(dir, it.ID, repo)...)
		} else {
			r.Removed = append(r.Removed, weavePlanOwnedRun(dir, repo, it)...)
		}
	}
	// Orphan caches: a run-N cache with no row at all.
	if targets, err := weaveManagedGOCacheSweepTargets(dir, q, false); err == nil {
		for _, t := range targets {
			if !t.Orphan {
				continue
			}
			a := sprintPruneAction{Kind: "cache", Repo: repo, Target: t.Path, ByteKind: "apparent_regular_file_bytes"}
			a.ExpectedBytes, _ = weaveArtifactBytes(t.Path)
			if apply {
				if err := safeRemoveManagedGOCache(dir, t.Path); err != nil {
					a.Err = err.Error()
				} else {
					a.Done, a.ActualBytes, a.BytesComplete = true, a.ExpectedBytes, true
				}
			}
			r.Removed = append(r.Removed, a)
		}
	}
	// Unclaimed workspaces (including stale sibling clones): clean ones go,
	// anything with uncommitted files or a branch that exists only there stays.
	if orphans, err := weaveOrphanWorkspaceTargets(dir, q); err == nil {
		for _, o := range orphans {
			if o.Hold != "" {
				r.Retained = append(r.Retained, fmt.Sprintf("workspace %s: %s", o.Path, strings.TrimSuffix(o.Hold, " (--force to delete anyway)")))
				continue
			}
			a := sprintPruneAction{Kind: "workspace", Repo: repo, Target: o.Path, ByteKind: "apparent_regular_file_bytes"}
			a.ExpectedBytes, _ = weaveArtifactBytes(o.Path)
			if apply {
				if err := safeRemoveWorkspace(dir, o.Path); err != nil {
					a.Err = err.Error()
				} else {
					a.Done, a.ActualBytes, a.BytesComplete = true, a.ExpectedBytes, true
				}
			}
			r.Removed = append(r.Removed, a)
		}
	}
	// Legacy run branches: agent/weave-issue-N[-reviewed] with no live row,
	// retired under the same three proofs as a run's own.
	base := weaveBaseBranch(root)
	out, err := gitOut(root, "for-each-ref", "--format=%(refname:short)", "refs/heads/agent/")
	if err != nil {
		return
	}
	for _, name := range strings.Fields(out) {
		m := weaveRunBranchRE.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		var id int64
		fmt.Sscan(m[1], &id)
		if live[id] {
			continue
		}
		if it := findWeaveItem(q, id); it != nil && isTerminalState(it.State) && m[2] == "" {
			continue // the run's own teardown above handles its canonical name
		}
		a := sprintPruneAction{Kind: "branch", Repo: repo, Target: name, BytesComplete: true}
		if !apply {
			if wt := gitBranchWorktree(root, name); wt != "" {
				r.Retained = append(r.Retained, fmt.Sprintf("branch %s: checked out in worktree %s", name, wt))
				continue
			}
			if !gitBranchIntegrated(root, base, name) {
				r.Retained = append(r.Retained, fmt.Sprintf("branch %s: carries a patch not in %s", name, base))
				continue
			}
			r.Removed = append(r.Removed, a)
			continue
		}
		deleted, err := weaveRetireBranch(root, base, name, "")
		switch {
		case err != nil:
			r.Retained = append(r.Retained, fmt.Sprintf("branch %s: %s", name, err.Error()))
		case deleted:
			a.Done = true
			r.Removed = append(r.Removed, a)
		}
	}
}

// weaveGCMissingRepo: the repository is gone, so the queue is metadata about
// nothing — unless a workspace still holds work that now has nowhere to go.
func weaveGCMissingRepo(r *weaveGCRoot, dir string, q *weaveQueue, apply bool) {
	for _, it := range q.Items {
		if it == nil {
			continue
		}
		if (it.WrapperPid > 0 && pidAlive(it.WrapperPid)) || (it.FinalizerPID > 0 && pidAlive(it.FinalizerPID)) {
			r.Refused = append(r.Refused, fmt.Sprintf("run #%d: wrapper or finalizer still alive", it.ID))
			return
		}
		lock, err := weaveRunLifecycleLock(dir, it.ID)
		if err != nil {
			r.Refused = append(r.Refused, fmt.Sprintf("run #%d: lifecycle lock held", it.ID))
			return
		}
		_ = lock.Release()
	}
	for _, parent := range []string{"workspaces", "sandboxes"} {
		entries, err := os.ReadDir(filepath.Join(dir, parent))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if !e.IsDir() {
				continue
			}
			p := filepath.Join(dir, parent, e.Name())
			if hold := weaveOrphanWorkspaceHold(p); hold != "" {
				r.Refused = append(r.Refused, fmt.Sprintf("workspace %s: %s and the repository is gone — nowhere to preserve it; move the work out by hand, then gc again",
					p, strings.TrimSuffix(hold, " (--force to delete anyway)")))
			}
		}
	}
	if len(r.Refused) > 0 {
		return
	}
	if err := weaveGCContainedRoot(dir); err != nil {
		r.Refused = append(r.Refused, err.Error())
		return
	}
	a := sprintPruneAction{Kind: "state-root", Target: dir, ByteKind: "apparent_regular_file_bytes"}
	a.ExpectedBytes, _ = weaveArtifactBytes(dir)
	if apply {
		if err := os.RemoveAll(dir); err != nil {
			a.Err = err.Error()
		} else {
			a.Done, a.ActualBytes, a.BytesComplete = true, a.ExpectedBytes, true
		}
	}
	r.Removed = append(r.Removed, a)
}

// weaveGCContainedRoot proves dir is a DIRECT child of a weave state root and
// not a symlink, before the one RemoveAll gc performs.
func weaveGCContainedRoot(dir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return err
	}
	if st, err := os.Lstat(abs); err != nil || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("state root is a symlink or unreadable; left alone")
	}
	for _, root := range append([]string{weaveStateRoot(home)}, weaveLegacyStateRoots(home)...) {
		if filepath.Dir(abs) == filepath.Clean(root) {
			return nil
		}
	}
	return errors.New("state root is not a direct child of a weave state root; left alone")
}
