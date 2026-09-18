package weave

// CLOSING CONDITIONS — what "wrapped up" means, checked rather than assumed.
//
// A green gate says the code works. It says nothing about whether the work was
// actually PUT somewhere the next sprint will find it. Those are different
// failures and the second is quieter: a tree that builds, passes, and has
// thirty uncommitted files looks perfectly healthy right up until the machine
// reboots or the next sprint's worker checks out a branch over the top of it.
//
// So closing a sprint checks three things per linked repo:
//
//	committed   no uncommitted changes in the working tree
//	pushed      nothing sitting only on this machine
//	pinned      .sibling-pins agrees with each sibling's HEAD, where one exists
//
// The third is here because this tree learned it the hard way: a pin is the
// ONLY sibling source CI sees, and inside an umbrella the siblings are
// submodules, so a stale pin cannot fail a local build. It fails ten minutes
// later, in CI, on code that plainly exists. Leaving a sprint with stale pins
// hands that to whoever comes next with no way to tell it was inherited.
//
// # The concurrency caveat, which is the interesting part
//
// A repo may be linked by MORE THAN ONE running sprint. When it is, dirtiness
// there is not evidence about this sprint — another sprint's worker may have
// put it there, and demanding a clean tree would either block a stop for
// somebody else's work or, worse, invite a conductor to "tidy up" changes that
// were not theirs.
//
// So a shared repo is REPORTED AND NOT REQUIRED CLEAN. The sprint can close
// over it; the report names the sprint it is shared with, so the state is
// attributable rather than mysterious. Only repos this sprint holds alone are
// held to the bar.
//
// This is the same principle as everything else here: never assert something
// the system cannot actually know. "This repo is dirty" is a fact; "this sprint
// left it dirty" is a claim, and with two sprints on one repo it is a claim
// nothing can support.

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"

	coregit "github.com/qiangli/yoke/git"
)

// repoState is one linked repo's closing readiness.
type repoState struct {
	Repo string `json:"repo"`
	Path string `json:"path,omitempty"`
	// Shared names the other RUNNING sprints that also link this repo. When
	// non-empty the repo is exempt from the clean bar — see the file comment.
	Shared []int64 `json:"shared_with,omitempty"`

	Dirty      int      `json:"dirty_files,omitempty"`
	Untracked  int      `json:"untracked_files,omitempty"` // reported, never blocking
	Unpushed   int      `json:"unpushed_commits,omitempty"`
	StalePins  []string `json:"stale_pins,omitempty"`
	Unknown    string   `json:"unknown,omitempty"` // why it could not be checked
	Exempt     bool     `json:"exempt,omitempty"`
	Referenced string   `json:"-"`
}

// OK reports a repo that meets the closing bar, or is exempt from it.
func (r *repoState) OK() bool {
	if r.Exempt {
		return true
	}
	return r.Dirty == 0 && r.Unpushed == 0 && len(r.StalePins) == 0 && r.Unknown == ""
}

// Describe renders one repo's state for a human deciding what to do about it.
func (r *repoState) Describe() string {
	switch {
	case r.Unknown != "":
		return fmt.Sprintf("%s: cannot check (%s)", r.Repo, r.Unknown)
	case r.Exempt:
		ids := make([]string, 0, len(r.Shared))
		for _, id := range r.Shared {
			ids = append(ids, fmt.Sprintf("#%d", id))
		}
		return fmt.Sprintf("%s: shared with %s — not required clean, state is not attributable to this sprint",
			r.Repo, strings.Join(ids, ", "))
	}
	var parts []string
	if r.Dirty > 0 {
		parts = append(parts, fmt.Sprintf("%d uncommitted file(s)", r.Dirty))
	}
	if r.Unpushed > 0 {
		parts = append(parts, fmt.Sprintf("%d unpushed commit(s)", r.Unpushed))
	}
	if len(r.StalePins) > 0 {
		parts = append(parts, fmt.Sprintf("stale pin(s): %s", strings.Join(r.StalePins, ", ")))
	}
	if len(parts) == 0 {
		if r.Untracked > 0 {
			return fmt.Sprintf("%s: clean (%d untracked, not blocking)", r.Repo, r.Untracked)
		}
		return r.Repo + ": clean"
	}
	return r.Repo + ": " + strings.Join(parts, ", ")
}

