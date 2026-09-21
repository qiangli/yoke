package weave

// VOLUNTEERING FOR A STORY — claim, submit, yield.
//
// A sprint's stories were reachable (`sprint next`) and closable (`todo done`),
// but there was no way for an agent to say I AM WORKING ON THIS. Two agents
// could pick the same p0 off the same board and discover the collision in a
// merge conflict, and the manager learned who was doing what only by asking.
//
// Three verbs, and the pairing matters:
//
//	claim    I am taking this story        (exclusive; announces to the manager)
//	yield    I am giving it back unfinished (the honest inverse of claim)
//	submit   it is ready for review/merge   (hands the decision to the manager)
//	accept   the manager verified it        (the only sprint-story close path)
//
// `claim`/`yield` is the pair. `submit` is not its opposite — it is the
// transition OUT of the pair, into the manager's hands. Conflating submit with
// yield would lose exactly the distinction that matters to whoever is watching
// the board: work handed back is available, work submitted is not.
//
// # Why the manager is notified rather than expected to look
//
// The sprint manager cannot poll every story of every sprint it holds. A claim
// is the moment the manager's picture of the sprint changes, so the claim
// publishes it — to the manager's inbox, addressed by name, on the sprint's own
// topic and room. That is the same bus the sprint's reachability check already
// measures, so an unread claim shows up as an unanswered message and blocks the
// manager from handing off over it.
//
// # Why a claim is not a lock
//
// It is an exclusive ANNOUNCEMENT, not a mutex: it stops two agents starting
// the same story by accident, and it is deliberately steal-able by an operator
// (--force) because the alternative — work permanently pinned to an agent that
// died — is the worse failure. The real isolation is the weave workspace.

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/issue"
	"github.com/qiangli/yoke/pkg/role"
	todopkg "github.com/qiangli/yoke/pkg/todo"
)

// notifySprintOwner tells the manager that its picture of the sprint changed.
//
// Unlike pingSprintConductor this does NOT require a held lease: a story can be
// claimed while the seat is empty, and that is precisely when the manager most
// needs the message waiting when it arrives. Delivery failure is returned but
// never fatal to the claim — the durable record is the story itself, and losing
// a notification must not lose the work.
func notifySprintOwner(s *weaveStory, from, body string) error {
	owner := strings.TrimSpace(s.Owner)
	if owner == "" || owner == from {
		return nil
	}
	n := bus.Notification{
		Principal: from,
		Topic:     sprintTopic(s.ID),
		To:        owner,
		Body:      body,
		Priority:  bus.DeliveryQueued,
	}
	if s.Contact != nil {
		n.Room = s.Contact.Ref
	}
	return bus.Publish(n)
}

// resolveSprintStoryFor finds a story in one of the sprint's tracked roots.
func resolveSprintStoryFor(s *weaveStory, repo, ref string) (string, *issue.Issue, error) {
	roots := sprintDeclaredStoryRoots(s)
	if strings.TrimSpace(repo) != "" {
		root, err := normalizeStoryRoot(repo)
		if err != nil {
			return "", nil, err
		}
		roots = []string{root}
	}
	if len(roots) == 0 {
		if root, err := normalizeStoryRoot(""); err == nil {
			roots = []string{root}
		}
	}
	for _, root := range roots {
		it, err := todopkg.ResolveRef(todopkg.RepoStore(root), ref)
		if err == nil && it != nil {
			if it.Sprint != s.ID {
				return "", nil, fmt.Errorf("story %s belongs to sprint #%d, not #%d", it.ID, it.Sprint, s.ID)
			}
			return root, it, nil
		}
	}
	return "", nil, fmt.Errorf("story %q not found in any root tracked by sprint #%d", ref, s.ID)
}

