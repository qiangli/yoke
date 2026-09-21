package weave

// A sprint goal is the durable contract. Story status and priority remain in
// the repo todo stores; this file only keeps stable references and derives the
// checklist/index every time it is read. That prevents a second, stale copy of
// either progress or ordering from forming on the sprint card.

import (
	"fmt"
	"github.com/qiangli/yoke/pkg/fleet"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/room"
	todopkg "github.com/qiangli/yoke/pkg/todo"
	"github.com/spf13/cobra"
)

type sprintStoryRef struct {
	Repo string `json:"repo"`
	ID   string `json:"id"`
}

type sprintGoalItem struct {
	ID           string           `json:"id"`
	Text         string           `json:"text"`
	Stories      []sprintStoryRef `json:"stories,omitempty"`
	GateRequired bool             `json:"gate_required,omitempty"`
	Evidence     string           `json:"evidence,omitempty"`
}

type sprintExecution struct {
	PriorityFirst bool            `json:"priority_first"`
	CurrentFocus  *sprintStoryRef `json:"current_focus,omitempty"`
	Override      string          `json:"override_reason,omitempty"`
}

type sprintStoryState struct {
	Ref      sprintStoryRef `json:"ref"`
	Title    string         `json:"title"`
	Status   string         `json:"status"`
	Priority string         `json:"priority,omitempty"`
	Seq      int            `json:"seq,omitempty"`
	Missing  bool           `json:"missing,omitempty"`
	// SprintID is the uuid the story's frontmatter names, when it does; the
	// no-card rung of the commit guard checks an optional Sprint-ID against it.
	SprintID string `json:"sprint_id,omitempty"`
}

// sprintInboxDeliveryLive reports whether mail addressed to this owner can
// actually arrive. It is a THIN PROJECTION of room.OwnerTransportFor and holds
// no logic of its own, deliberately.
//
// It used to carry a private copy of that check, and two predicates answering
// one question about one seat is the defect sprint 105 was opened to fix: the
// board and `bashy agent` disagreed about the same conductor at the same
// instant. A second copy here would have rebuilt exactly that.
//
// ONE BEHAVIOUR CHANGED IN THE MERGE, on purpose rather than by inheritance.
// The old copy accepted CapInboxStream ONLY when the card's Mode was
// "sprint-inbox", so a live `bashy inbox --watch --as X` (Mode "inbox") did not
// count as reachable. It should: an agent holding its own inbox watch open has
// undertaken to read it, which is the entire content of the attached rung. The
// shared predicate counts it, so this now reports reachable in a case that
// previously read as unreachable.
//
// The old copy also required card.Nick == owner. The shared predicate resolves
// through AgentClaimID with the legacy bare-name fallback, which is the same
// identity check every other caller uses.
func sprintInboxDeliveryLive(owner string) bool {
	transport, _ := room.OwnerTransportFor(owner)
	return transport.Deliverable()
}

// sprintReadyLine tells the new conductor the ONE thing it now has to do.
//
// It used to report NOT READY unless a stream was attached, which named a
// condition rather than an action and sent an agent off to arrange machinery.
// Reading your inbox is the whole job: it is how mail arrives and it is what
// keeps the seat live (RefreshSprintOwnerActivity).
// sprintSeatToolMismatch reports when the caller's harness is not the tool the
// seat's name is bound to — i.e. somebody is about to work under a name that
// belongs to a different agent.
//
// It WARNS rather than refuses, and the boundary is deliberate. The host can
// see the mismatch: fleet.DetectTool names the harness actually running, and the
// agent record names the tool the seat is bound to. What the host CANNOT see is
// whether the name is legitimately the caller's own — an agent may hold a name
// across sessions and know it from its own memory. So the host reports the fact
// it can prove and leaves the judgement to whoever can make it.
//
// Observed 2026-09-05: a codex CLI picked up a sprint under a name bound to
// claude:opus5, because `sprint show` named the previous holder next to a resume
// hint. The fleet then reports the wrong tool for the seat, band and routing read
// that binding, and the work is attributed to an agent that did none of it.
func sprintSeatToolMismatch(owner string) string {
	tool, detected := fleet.DetectTool()
	if !detected || strings.TrimSpace(tool) == "" {
		return ""
	}
	a, ok := fleetCatalog().Agent(strings.TrimSpace(owner))
	if !ok || strings.TrimSpace(a.Tool) == "" {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(a.Tool), strings.TrimSpace(tool)) {
		return ""
	}
	return fmt.Sprintf("\n  NOTE: this seat's name is bound to %s:%s, and you are running under %s.\n"+
		"  If %q is genuinely your own name, carry on. If you adopted it from the sprint\n"+
		"  record, take the seat under YOUR name instead — `bashy agent add <name> --tool %s\n"+
		"  --model <model>` — or the fleet reports the wrong tool for this seat and the work\n"+
		"  is attributed to an agent that did none of it.",
		a.Tool, a.Model, tool, owner, tool)
}

