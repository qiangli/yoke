package weave

// A SPRINT MUST BE ABLE TO SAY IT IS CLEAN — AT ANY MOMENT, NOT ONLY AT THE END.
//
// Cleanliness spans three things, and a sprint that is clean in two of them is
// not clean:
//
//	the SPRINT    every linked run is terminal and merged, or explicitly stopped
//	the REPOS     no uncommitted work, nothing unpushed, no orphan branches
//	the HOST      no workspace, worktree, socket or log left behind by dead work
//
// # Why this is not a close-time gate
//
// Closing is the LAST moment this matters and the least useful one. The
// question "is this sprint's state consistent right now" is asked constantly by
// somebody who is not the conductor: another agent about to rebuild a shared
// repo, a reviewer deciding whether a branch is safe to merge, a second sprint
// touching the same checkout. If the only way to ask is to try to end the
// sprint, nobody can ask.
//
// So the check is a standing, non-mutating verb — `bashy sprint prune`, which
// REPORTS by default — that anyone may run at any time, with no sprint id, no
// conductor lease, and no context. `sprint move ... done` gates on it; `sprint
// pause` and `sprint handoff` fold the findings into the CONTINUITY brief,
// because the successor inheriting an unclean tree needs to know that before
// they start, and continuity is exactly the field for facts the next conductor
// must not rediscover.
//
// # Reporting, not repairing
//
// Nothing here deletes or merges. It reports what is unclean and names the verb
// that fixes each class. Reclaiming is `sprint prune`, merging is `weave pull` /
// `weave salvage` — both gated, both separate, both deliberately not reachable
// from a function whose job is to tell the truth about state.

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// sprintHygiene is the reported cleanliness of one sprint.
type sprintHygiene struct {
	Repos    []string `json:"repos,omitempty"`
	Problems []string `json:"problems,omitempty"`
	// Shared names repositories whose dirty state belongs to another active
	// sprint as well. They remain visible in reports, but cannot block this
	// sprint's close because the state is not attributable to it alone.
	Shared []string `json:"shared,omitempty"`
	// Reclaimable names host artifacts that prune could remove right now. It is
	// informational: leftover disk is untidy, not unsafe, and must never make a
	// sprint unclosable on its own.
	Reclaimable []string `json:"reclaimable,omitempty"`
}

// Clean reports whether anything BLOCKS a clean close. Reclaimable disk does
// not: an operator who ends a sprint without pruning has an untidy host, not a
// lost commit, and conflating the two would train people to --force.
func (h sprintHygiene) Clean() bool { return len(h.Problems) == 0 }

// sprintCheckHygiene inspects every repo this sprint touched.
//
// It never mutates. Errors reading a repo become PROBLEMS rather than silent
// passes — a checkout we cannot inspect is not a checkout we may declare clean.
func sprintCheckHygiene(s *weaveStory) sprintHygiene {
	h := sprintHygiene{}
	if s == nil {
		return h
	}
	seen := map[string]bool{}
	shared := sprintSharedHygieneRoots(s, sprintHygieneBoard())
	for _, run := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(run)
		if err != nil {
			h.Problems = append(h.Problems,
				fmt.Sprintf("run %s#%d: queue unreadable (%v) — the checkout may have moved; `bashy sprint unlink %d --repo %s --task %d` if it is gone for good", run.Repo, run.ID, err, s.ID, run.Repo, run.ID))
			continue
		}
		root, ok := weaveRepoRootForQueue(dir)
		if !ok {
			h.Problems = append(h.Problems,
				fmt.Sprintf("run %s#%d: checkout for its queue is gone (%s) — restore it, or `bashy sprint unlink %d --repo %s --task %d`", run.Repo, run.ID, dir, s.ID, run.Repo, run.ID))
			continue
		}
		if seen[root] {
			continue
		}
		seen[root] = true
		h.Repos = append(h.Repos, root)
		if ids := shared[hygieneRootKey(root)]; len(ids) > 0 {
			h.Shared = append(h.Shared, sprintSharedHygieneLabel(run.Repo, root, ids))
			continue
		}
		sprintInspectRepo(root, run.Repo, &h)
	}
	// DECLARED roots only — never the implicit current checkout.
	//
	// sprintStoryRoots() appends the cwd as a zero-configuration source, which
	// is right for FINDING stories and wrong for a gate: it would block a close,
	// or report a sprint unclean, because the operator happened to be standing
	// in an unrelated dirty repo. A sprint is answerable for the repos it
	// tracked (`sprint track`) and the repos its runs are in. Nothing else.
	for _, root := range sprintDeclaredStoryRoots(s) {
		if seen[root] {
			continue
		}
		seen[root] = true
		h.Repos = append(h.Repos, root)
		if ids := shared[hygieneRootKey(root)]; len(ids) > 0 {
			h.Shared = append(h.Shared, sprintSharedHygieneLabel(filepath.Base(root), root, ids))
			continue
		}
		sprintInspectRepo(root, filepath.Base(root), &h)
	}
	sprintInspectRuns(s, &h)
	sort.Strings(h.Problems)
	sort.Strings(h.Reclaimable)
	sort.Strings(h.Repos)
	sort.Strings(h.Shared)
	return h
}