// checkClosingConditions inspects every repo this sprint links.
//
// repoPath resolves a linked run to a working tree; it is injected so the
// caller owns how a queue becomes a path and this stays testable without a
// filesystem full of repos.
func checkClosingConditions(s *weaveStory, others []*weaveStory, repoPath func(sprintRun) (string, bool)) []repoState {
	shared := sharedRepos(s, others)
	seen := map[string]bool{}
	var out []repoState
	for _, run := range s.Runs {
		key := sprintRunKey(run)
		if run.Repo == "" || seen[key] {
			continue
		}
		seen[key] = true
		st := repoState{Repo: run.Repo, Shared: shared[key]}
		if len(st.Shared) > 0 {
			st.Exempt = true
			out = append(out, st)
			continue
		}
		path, ok := repoPath(run)
		if !ok {
			st.Unknown = "no unique checkout on this host"
			out = append(out, st)
			continue
		}
		st.Path = path
		inspectRepo(&st)
		out = append(out, st)
	}
	return out
}

// sharedRepos maps a repo name to the other RUNNING sprints that also link it.
//
// Only running sprints count. A finished sprint that once touched the repo has
// no worker that could be dirtying it now, so treating it as a sharer would
// exempt repos forever on the strength of history.
func sharedRepos(s *weaveStory, others []*weaveStory) map[string][]int64 {
	mine := map[string]bool{}
	for _, r := range s.Runs {
		mine[sprintRunKey(r)] = true
	}
	out := map[string][]int64{}
	for _, o := range others {
		if o == nil || o.ID == s.ID || o.currentBox() == nil {
			continue
		}
		for _, r := range o.Runs {
			key := sprintRunKey(r)
			if mine[key] {
				out[key] = append(out[key], o.ID)
				continue
			}
			// A legacy link has no checkout identity. Treat it as sharing any
			// stable link with the same basename rather than wrongly certifying
			// that either sprint has exclusive ownership.
			for _, own := range s.Runs {
				if own.Repo == r.Repo && (own.Queue == "" || r.Queue == "") {
					out[sprintRunKey(own)] = append(out[sprintRunKey(own)], o.ID)
				}
			}
		}
	}
	return out
}

// inspectRepo fills in the three checks. Every failure to check is recorded as
// Unknown rather than passing: a check that could not run is not a check that
// succeeded.
func inspectRepo(st *repoState) {
	_, entries, err := coregit.Status(st.Path)
	if err != nil {
		st.Unknown = err.Error()
		return
	}
	// UNTRACKED IS NOT DIRTY, for two reasons that point the same way.
	//
	// Substantively, an untracked scratch file is not unfinished work — it is
	// litter, and blocking a sprint close on it would train conductors to
	// `--force` habitually, which costs the check its meaning.
	//
	// Mechanically, go-git does not consult every ignore source real git does
	// (a global excludesfile, .git/info/exclude), so it reports files git
	// itself calls clean. Counting those would block closes on state the
	// conductor cannot see with `git status` — a check that fails for reasons
	// its user cannot reproduce is worse than no check.
	//
	// Modifications to TRACKED files are the real signal and are counted.
	for _, e := range entries {
		if e.Status == "??" {
			st.Untracked++
			continue
		}
		st.Dirty++
	}

	// Unpushed: commits on HEAD that the upstream does not have. A repo with no
	// upstream is not "unpushed", it is unconfigured, and saying otherwise would
	// block a stop over a state the conductor cannot fix from here.
	if n, err := coregit.RevListCount(st.Path, "@{u}..HEAD"); err == nil {
		st.Unpushed = n
	}

	st.StalePins = stalePins(st.Path)
}

// stalePins reports sibling pins that disagree with the sibling's HEAD.
//
// Absent .sibling-pins means nothing to check — most repos have none, and their
// absence is not a finding.
func stalePins(repoPath string) []string {
	data, err := os.ReadFile(filepath.Join(repoPath, ".sibling-pins"))
	if err != nil {
		return nil
	}
	var stale []string
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name, pinned, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		name, pinned = strings.TrimSpace(name), strings.TrimSpace(pinned)
		if name == "" || pinned == "" {
			continue
		}
		sib := filepath.Join(filepath.Dir(repoPath), name)
		if _, err := os.Stat(filepath.Join(sib, ".git")); err != nil {
			continue // sibling not checked out here — nothing to compare against
		}
		head, err := coregit.RevParse(coregit.RevParseOptions{RepoPath: sib})
		if err != nil || head == nil || head.Hash == "" {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(head.Hash), pinned) {
			stale = append(stale, name)
		}
	}
	return stale
}

// weaveRepoRootForQueue reads the repo a queue serves.
//
// The queue DIRECTORY name deliberately cannot be reversed into a path — the
// tag is hashed so a subagent handed the workspace never learns where the
// origin lives. The queue's own Root field is the sanctioned way back, stamped
// on write for exactly this kind of conductor-side lookup.
func weaveRepoRootForQueue(dir string) (string, bool) {
	q, err := loadWeaveQueue(dir)
	if err != nil || q == nil || strings.TrimSpace(q.Root) == "" {
		return "", false
	}
	if _, err := os.Stat(filepath.Join(q.Root, ".git")); err != nil {
		return "", false
	}
	return q.Root, true
}