// sprintSeatDeliveryAdvisory reports that mail addressed to this seat cannot
// currently WAKE it — and REPORTS rather than refuses, for the same reason
// sprintSeatToolMismatch above does.
//
// THE DECISION, pinned by TestSprintDeliveryGateIsConsistent (todo 27ae4f3792e2).
// This check used to REFUSE exactly one verb: `sprint focus`, which sets an
// advisory pointer at the story the manager intends next. The same owner could
// start, take, checkpoint and END the sprint — `end` is irreversible and closes
// the card — and was blocked only from the cheapest, most reversible, purely
// bookkeeping operation. That asymmetry cost sprint #135 its focus pointer: the
// refusal reads as "this seat is not properly established", so the conductor
// went looking for a problem that did not exist and drove the sprint without it.
//
// Three placements were possible (the story enumerates them). The file already
// answers the question. `validateSprintOwner` REFUSES an unregistered owner at
// every point an owner is written, because that is a fact the host can prove and
// which is always wrong. `sprintSeatToolMismatch` WARNS, because the host can
// prove the mismatch but CANNOT judge whether the name is legitimately the
// caller's own. Live delivery is the second kind: a human operator driving a
// sprint from a terminal is a legitimate mode that no managed session backs, and
// `sprint reach` already reports it exactly this way. So it warns, everywhere —
// seating, focusing and ending alike — and refuses nowhere.
//
// This does NOT leave the underlying question unanswered, which is what the
// story forbids: the advisory now appears on the verbs where an unwakeable owner
// actually matters (seating one, and ending a sprint under one), where before it
// appeared on none of them.
func sprintSeatDeliveryAdvisory(owner string) string {
	if strings.TrimSpace(owner) == "" || sprintInboxDeliveryLive(owner) {
		return ""
	}
	return fmt.Sprintf("\nnote: %s has no verified managed inbox delivery — mail addressed to this seat "+
		"cannot wake it. Launch it through Bashy if it is meant to be driven by mail; a terminal "+
		"`bashy inbox --watch --as %s` keeps the seat live but cannot be woken. Harmless if you are "+
		"steering this sprint yourself.", owner, owner)
}

func sprintReadyLine(id int64, owner string) string {
	// Name the MANAGER'S job first. This line used to offer only "read your
	// mail", which reads as an individual-contributor next step and is how a
	// conductor ends up working a whole sprint alone beside an idle fleet.
	return fmt.Sprintf("you are the MANAGER of this sprint — its conductor, which is what `conductor:%d` addresses: "+
		"prioritize its stories, then delegate them "+
		"to agents from `bashy agent list` (run independent stories in parallel; work one yourself only "+
		"if it finishes immediately; the roster is yours to extend — `bashy agent add`/`clone`; widen to the "+
		"number of READY INDEPENDENT stories, not the size of the roster — agents cost tokens and contend for "+
		"rate limits, see `bashy weave fleet`)\n"+
		"next: `bashy skill show conductor` — the PROCEDURE for this seat, written for an agent: "+
		"decompose, file stories, launch and monitor the fleet, gate every merge. Then "+
		"`bashy sprint show %d` for the backlog · `bashy inbox --as %s` (reads your mail and keeps "+
		"the seat live; `--watch` to stay attached; `bashy skill show inbox` for how mail works)"+
		sprintSeatToolMismatch(owner)+sprintSeatDeliveryAdvisory(owner), id, id, owner)
}