// sprintHygieneBoard returns the board already held by a sprint mutation. A
// standing `sprint prune` call has no mutation callback, so it reads the same
// durable board directly. Failure to read merely removes the sharing exemption;
// hygiene stays fail-closed and reports the dirty repository normally.
func sprintHygieneBoard() []*weaveStory {
	if currentBoard != nil {
		return currentBoard
	}
	dir, err := sprintStoreDir()
	if err != nil {
		return nil
	}
	q, err := readWeaveQueue(dir)
	if err != nil || q == nil {
		return nil
	}
	return q.Stories
}

// sprintSharedHygieneRoots finds exact checkouts attributed to another active
// sprint. Declared story roots cover the repository holding that sprint's
// stories (for example the umbrella); linked weave runs cover subrepositories
// where implementation is active. Merely sharing a basename is insufficient.
func sprintSharedHygieneRoots(s *weaveStory, stories []*weaveStory) map[string][]int64 {
	out := map[string][]int64{}
	for _, other := range stories {
		if other == nil || s == nil || other.ID == s.ID || other.currentBox() == nil {
			continue
		}
		roots := map[string]bool{}
		for _, root := range sprintDeclaredStoryRoots(other) {
			roots[hygieneRootKey(root)] = true
		}
		for _, run := range other.Runs {
			dir, err := weaveQueueDirForSprintRun(run)
			if err != nil {
				continue
			}
			if root, ok := weaveRepoRootForQueue(dir); ok {
				roots[hygieneRootKey(root)] = true
			}
		}
		for root := range roots {
			out[root] = append(out[root], other.ID)
		}
	}
	for root := range out {
		sort.Slice(out[root], func(i, j int) bool { return out[root][i] < out[root][j] })
	}
	return out
}

func hygieneRootKey(root string) string {
	root = filepath.Clean(strings.TrimSpace(root))
	if abs, err := filepath.Abs(root); err == nil {
		root = abs
	}
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return filepath.Clean(root)
}

func sprintSharedHygieneLabel(label, root string, ids []int64) string {
	refs := make([]string, len(ids))
	for i, id := range ids {
		refs[i] = fmt.Sprintf("#%d", id)
	}
	return fmt.Sprintf("%s: shared with active sprint(s) %s in %s — dirty state is attributed, not ignored",
		label, strings.Join(refs, ", "), root)
}

// sprintInspectRepo reports the git-visible state of one checkout.
func sprintInspectRepo(root, label string, h *sprintHygiene) {
	if dirty, files := gitDirtyFiles(root); dirty {
		h.Problems = append(h.Problems,
			fmt.Sprintf("%s: %d uncommitted file(s) in %s — inspect `git -C %s status`, then commit or `git -C %s checkout -- .`", label, files, root, root, root))
	}
	if ahead, ok := gitUnpushedCount(root); ok && ahead > 0 {
		h.Problems = append(h.Problems,
			fmt.Sprintf("%s: %d commit(s) not pushed in %s — a successor cloning from origin would not see them; `git -C %s log @{upstream}..HEAD --oneline` then `git -C %s push`", label, ahead, root, root, root))
	}
	for _, wt := range gitStaleWorktrees(root) {
		h.Reclaimable = append(h.Reclaimable,
			fmt.Sprintf("%s: worktree %s — clean or missing; `git -C %s worktree remove <path>` (or `git -C %s worktree prune`)", label, wt, root, root))
	}
	for _, br := range gitIntegratedBranches(root) {
		h.Reclaimable = append(h.Reclaimable,
			fmt.Sprintf("%s: branch %s in %s — every patch already upstream; verify `git -C %s cherry %s %s`, then `git -C %s branch -d %s`", label, br, root, root, weaveBaseBranch(root), br, root, br))
	}
}