func newSprintClaimCmd() *cobra.Command {
	var flags weaveOutputFlags
	var owner, repo string
	var force bool
	cmd := &cobra.Command{
		Use:     "claim <sprint> <story>",
		Aliases: []string{"take-story"},
		Short:   "Volunteer for a story on an active sprint — announces it to the manager",
		Long: `claim is how an agent volunteers. It marks the story as yours, and tells
the sprint's manager, so two agents cannot start the same p0 and discover it in
a merge conflict.

The claimant must be a live entry in "bashy agent" for the same reason a sprint
owner must: the manager will reply to this name, and a name nobody is behind
turns a collaboration into a wait.

A claim is an exclusive ANNOUNCEMENT, not a lock. --force takes a story held by
somebody else, because work pinned forever to an agent that died is worse than a
contested claim. The real isolation is the weave workspace.

Finish with submit (ready for review) or yield (handing it back unfinished).`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := sprintArg(cmd, flags.mode(), "sprint claim", args[0])
			if err != nil {
				return err
			}
			return runSprintStoryClaim(cmd, id, args[1], owner, repo, force, &flags)
		},
	}
	role.AttachOwner(cmd.Flags(), &owner, role.Assignee,
		"agent claiming the story (default $WEAVE_AGENT/$WEAVE_CONDUCTOR)")
	cmd.Flags().StringVar(&repo, "repo", "", "repo root holding the story")
	cmd.Flags().BoolVar(&force, "force", false, "take a story another agent holds")
	flags.attach(cmd)
	return cmd
}

func runSprintStoryClaim(cmd *cobra.Command, id int64, ref, as, repo string, force bool, flags *weaveOutputFlags) error {
	who := strings.TrimSpace(as)
	if who == "" {
		who = weaveConductorName("")
	}
	if err := validateSprintClaimant(who); err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), flags.mode(), "sprint claim", weavecli.ExitPrecondFail, err))
	}
	who, _ = canonicalFleetAgentName(who)
	return runWeaveStoryMutate(cmd, id, "sprint claim", flags, func(s *weaveStory) (string, error) {
		if !sprintColumnOpen(s.Column) {
			return "", fmt.Errorf("sprint #%d is %s — only an ACTIVE sprint takes volunteers", id, s.Column)
		}
		root, it, err := resolveSprintStoryFor(s, repo, ref)
		if err != nil {
			return "", err
		}
		if it.Status == todopkg.StatusDone || it.Status == issue.StatusClosed {
			return "", fmt.Errorf("story %s is already closed", it.ID)
		}
		if held := strings.TrimSpace(it.Assignee); held != "" && !strings.EqualFold(held, who) && !force {
			return "", fmt.Errorf("story %s is held by %s — coordinate with them (`bashy mb send %s ...`), or --force to take it",
				it.ID, held, held)
		}
		prev := strings.TrimSpace(it.Assignee)
		it.Assignee = who
		it.Status = todopkg.StatusAssigned
		if _, err := todopkg.RepoStore(root).Save(it); err != nil {
			return "", fmt.Errorf("record the claim: %w", err)
		}
		note := fmt.Sprintf("%s claimed story %s — %s", who, shortSprintStoryID(it.ID), it.Title)
		if prev != "" && !strings.EqualFold(prev, who) {
			note = fmt.Sprintf("%s TOOK story %s from %s — %s", who, shortSprintStoryID(it.ID), prev, it.Title)
		}
		weaveStoryAppend(s, who, "system", note)
		delivery := ""
		if err := notifySprintOwner(s, who, note+" (sprint #"+strconv.FormatInt(id, 10)+")"); err != nil {
			// Reported, never swallowed: a manager that was not told is a
			// manager that will be surprised, and silence here reads exactly
			// like a delivered message.
			delivery = fmt.Sprintf("; manager NOT notified (%v) — tell %s yourself", err, s.Owner)
		}
		return fmt.Sprintf("sprint #%d: %s claimed %s — submit when ready, yield to hand it back%s",
			id, who, shortSprintStoryID(it.ID), delivery), nil
	})
}