func normalizeStoryRoot(root string) (string, error) {
	if strings.TrimSpace(root) == "" {
		if found, ok := todopkg.FindGitRoot(); ok {
			root = found
		} else {
			return "", fmt.Errorf("no git repo here; pass --repo <root>")
		}
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	abs = filepath.Clean(abs)
	// A root that is not a directory is a mistake, not a store: `--repo --plain`
	// once recorded "<cwd>/--plain" as a tracked repo.
	if st, err := os.Stat(abs); err != nil || !st.IsDir() {
		return "", fmt.Errorf("repo root %q is not an existing directory", abs)
	}
	return abs, nil
}

func sprintStoryRoots(s *weaveStory) []string {
	seen := map[string]bool{}
	var roots []string
	for _, root := range s.StoryRoots {
		if r, err := normalizeStoryRoot(root); err == nil && !seen[r] {
			seen[r] = true
			roots = append(roots, r)
		}
	}
	// The current checkout is a useful zero-configuration source, but is never
	// persisted by a read. `sprint track` makes it durable for later handoffs.
	if r, err := normalizeStoryRoot(""); err == nil && !seen[r] {
		roots = append(roots, r)
	}
	return roots
}

func loadSprintStories(s *weaveStory) ([]sprintStoryState, error) {
	seen := map[string]bool{}
	var out []sprintStoryState
	for _, root := range sprintStoryRoots(s) {
		items, err := todopkg.List(todopkg.RepoStore(root), "")
		if err != nil {
			return nil, fmt.Errorf("stories in %s: %w", root, err)
		}
		for _, it := range items {
			if !storyBelongsToSprint(it, s) {
				continue
			}
			key := root + "\x00" + it.ID
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, sprintStoryState{Ref: sprintStoryRef{Repo: root, ID: it.ID}, Title: it.Title, Status: it.Status, Priority: it.Priority, Seq: it.Seq, SprintID: it.SprintID})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if a, b := todopkg.PriorityRank(out[i].Priority), todopkg.PriorityRank(out[j].Priority); a != b {
			return a < b
		}
		if out[i].Seq != out[j].Seq {
			return out[i].Seq < out[j].Seq
		}
		return out[i].Ref.ID < out[j].Ref.ID
	})
	return out, nil
}

func resolveSprintStory(ref sprintStoryRef) sprintStoryState {
	it, err := todopkg.ResolveRef(todopkg.RepoStore(ref.Repo), ref.ID)
	if err != nil {
		return sprintStoryState{Ref: ref, Status: "missing", Missing: true}
	}
	return sprintStoryState{Ref: sprintStoryRef{Repo: ref.Repo, ID: it.ID}, Title: it.Title, Status: it.Status, Priority: it.Priority, Seq: it.Seq}
}

func sprintGoalDone(g sprintGoalItem) bool {
	if len(g.Stories) == 0 {
		return strings.TrimSpace(g.Evidence) != ""
	}
	for _, ref := range g.Stories {
		story := resolveSprintStory(ref)
		if story.Missing || (story.Status != todopkg.StatusDone && story.Status != issue.StatusClosed) {
			return false
		}
	}
	return !g.GateRequired || strings.TrimSpace(g.Evidence) != ""
}

func sprintGoalDangling(g sprintGoalItem) []string {
	var out []string
	for _, ref := range g.Stories {
		if resolveSprintStory(ref).Missing {
			out = append(out, ref.Repo+"#"+ref.ID)
		}
	}
	return out
}

func sprintUncheckedGoals(s *weaveStory) []string {
	var out []string
	for _, g := range s.Goal {
		if !sprintGoalDone(g) {
			out = append(out, g.ID)
		}
	}
	return out
}

func nextSprintStory(s *weaveStory) (*sprintStoryState, error) {
	stories, err := loadSprintStories(s)
	if err != nil {
		return nil, err
	}
	for i := range stories {
		if stories[i].Status != todopkg.StatusDone && stories[i].Status != issue.StatusClosed && stories[i].Status != todopkg.StatusBlocked {
			return &stories[i], nil
		}
	}
	return nil, nil
}