// sprintInspectRuns reports linked runs that are not in a resting state.
func sprintInspectRuns(s *weaveStory, h *sprintHygiene) {
	for _, run := range s.Runs {
		dir, err := weaveQueueDirForSprintRun(run)
		if err != nil {
			continue
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			continue
		}
		for _, it := range q.Items {
			if it.ID != run.ID {
				continue
			}
			switch {
			case it.State == "working":
				h.Problems = append(h.Problems, fmt.Sprintf(
					"run %s#%d %q is still working (branch %s, workspace %s) — watch `bashy weave log %d --follow`, then `bashy weave pause` or let it finish", run.Repo, run.ID, it.Title, it.Branch, it.Workspace, run.ID))
			case it.UnmergedCommits > 0:
				h.Problems = append(h.Problems, fmt.Sprintf(
					"run %s#%d %q has %d unmerged commit(s) on branch %s — review `bashy weave status %d`, then `bashy weave pull` (gated) or `bashy weave salvage %d`",
					run.Repo, run.ID, it.Title, it.UnmergedCommits, it.Branch, run.ID, run.ID))
			case it.NeedsSteward:
				h.Problems = append(h.Problems, fmt.Sprintf(
					"run %s#%d %q needs a decision: %s — see `bashy weave status %d`, then salvage, merge, or `bashy weave abandon %d`", run.Repo, run.ID, it.Title, it.StewardReason, run.ID, run.ID))
			case it.Workspace != "":
				if _, err := os.Stat(it.Workspace); err == nil && weavePrunableForSweep(it.State, false) {
					h.Reclaimable = append(h.Reclaimable, fmt.Sprintf(
						"run %s#%d workspace %s (state %s, merged) — `bashy sprint prune --apply` or `bashy weave prune`", run.Repo, run.ID, it.Workspace, it.State))
				}
			}
		}
	}
}

// --- git probes. Each returns "unknown" rather than guessing. ---

func gitDirtyFiles(root string) (bool, int) {
	out, err := exec.Command("git", "-C", root, "status", "--porcelain").Output()
	if err != nil {
		return false, 0
	}
	n := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if strings.TrimSpace(line) != "" {
			n++
		}
	}
	return n > 0, n
}

// gitUnpushedCount counts commits on HEAD that its upstream does not have.
// No upstream means the question does not apply, not that the answer is zero.
func gitUnpushedCount(root string) (int, bool) {
	out, err := exec.Command("git", "-C", root, "rev-list", "--count", "@{upstream}..HEAD").Output()
	if err != nil {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(strings.TrimSpace(string(out)), "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// gitStaleWorktrees lists registered worktrees whose directory is gone, plus
// those that are clean and therefore safe to remove.
//
// A worktree with uncommitted work is NEVER listed: it holds the only copy of
// something, and offering it for reclamation is how work disappears.
func gitStaleWorktrees(root string) []string {
	out, err := exec.Command("git", "-C", root, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return nil
	}
	var res []string
	for i, block := range strings.Split(string(out), "\n\n") {
		var path string
		for _, line := range strings.Split(block, "\n") {
			if strings.HasPrefix(line, "worktree ") {
				path = strings.TrimSpace(strings.TrimPrefix(line, "worktree "))
			}
		}
		// THE FIRST ENTRY IS THE MAIN WORKING TREE — never a linked one, never
		// removable, and in a SUBMODULE it is reported as the superproject's
		// .git/modules/<name> path rather than the checkout. Comparing against
		// root therefore does not exclude it, and every submodule showed up as
		// a reclaimable worktree pointing at its own gitdir.
		if i == 0 || path == "" || path == root {
			continue
		}
		if _, err := os.Stat(path); err != nil {
			res = append(res, path+" (missing — `git worktree prune`)")
			continue
		}
		if dirty, _ := gitDirtyFiles(path); !dirty {
			res = append(res, path)
		}
	}
	return res
}

// gitIntegratedBranches lists local branches whose every patch already exists
// upstream — the umbrella's mechanical cleanup rule, implemented.
//
// TWO tests, because either alone is wrong. Ancestry catches a plain merge or
// fast-forward. `git cherry` catches the rebase/squash case, where the branch is
// NOT an ancestor yet every patch it carries has an equivalent on the base: it
// prints "- <sha>" for an integrated patch and "+ <sha>" for a unique one, so a
// branch is integrated exactly when no line starts with '+'.
//
// A branch with even one unique patch is never returned. That is the whole
// safety property: cleanup may only ever remove a pointer to commits that
// already live somewhere else.
func gitIntegratedBranches(root string) []string {
	base := weaveBaseBranch(root)
	if base == "" {
		return nil
	}
	out, err := exec.Command("git", "-C", root, "for-each-ref", "--format=%(refname:short)", "refs/heads/").Output()
	if err != nil {
		return nil
	}
	cur, _ := exec.Command("git", "-C", root, "rev-parse", "--abbrev-ref", "HEAD").Output()
	current := strings.TrimSpace(string(cur))
	var res []string
	for _, br := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		br = strings.TrimSpace(br)
		if br == "" || br == base || br == current {
			continue
		}
		if gitBranchIntegrated(root, base, br) {
			res = append(res, br)
		}
	}
	return res
}

func gitBranchIntegrated(root, base, branch string) bool {
	// Ancestor: the branch tip is already reachable from base.
	if err := exec.Command("git", "-C", root, "merge-base", "--is-ancestor", branch, base).Run(); err == nil {
		return true
	}
	// Cherry-equivalent: every patch has an upstream twin.
	out, err := exec.Command("git", "-C", root, "cherry", base, branch).Output()
	if err != nil {
		return false
	}
	body := strings.TrimSpace(string(out))
	if body == "" {
		// No patches at all relative to base — nothing unique to lose.
		return true
	}
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "+") {
			return false
		}
	}
	return true
}