func newSprintYieldCmd() *cobra.Command {
	var flags weaveOutputFlags
	var as, repo, reason string
	cmd := &cobra.Command{
		Use:     "yield <sprint> <story>",
		Aliases: []string{"untake-story", "unclaim"},
		Short:   "Give a claimed story back to the sprint, unfinished",
		Long: `yield is the honest inverse of claim: this story is not finished and I am
not the one finishing it. The story returns to the open queue, and the manager
is told, so it stops counting on work nobody is doing.

It is deliberately NOT the same as submit. Yielded work is available for
somebody else; submitted work is waiting on the manager. Collapsing the two
would hide which of those a story is in — the one thing anybody scanning the
board needs to know.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			return runSprintStoryYield(cmd, id, args[1], as, repo, reason, &flags)
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "yielding agent (default $WEAVE_AGENT/$WEAVE_CONDUCTOR)")
	cmd.Flags().StringVar(&repo, "repo", "", "repo root holding the story")
	cmd.Flags().StringVarP(&reason, "message", "m", "", "why it is being handed back — what the next agent should know")
	flags.attach(cmd)
	return cmd
}

func runSprintStoryYield(cmd *cobra.Command, id int64, ref, as, repo, reason string, flags *weaveOutputFlags) error {
	who := strings.TrimSpace(as)
	if who == "" {
		who = weaveConductorName("")
	}
	return runWeaveStoryMutate(cmd, id, "sprint yield", flags, func(s *weaveStory) (string, error) {
		root, it, err := resolveSprintStoryFor(s, repo, ref)
		if err != nil {
			return "", err
		}
		held := strings.TrimSpace(it.Assignee)
		if held == "" {
			return "", fmt.Errorf("story %s is not claimed by anyone", it.ID)
		}
		it.Assignee = ""
		it.Status = todopkg.StatusTodo
		if _, err := todopkg.RepoStore(root).Save(it); err != nil {
			return "", fmt.Errorf("record the yield: %w", err)
		}
		note := fmt.Sprintf("%s yielded story %s back to the queue", who, shortSprintStoryID(it.ID))
		if r := strings.TrimSpace(reason); r != "" {
			note += ": " + r
		}
		weaveStoryAppend(s, who, "decision", note)
		delivery := ""
		if err := notifySprintOwner(s, who, note+" (sprint #"+strconv.FormatInt(id, 10)+")"); err != nil {
			delivery = fmt.Sprintf("; manager NOT notified (%v)", err)
		}
		return fmt.Sprintf("sprint #%d: %s yielded %s — it is open again%s",
			id, who, shortSprintStoryID(it.ID), delivery), nil
	})
}

func newSprintSubmitCmd() *cobra.Command {
	var flags weaveOutputFlags
	var as, repo, evidence string
	cmd := &cobra.Command{
		Use:   "submit <sprint> <story>",
		Short: "Hand a finished story to the manager for merge or closure",
		Long: `submit says the work is done and the decision is now the manager's.

An optional -m message can carry a commit, branch, test result, or other useful
continuity note. It is context, not a mandatory review or verification gate.

The story stays assigned to you until the manager closes it. That is
deliberate: work waiting on manager action is not available for somebody else to pick
up, and pretending otherwise is how two agents end up on one story.`,
		Args:    cobra.ExactArgs(2),
		Example: "  bashy sprint submit 99 3ceb3afc -m \"coreutils 9fa9f08; go test ./pkg/weave green\"",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			return runSprintStorySubmit(cmd, id, args[1], as, repo, evidence, &flags)
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "submitting agent (default $WEAVE_AGENT/$WEAVE_CONDUCTOR)")
	cmd.Flags().StringVar(&repo, "repo", "", "repo root holding the story")
	cmd.Flags().StringVarP(&evidence, "message", "m", "", "optional continuity note: what changed, commit/branch, or checks run")
	flags.attach(cmd)
	return cmd
}

func runSprintStorySubmit(cmd *cobra.Command, id int64, ref, as, repo, evidence string, flags *weaveOutputFlags) error {
	who := strings.TrimSpace(as)
	if who == "" {
		who = weaveConductorName("")
	}
	return runWeaveStoryMutate(cmd, id, "sprint submit", flags, func(s *weaveStory) (string, error) {
		_, it, err := resolveSprintStoryFor(s, repo, ref)
		if err != nil {
			return "", err
		}
		if held := strings.TrimSpace(it.Assignee); held != "" && !strings.EqualFold(held, who) {
			return "", fmt.Errorf("story %s is held by %s, not %s — claim it first, or let them submit it", it.ID, held, who)
		}
		if strings.TrimSpace(it.Assignee) == "" {
			return "", fmt.Errorf("story %s is unclaimed — run `bashy sprint claim %d %s --owner %s` before submission", it.ID, id, it.ID, who)
		}
		note := fmt.Sprintf("%s submitted story %s for merge/closure", who, shortSprintStoryID(it.ID))
		if message := strings.TrimSpace(evidence); message != "" {
			note += ": " + message
		}
		weaveStoryAppend(s, who, "decision", note)
		delivery := ""
		if err := notifySprintOwner(s, who, note+" (sprint #"+strconv.FormatInt(id, 10)+")"); err != nil {
			delivery = fmt.Sprintf("; manager NOT notified (%v) — tell %s yourself", err, s.Owner)
		}
		owner := s.Owner
		if owner == "" {
			owner = "the manager"
		}
		_ = time.Now
		return fmt.Sprintf("sprint #%d: %s submitted %s — %s can merge or close it%s",
			id, who, shortSprintStoryID(it.ID), owner, delivery), nil
	})
}

func newSprintAcceptCmd() *cobra.Command {
	var flags weaveOutputFlags
	var repo, evidence string
	cmd := &cobra.Command{
		Use:   "accept <sprint> <story>",
		Short: "Close a claimed and submitted story",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			return runSprintStoryAccept(cmd, id, args[1], repo, evidence, &flags)
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "repo root holding the story")
	cmd.Flags().StringVarP(&evidence, "message", "m", "", "optional manager continuity note")
	flags.attach(cmd)
	return cmd
}

func sprintSubmissionEvidence(s *weaveStory, storyID string) bool {
	needle := " submitted story " + shortSprintStoryID(storyID) + " for "
	for _, c := range s.Thread {
		if strings.Contains(c.Body, needle) {
			return true
		}
	}
	return false
}

func sprintAcceptanceEvidence(s *weaveStory, storyID string) bool {
	needle := " accepted story " + shortSprintStoryID(storyID)
	for _, c := range s.Thread {
		if strings.Contains(c.Body, needle) {
			return true
		}
	}
	return false
}

func runSprintStoryAccept(cmd *cobra.Command, id int64, ref, repo, evidence string, flags *weaveOutputFlags) error {
	return runWeaveStoryMutate(cmd, id, "sprint accept", flags, func(s *weaveStory) (string, error) {
		actor := weaveStoryConductorName(s, "")
		if s.Lease == nil || !strings.EqualFold(strings.TrimSpace(s.Lease.Holder), actor) {
			return "", fmt.Errorf("only sprint #%d's current manager may accept stories", id)
		}
		root, it, err := resolveSprintStoryFor(s, repo, ref)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(it.Assignee) == "" {
			return "", fmt.Errorf("story %s was never claimed", it.ID)
		}
		if !sprintSubmissionEvidence(s, it.ID) {
			return "", fmt.Errorf("story %s has no submitted delivery evidence — run `bashy sprint submit %d %s -m \"<evidence>\"` first", it.ID, id, it.ID)
		}
		it.Status = todopkg.StatusDone
		now := time.Now().UTC()
		it.Closed = &now
		it.ClosedBy = actor
		if _, err := todopkg.RepoStore(root).Save(it); err != nil {
			return "", err
		}
		note := fmt.Sprintf("%s accepted story %s", actor, shortSprintStoryID(it.ID))
		if message := strings.TrimSpace(evidence); message != "" {
			note += ": " + message
		}
		weaveStoryAppend(s, actor, "decision", note)
		return fmt.Sprintf("sprint #%d: accepted story %s", id, shortSprintStoryID(it.ID)), nil
	})
}

// sprintStoryClosureAudit keeps lifecycle completion about durable state, not
// review ceremony: every story must be closed, but no acceptance note or test
// artifact is mandatory when the sprint has no applicable verification gate.
func sprintStoryClosureAudit(s *weaveStory) error {
	for _, root := range sprintDeclaredStoryRoots(s) {
		items, err := todopkg.List(todopkg.RepoStore(root), "")
		if err != nil {
			return fmt.Errorf("sprint #%d cannot check stories in %s: %w", s.ID, root, err)
		}
		for _, it := range items {
			if it.Sprint != s.ID {
				continue
			}
			if it.Status != todopkg.StatusDone && it.Status != issue.StatusClosed {
				return fmt.Errorf("sprint #%d cannot end — story %s is still %s; close it or move it to another sprint", s.ID, it.ID, it.Status)
			}
		}
	}
	return nil
}