func renderSprintExecution(w io.Writer, s *weaveStory) {
	fmt.Fprintln(w, "  execution:  PRIORITY-FIRST (P0 → P1 → P2 → P3); lower-priority focus requires a recorded override")
	if s.Execution.CurrentFocus != nil {
		cur := resolveSprintStory(*s.Execution.CurrentFocus)
		fmt.Fprintf(w, "  focus:      [%s/%s] %s (%s)\n", cur.Priority, cur.Status, cur.Title, cur.Ref.ID)
	}
	if next, err := nextSprintStory(s); err == nil && next != nil {
		fmt.Fprintf(w, "  next:       [%s/%s] %s (%s)\n", next.Priority, next.Status, next.Title, next.Ref.ID)
	}
	if len(s.Goal) > 0 {
		fmt.Fprintln(w, "  ── goal checklist (derived from story closure + evidence) ──")
		for _, g := range s.Goal {
			mark := " "
			if sprintGoalDone(g) {
				mark = "x"
			}
			warning := ""
			if dangling := sprintGoalDangling(g); len(dangling) > 0 {
				warning = "  WARNING dangling: " + strings.Join(dangling, ", ")
			}
			fmt.Fprintf(w, "  [%s] %s — %s%s\n", mark, g.ID, g.Text, warning)
		}
	}
}

func newSprintTrackCmd() *cobra.Command {
	var flags weaveOutputFlags
	var repo string
	cmd := &cobra.Command{Use: "track <sprint>", Short: "Add a repo todo store to the sprint's derived story index", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := sprintArg(cmd, flags.mode(), "sprint track", args[0])
		if err != nil {
			return err
		}
		root, err := normalizeStoryRoot(repo)
		if err != nil {
			return err
		}
		return runWeaveStoryMutate(cmd, id, "sprint track", &flags, func(s *weaveStory) (string, error) {
			for _, old := range s.StoryRoots {
				if old == root {
					return fmt.Sprintf("sprint #%d already tracks %s", id, root), nil
				}
			}
			s.StoryRoots = append(s.StoryRoots, root)
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "system", "tracked story repo "+root)
			return fmt.Sprintf("sprint #%d tracks %s", id, root), nil
		})
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repo root (default current git repo)")
	flags.attach(cmd)
	return cmd
}

// newSprintUntrackCmd is the exact inverse of track: drop one root from the
// sprint's derived story index. The root is matched as recorded, so a root
// that no longer exists on disk can still be removed.
func newSprintUntrackCmd() *cobra.Command {
	var flags weaveOutputFlags
	var repo string
	cmd := &cobra.Command{Use: "untrack <sprint>", Short: "Remove a repo todo store from the sprint's derived story index", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("sprint must be an integer: %q", args[0])
		}
		root := strings.TrimSpace(repo)
		if root == "" {
			if root, err = normalizeStoryRoot(""); err != nil {
				return err
			}
		} else if abs, err := filepath.Abs(root); err == nil {
			root = filepath.Clean(abs)
		}
		return runWeaveStoryMutate(cmd, id, "sprint untrack", &flags, func(s *weaveStory) (string, error) {
			kept := s.StoryRoots[:0]
			removed := false
			for _, old := range s.StoryRoots {
				if old == root {
					removed = true
					continue
				}
				kept = append(kept, old)
			}
			if !removed {
				return "", fmt.Errorf("sprint #%d does not track %s", id, root)
			}
			s.StoryRoots = kept
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "system", "untracked story repo "+root)
			return fmt.Sprintf("sprint #%d no longer tracks %s", id, root), nil
		})
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repo root (default current git repo)")
	flags.attach(cmd)
	return cmd
}

func newSprintGoalCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "goal", Short: "Manage the durable, derived sprint goal checklist"}
	cmd.AddCommand(newSprintGoalAddCmd(), newSprintGoalLinkCmd(), newSprintGoalEvidenceCmd(), newSprintGoalRmCmd())
	return cmd
}