// renderSprintHygiene prints what is unclean, then what could be reclaimed.
func renderSprintHygiene(w io.Writer, h sprintHygiene) {
	for _, p := range h.Problems {
		fmt.Fprintf(w, "  UNCLEAN: %s\n", p)
	}
	for _, r := range h.Reclaimable {
		fmt.Fprintf(w, "  reclaimable: %s\n", r)
	}
	for _, shared := range h.Shared {
		fmt.Fprintf(w, "  SHARED: %s\n", shared)
	}
}

// sprintStateAddendum renders the sprint's current findings as a block to
// append to a continuity brief.
//
// pause and handoff are the two moments a sprint changes hands, and an unclean
// tree is precisely the fact a successor must not have to rediscover: they will
// otherwise start by building a repo somebody left dirty, or by branching from
// a checkout with unpushed commits, and blame their own first command. Folding
// the findings into continuity puts them in the one field the successor is
// guaranteed to read — `sprint resume` prints it.
//
// Empty when everything is clean, so a healthy handoff stays terse.
func sprintStateAddendum(s *weaveStory) string {
	hy := sprintCheckHygiene(s)
	coverage := sprintCoverageProblems(s)
	reach := sprintCheckReachability(s)
	if hy.Clean() && len(coverage) == 0 && len(reach.Problems) == 0 && len(hy.Reclaimable) == 0 && len(hy.Shared) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n\nSTATE AT HANDOVER (bashy sprint prune ")
	b.WriteString(fmt.Sprintf("%d", s.ID))
	b.WriteString("):")
	for _, p := range hy.Problems {
		b.WriteString("\n  UNCLEAN: " + p)
	}
	for _, p := range reach.Problems {
		b.WriteString("\n  UNREACHABLE: " + p)
	}
	for _, c := range coverage {
		b.WriteString("\n  UNCOVERED: " + c)
	}
	for _, shared := range hy.Shared {
		b.WriteString("\n  SHARED: " + shared)
	}
	if n := len(hy.Reclaimable); n > 0 {
		b.WriteString(fmt.Sprintf("\n  reclaimable: %d item(s) — `bashy sprint prune %d --apply`", n, s.ID))
	}
	return b.String()
}

// sprintDeclaredStoryRoots is sprintStoryRoots without the implicit cwd.
func sprintDeclaredStoryRoots(s *weaveStory) []string {
	seen := map[string]bool{}
	var roots []string
	for _, root := range s.StoryRoots {
		if r, err := normalizeStoryRoot(root); err == nil && !seen[r] {
			seen[r] = true
			roots = append(roots, r)
		}
	}
	return roots
}

// sprintUnansweredGate refuses to walk away from a seat with unread requests.
//
// pause, handoff and end are the three ways a conductor stops answering. Each
// of them, with mail sitting unread, converts a question into an indefinite
// wait: the sender has no signal that nobody is coming, and the durable queue
// faithfully preserves a message that will never be looked at.
//
// Reading is enough to clear it. The gate does not demand a reply — an agent
// may legitimately decide a request needs no answer — only that somebody LOOKED,
// which is the thing that was missing.
func sprintUnansweredGate(s *weaveStory, verb string) error {
	if s == nil || strings.TrimSpace(s.Owner) == "" {
		return nil
	}
	n, oldest := sprintOwnerUnanswered(s.Owner)
	if n == 0 {
		return nil
	}
	return fmt.Errorf("sprint #%d cannot %s: %d unanswered message(s) for %q, oldest %s.\n"+
		"  somebody is blocked on an answer that would never come.\n"+
		"  read them first: bashy inbox --as %s\n"+
		"  (reading is enough — the gate asks that somebody looked, not that you replied)",
		s.ID, verb, n, s.Owner, oldest.Round(time.Minute), s.Owner)
}