// repoAssignment is WHO IS WORKING ON WHAT in one repo — reported, so an agent
// never has to infer it.
//
// Guessing is the failure this replaces. Without it, an agent picking up work
// reads a dirty tree or a branch and has to reason about whether somebody else
// owns it; two agents reading the same ambiguity reach the same wrong answer
// and collide. The queue already knows — it records the run, its state, and
// which tool is driving it — so the answer is a lookup, not an inference.
type repoAssignment struct {
	Repo   string `json:"repo"`
	Run    int64  `json:"run"`
	Title  string `json:"title,omitempty"`
	State  string `json:"state,omitempty"`
	Tool   string `json:"tool,omitempty"`
	Sprint int64  `json:"sprint"`
	Branch string `json:"branch,omitempty"`
}

// repoAssignments reports every linked run across the given sprints, so one
// call answers "who is in this repo right now" across ALL of them — which is
// the only useful scope. A per-sprint answer still leaves an agent guessing
// about the other sprints.
func repoAssignments(stories []*weaveStory) []repoAssignment {
	var out []repoAssignment
	byQueue := map[string]*weaveQueue{}
	for _, s := range stories {
		if s == nil {
			continue
		}
		for _, run := range s.Runs {
			a := repoAssignment{Repo: run.Repo, Run: run.ID, Sprint: s.ID}
			if dir, err := weaveQueueDirForSprintRun(run); err == nil {
				q, cached := byQueue[dir]
				if !cached {
					if loaded, err := loadWeaveQueue(dir); err == nil {
						q = loaded
						byQueue[dir] = loaded
					}
				}
				if q != nil {
					for _, it := range q.Items {
						if it.ID == run.ID {
							a.Title, a.State, a.Tool, a.Branch = it.Title, it.State, it.Tool, it.Branch
							break
						}
					}
				}
			}
			out = append(out, a)
		}
	}
	return out
}

// newSprintWhoCmd answers "who is working on what, in which repo" — reported,
// so an agent never infers it.
//
// The inference it replaces is the dangerous kind. An agent that finds a dirty
// tree or an unfamiliar branch has to reason about whether somebody else owns
// it, and two agents reading the same ambiguity reach the same wrong answer and
// collide. The queue already records the run, its state and the tool driving
// it; this is a lookup that was being left to guesswork.
//
// It spans EVERY sprint, not just one. A per-sprint answer still leaves an
// agent guessing about the others, which is the same failure one level up.
func newSprintWhoCmd() *cobra.Command {
	var flags weaveOutputFlags
	var repoFilter string
	cmd := &cobra.Command{
		Use:   "who",
		Short: "Who is working on what, per repo, across every sprint",
		Long: "who reports the current assignment of work to repos: which sprint, which\n" +
			"run, which agent, and what state it is in.\n\n" +
			"It exists so no agent has to GUESS. A dirty tree or an unfamiliar branch is\n" +
			"ambiguous on its own, and two agents resolving that ambiguity the same wrong\n" +
			"way is how they collide. The queue already knows; this reads it.",
		Example: "  bashy sprint who\n  bashy sprint who --repo coreutils",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			dir, err := weaveStoryDir(cmd, mode, "sprint who")
			if err != nil {
				return err
			}
			q, lerr := loadWeaveQueue(dir)
			if lerr != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "sprint who", weavecli.ExitGenericFail, lerr))
			}
			all := repoAssignments(q.Stories)
			if repoFilter != "" {
				var f []repoAssignment
				for _, a := range all {
					if a.Repo == repoFilter {
						f = append(f, a)
					}
				}
				all = f
			}
			sort.Slice(all, func(i, j int) bool {
				if all[i].Repo != all[j].Repo {
					return all[i].Repo < all[j].Repo
				}
				return all[i].Run < all[j].Run
			})
			if mode == weavecli.OutputJSON {
				return ec(emitOK(cmd.OutOrStdout(), mode, "sprint who", map[string]any{"assignments": all}))
			}
			out := cmd.OutOrStdout()
			if len(all) == 0 {
				fmt.Fprintln(out, "no runs are linked to any sprint — `sprint link <id> --repo <name> --task <issue>`")
				return nil
			}
			cur := ""
			for _, a := range all {
				if a.Repo != cur {
					cur = a.Repo
					fmt.Fprintf(out, "%s\n", cur)
				}
				state := a.State
				if state == "" {
					state = "unknown to this host"
				}
				tool := a.Tool
				if tool == "" {
					tool = "—"
				}
				fmt.Fprintf(out, "  run #%-4d sprint #%-3d %-9s %-10s %s\n",
					a.Run, a.Sprint, state, tool, weaveTruncate(a.Title, 40))
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&repoFilter, "repo", "", "only this repo")
	flags.attach(cmd)
	return cmd
}