// newSprintGoalRmCmd removes a required outcome from the checklist.
//
// WHY THIS EXISTS. A goal item checks only when every story linked to it is
// closed (sprintGoalDone), and `sprint move <id> done` refuses over any
// unchecked item — a refusal --force deliberately does NOT cover, because a
// plan you did not finish is not a plan you may declare finished.
//
// That left one state with no exit. Move a sprint's remaining open stories to
// a successor sprint — the ordinary way to close a sprint that ran out of time
// — and its goal items keep pointing at stories that now belong to the other
// card. They can never close HERE, `goal link` refuses to re-point them
// (it requires it.Sprint == id), and there was no way to retire the item. The
// sprint was then permanently unclosable: observed on #123 and #126, and
// recorded in #126's own continuity as "the CLI has no unlink operation".
//
// WHAT IT IS NOT. This is not a way to make a red sprint look green. Removing
// an outcome says it is no longer required OF THIS SPRINT — because it moved
// to a successor, or because the operator dropped it from scope. It never says
// the outcome was achieved. So --reason is mandatory and the removed item is
// written to the thread VERBATIM (text, gate flag, story refs, evidence), which
// is the durable record; the checklist is only its index. A reader of the
// closed card can still see every outcome that was ever required of it, who
// retired it, and why.
func newSprintGoalRmCmd() *cobra.Command {
	var flags weaveOutputFlags
	var reason string
	cmd := &cobra.Command{
		Use:   "rm <sprint> <goal>",
		Short: "Retire a required outcome from the checklist — it moved elsewhere or left scope, never because it was met",
		Args:  cobra.ExactArgs(2),
	}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return fmt.Errorf("sprint must be an integer: %q", args[0])
		}
		reason = strings.TrimSpace(reason)
		if reason == "" {
			return fmt.Errorf("--reason is required: say where this outcome went (a successor sprint) or who took it out of scope")
		}
		return runWeaveStoryMutate(cmd, id, "sprint goal rm", &flags, func(s *weaveStory) (string, error) {
			idx := -1
			for i := range s.Goal {
				if s.Goal[i].ID == args[1] {
					idx = i
					break
				}
			}
			if idx < 0 {
				return "", fmt.Errorf("goal item %q not found", args[1])
			}
			g := s.Goal[idx]
			s.Goal = append(s.Goal[:idx], s.Goal[idx+1:]...)
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "decision", sprintGoalEpitaph(g, reason))
			return fmt.Sprintf("sprint #%d retired goal %s (%d remaining); recorded on the thread", id, g.ID, len(s.Goal)), nil
		})
	}
	cmd.Flags().StringVar(&reason, "reason", "", "where the outcome went, or who took it out of scope — recorded on the card")
	flags.attach(cmd)
	return cmd
}

// sprintGoalEpitaph renders everything the checklist knew about a retired goal,
// so removing the index entry loses nothing. The removal is only as honest as
// this line is complete.
func sprintGoalEpitaph(g sprintGoalItem, reason string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "retired goal item %s — %s", g.ID, reason)
	fmt.Fprintf(&b, "\n  outcome was: %s", g.Text)
	if g.GateRequired {
		b.WriteString("\n  gate-required: yes")
	}
	for _, ref := range g.Stories {
		story := resolveSprintStory(ref)
		status := story.Status
		if story.Missing {
			status = "missing"
		}
		fmt.Fprintf(&b, "\n  story: %s (%s) [%s]", ref.ID, filepath.Base(ref.Repo), status)
	}
	if e := strings.TrimSpace(g.Evidence); e != "" {
		fmt.Fprintf(&b, "\n  evidence on record: %s", e)
	}
	return b.String()
}

func findSprintGoal(s *weaveStory, id string) *sprintGoalItem {
	for i := range s.Goal {
		if s.Goal[i].ID == id {
			return &s.Goal[i]
		}
	}
	return nil
}

func newSprintGoalAddCmd() *cobra.Command {
	var flags weaveOutputFlags
	var goalID, text, story, repo string
	var gate bool
	cmd := &cobra.Command{Use: "add <sprint>", Short: "Add a required outcome to the sprint checklist (--story covers one in the same step)", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		goalID, text = strings.TrimSpace(goalID), strings.TrimSpace(text)
		if goalID == "" || text == "" {
			return fmt.Errorf("--id and --text are required")
		}
		// CREATE AND LINK IN ONE STEP. Covering a newly reported bug was two
		// commands (add, then link), and the second is the one that actually
		// closes the coverage gap — so the common case of "a p0 just arrived,
		// put it in the plan" was exactly the case most likely to be left half
		// done, leaving the plan silently not describing the sprint again.
		var linkRoot string
		var linkItem *issue.Issue
		if strings.TrimSpace(story) != "" {
			root, err := normalizeStoryRoot(repo)
			if err != nil {
				return err
			}
			it, err := todopkg.ResolveRef(todopkg.RepoStore(root), story)
			if err != nil {
				return err
			}
			if it.Sprint != id {
				return fmt.Errorf("story %s belongs to sprint #%d, not #%d", it.ID, it.Sprint, id)
			}
			linkRoot, linkItem = root, it
		}
		return runWeaveStoryMutate(cmd, id, "sprint goal add", &flags, func(s *weaveStory) (string, error) {
			if findSprintGoal(s, goalID) != nil {
				return "", fmt.Errorf("goal item %q already exists", goalID)
			}
			item := sprintGoalItem{ID: goalID, Text: text, GateRequired: gate}
			msg := fmt.Sprintf("sprint #%d added goal %s", id, goalID)
			if linkRoot != "" {
				item.Stories = append(item.Stories, sprintStoryRef{Repo: linkRoot, ID: linkItem.ID})
				found := false
				for _, old := range s.StoryRoots {
					found = found || old == linkRoot
				}
				if !found {
					s.StoryRoots = append(s.StoryRoots, linkRoot)
				}
				msg += " linked to story " + linkItem.ID
			}
			s.Goal = append(s.Goal, item)
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "system", "added goal item "+goalID)
			if linkRoot != "" {
				weaveStoryAppend(s, weaveStoryConductorName(s, ""), "system",
					"linked story "+linkItem.ID+" to goal "+goalID)
			}
			return msg, nil
		})
	}
	cmd.Flags().StringVar(&goalID, "id", "", "stable checklist id")
	cmd.Flags().StringVar(&text, "text", "", "required outcome")
	cmd.Flags().StringVar(&story, "story", "", "todo id or unique prefix to cover with this goal, in one step")
	cmd.Flags().StringVar(&repo, "repo", "", "repo root holding the story (default: this checkout)")
	cmd.Flags().BoolVar(&gate, "gate-required", false, "require recorded evidence after stories close")
	flags.attach(cmd)
	return cmd
}

func newSprintGoalLinkCmd() *cobra.Command {
	var flags weaveOutputFlags
	var repo, story string
	cmd := &cobra.Command{Use: "link <sprint> <goal>", Short: "Link a repo story to a goal item", Args: cobra.ExactArgs(2)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		root, err := normalizeStoryRoot(repo)
		if err != nil {
			return err
		}
		it, err := todopkg.ResolveRef(todopkg.RepoStore(root), story)
		if err != nil {
			return err
		}
		if it.Sprint != id {
			return fmt.Errorf("story %s belongs to sprint #%d, not #%d", it.ID, it.Sprint, id)
		}
		return runWeaveStoryMutate(cmd, id, "sprint goal link", &flags, func(s *weaveStory) (string, error) {
			g := findSprintGoal(s, args[1])
			if g == nil {
				return "", fmt.Errorf("goal item %q not found", args[1])
			}
			ref := sprintStoryRef{Repo: root, ID: it.ID}
			for _, old := range g.Stories {
				if old == ref {
					return fmt.Sprintf("goal %s already links %s", g.ID, it.ID), nil
				}
			}
			g.Stories = append(g.Stories, ref)
			found := false
			for _, old := range s.StoryRoots {
				found = found || old == root
			}
			if !found {
				s.StoryRoots = append(s.StoryRoots, root)
			}
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "system", fmt.Sprintf("linked story %s to goal %s", it.ID, g.ID))
			return fmt.Sprintf("sprint #%d goal %s linked %s", id, g.ID, it.ID), nil
		})
	}
	cmd.Flags().StringVar(&repo, "repo", "", "story repo root (default current)")
	cmd.Flags().StringVar(&story, "story", "", "todo id or unique prefix")
	_ = cmd.MarkFlagRequired("story")
	flags.attach(cmd)
	return cmd
}

func newSprintGoalEvidenceCmd() *cobra.Command {
	var flags weaveOutputFlags
	var message string
	cmd := &cobra.Command{Use: "evidence <sprint> <goal>", Short: "Record gate evidence or human approval for a goal item", Args: cobra.ExactArgs(2)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		if strings.TrimSpace(message) == "" {
			return fmt.Errorf("-m <evidence> required")
		}
		return runWeaveStoryMutate(cmd, id, "sprint goal evidence", &flags, func(s *weaveStory) (string, error) {
			g := findSprintGoal(s, args[1])
			if g == nil {
				return "", fmt.Errorf("goal item %q not found", args[1])
			}
			g.Evidence = strings.TrimSpace(message)
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "review", "goal "+g.ID+" evidence: "+g.Evidence)
			return fmt.Sprintf("sprint #%d goal %s evidence recorded", id, g.ID), nil
		})
	}
	cmd.Flags().StringVarP(&message, "message", "m", "", "gate result or human approval")
	flags.attach(cmd)
	return cmd
}

func newSprintNextCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{Use: "next <sprint>", Short: "Show the highest-priority runnable sprint story", Args: cobra.ExactArgs(1)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := sprintArg(cmd, flags.mode(), "sprint next", args[0])
		if err != nil {
			return err
		}
		dir, err := weaveStoryDir(cmd, flags.mode(), "sprint next")
		if err != nil {
			return err
		}
		q, err := loadWeaveQueue(dir)
		if err != nil {
			return err
		}
		s := findWeaveStory(q, id)
		if s == nil {
			return fmt.Errorf("sprint #%d not found", id)
		}
		next, err := nextSprintStory(s)
		if err != nil {
			return err
		}
		if flags.mode() == weavecli.OutputJSON {
			return ec(emitOK(cmd.OutOrStdout(), flags.mode(), "sprint next", map[string]any{"sprint": id, "story": next}))
		}
		if next == nil {
			fmt.Fprintf(cmd.OutOrStdout(), "sprint next: sprint #%d has no runnable unchecked story\n", id)
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "sprint next: [%s/%s] %s — %s\n", next.Priority, next.Status, next.Ref.ID, next.Title)
		return nil
	}
	flags.attach(cmd)
	return cmd
}

func newSprintFocusCmd() *cobra.Command {
	var flags weaveOutputFlags
	var repo, override string
	cmd := &cobra.Command{Use: "focus <sprint> <story>", Short: "Set current focus, enforcing priority-first execution", Args: cobra.ExactArgs(2)}
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		id, err := strconv.ParseInt(args[0], 10, 64)
		if err != nil {
			return err
		}
		root, err := normalizeStoryRoot(repo)
		if err != nil {
			return err
		}
		it, err := todopkg.ResolveRef(todopkg.RepoStore(root), args[1])
		if err != nil {
			return err
		}
		if it.Sprint != id {
			return fmt.Errorf("story %s belongs to sprint #%d, not #%d", it.ID, it.Sprint, id)
		}
		return runWeaveStoryMutate(cmd, id, "sprint focus", &flags, func(s *weaveStory) (string, error) {
			owner := weaveStoryConductorName(s, "")
			next, err := nextSprintStory(s)
			if err != nil {
				return "", err
			}
			if next != nil && todopkg.PriorityRank(it.Priority) > todopkg.PriorityRank(next.Priority) && strings.TrimSpace(override) == "" {
				return "", fmt.Errorf("priority-first policy: %s is %s while runnable %s is %s; pass --override <reason> to record an exception", it.ID, it.Priority, next.Ref.ID, next.Priority)
			}
			ref := sprintStoryRef{Repo: root, ID: it.ID}
			s.Execution = sprintExecution{PriorityFirst: true, CurrentFocus: &ref, Override: strings.TrimSpace(override)}
			body := fmt.Sprintf("focused story %s (%s)", it.ID, it.Priority)
			if s.Execution.Override != "" {
				body += "; priority override: " + s.Execution.Override
			}
			weaveStoryAppend(s, weaveStoryConductorName(s, ""), "decision", body)
			return fmt.Sprintf("sprint #%d focus %s%s", id, it.ID,
				sprintSeatDeliveryAdvisory(owner)), nil
		})
	}
	cmd.Flags().StringVar(&repo, "repo", "", "story repo root (default current)")
	cmd.Flags().StringVar(&override, "override", "", "required reason when bypassing a runnable higher-priority story")
	flags.attach(cmd)
	return cmd
}
