package weave

// A SPRINT'S TEXT MUST BE CORRECTABLE — AND EVERY CORRECTION MUST BE ON RECORD.
//
// `sprint add` took a title, a spec ref and acceptance criteria, and there was
// no way to change any of them afterwards. A chat session gets `/rename`; a
// sprint, which lives for weeks and whose acceptance text is the definition of
// done, got nothing. A typo in a title was permanent, and acceptance criteria
// written before the work was understood could never be corrected.
//
// # Why the audit is the load-bearing half
//
// Acceptance IS the done-definition. `sprint move ... done` and the goal gate
// both measure the sprint against it, so an unaudited edit would let a sprint
// pass a gate it never set — change the criteria to match what happened and the
// sprint closes green, with nothing anywhere recording that the bar moved.
//
// So every field change appends old → new to the sprint thread. The thread is
// append-only, which makes the edit a fact rather than a replacement, and the
// close gate remains meaningful because the bar it measured is still readable.
//
// Editing acceptance on a sprint that is already DONE additionally requires a
// stated reason: at that point the criteria are history, and history gets
// amended with an explanation or not at all.

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/role"
)

func newWeaveStoryEditCmd() *cobra.Command {
	var flags weaveOutputFlags
	var title, primaryGoal, spec, acceptance, epic, owner, reason string
	cmd := &cobra.Command{
		Use:   "edit <sprint>",
		Short: "Correct a sprint's title, primary goal, spec, acceptance, epic or owner — every change on the record",
		Long: `edit changes a sprint card's text after creation.

Every field change is appended to the sprint thread as "field: old → new". That
is not bookkeeping: acceptance is the definition of done, and both the close
gate and the goal checklist measure the sprint against it. An unaudited edit
would let a sprint pass a bar it never set, with nothing recording that the bar
moved. Changing acceptance on a sprint already in done requires --reason.

Passing --owner re-points the sprint's coordination address, so the new name
must be a unique NAME shown by "bashy agent list". It does NOT take the
conductor lease; use sprint take.`,
		Args: cobra.ExactArgs(1),
		Example: "  bashy sprint edit 99 --title \"Bashy Yoke II — coordination truthfulness\"\n" +
			"  bashy sprint edit 99 --acceptance \"...\" --reason \"criteria clarified after triage\"\n" +
			"  bashy sprint edit 99 --spec docs/bashy-yoke-framework.md",
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			if title == "" && primaryGoal == "" && spec == "" && acceptance == "" && epic == "" && owner == "" {
				return fmt.Errorf("nothing to change: pass at least one of --title --primary-goal --spec --acceptance --epic --owner")
			}
			expectedOwner := ""
			mutate := func() error {
				return runWeaveStoryMutate(cmd, id, "sprint edit", &flags, func(s *weaveStory) (string, error) {
					actor := weaveStoryConductorName(s, "")
					var changed []string
					record := func(field, from, to string) {
						weaveStoryAppend(s, actor, "system",
							fmt.Sprintf("edited %s: %q → %q", field, from, to))
						changed = append(changed, field)
					}
					if title != "" && title != s.Title {
						record("title", s.Title, title)
						s.Title = title
					}
					if primaryGoal != "" && primaryGoal != s.PrimaryGoal {
						record("primary goal", s.PrimaryGoal, primaryGoal)
						s.PrimaryGoal = strings.TrimSpace(primaryGoal)
					}
					if spec != "" && spec != s.SpecRef {
						record("spec", s.SpecRef, spec)
						s.SpecRef = spec
					}
					if epic != "" && epic != s.Epic {
						record("epic", s.Epic, epic)
						s.Epic = epic
					}
					if acceptance != "" && acceptance != s.Acceptance {
						// A closed sprint's criteria are history. Amending history
						// without saying why is the one edit that can rewrite what
						// a green close meant.
						if strings.EqualFold(strings.TrimSpace(s.Column), "done") &&
							strings.TrimSpace(reason) == "" {
							return "", fmt.Errorf(
								"sprint #%d is done — changing its acceptance rewrites what its close meant; pass --reason \"<why>\"", id)
						}
						record("acceptance", s.Acceptance, acceptance)
						s.Acceptance = acceptance
					}
					if owner != "" {
						// Same rule as take/resume/start: an owner is an ADDRESS.
						if err := validateSprintOwner(owner); err != nil {
							return "", err
						}
						owner, _ = canonicalFleetAgentName(owner)
					}
					if owner != "" && owner != s.Owner {
						if strings.TrimSpace(s.Owner) != expectedOwner {
							return "", fmt.Errorf("sprint #%d sprint manager changed concurrently from %s to %s", id, expectedOwner, s.Owner)
						}
						if sprintColumnOpen(s.Column) || s.currentBox().Running() {
							return "", fmt.Errorf("sprint #%d is active — change its sprint manager with `sprint take %d --owner %s`", id, id, owner)
						}
						record("owner", s.Owner, owner)
						s.Owner = owner
					}
					if len(changed) == 0 {
						return fmt.Sprintf("sprint #%d unchanged", id), nil
					}
					if r := strings.TrimSpace(reason); r != "" {
						weaveStoryAppend(s, actor, "decision", "edit reason: "+r)
					}
					return fmt.Sprintf("sprint #%d edited: %s", id, strings.Join(changed, ", ")), nil
				})
			}
			if owner == "" {
				return mutate()
			}
			return runSprintOwnerLifecycle(cmd, &flags, id, "sprint edit", "edit managed sprint owner", func() error {
				if err := validateSprintOwner(owner); err != nil {
					return err
				}
				owner, _ = canonicalFleetAgentName(owner)
				before, err := sprintOwnerSnapshot(id)
				if err != nil {
					return err
				}
				expectedOwner = strings.TrimSpace(before.Owner)
				if owner != expectedOwner && (sprintColumnOpen(before.Column) || before.currentBox().Running()) {
					return fmt.Errorf("sprint #%d is active — change its sprint manager with `sprint take %d --owner %s`", id, id, owner)
				}
				if expectedOwner != "" && !strings.EqualFold(expectedOwner, owner) {
					cwd, _ := os.Getwd()
					if err := retireSprintOwnerSession(cmd.Context(), id, expectedOwner, cwd); err != nil {
						return fmt.Errorf("cannot transfer sprint #%d manager from %s to %s: %w", id, expectedOwner, owner, err)
					}
				}
				return mutate()
			})
		},
	}
	cmd.Flags().StringVar(&title, "title", "", "new title")
	cmd.Flags().StringVar(&primaryGoal, "primary-goal", "", "new one-sentence primary outcome")
	cmd.Flags().StringVar(&spec, "spec", "", "new spec/handoff doc reference")
	cmd.Flags().StringVar(&acceptance, "acceptance", "", "new acceptance / done criteria")
	cmd.Flags().StringVar(&epic, "epic", "", "new epic grouping label")
	role.AttachOwner(cmd.Flags(), &owner, role.ProjectManager,
		"new sprint manager; must be a NAME shown by bashy agent list")
	cmd.Flags().StringVar(&reason, "reason", "", "why this edit is correct — required to amend a done sprint's acceptance")
	flags.attach(cmd)
	return cmd
}

// newWeaveStoryRmCmd removes a sprint card.
//
// Sprint was the only one of the three work layers with no removal path — todo
// has `rm`, weave has `abandon`/`reset`, sprint had nothing — so a card created
// by mistake was permanent board furniture. That asymmetry is also what stopped
// this very feature from being demonstrated by experiment: proving that a
// sprint could close over uncovered work needed a throwaway sprint, and there
// was no way to clean one up afterwards.
//
// The refusals are deliberately blunt. A sprint is a coordination object other
// agents address; deleting one out from under a live conductor or a linked run
// is not a tidy-up, it is a coordination failure.
func newWeaveStoryRmCmd() *cobra.Command {
	var flags weaveOutputFlags
	var force bool
	cmd := &cobra.Command{
		Use:     "rm <sprint>",
		Aliases: []string{"remove", "delete"},
		Short:   "Remove a sprint card (refuses while it holds a lease, linked runs, or an open column)",
		Long: `rm deletes a sprint card from the board.

It refuses while anything still depends on the card:

  a live conductor lease   somebody is working it right now
  linked runs              unlink or finish them first; the runs outlive the card
  an open column           move it out of doing first, which also closes its room

--force waives the column and lease checks for a card that was created by
mistake. It does NOT waive the linked-run check: a card with runs is the only
record of which repos that work touched, and deleting it strands them.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			return runWeaveStoryRemove(cmd, id, force, &flags)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "remove a mistaken card despite an open column or a held lease")
	flags.attach(cmd)
	return cmd
}

// runWeaveStoryRemove deletes the card under the queue lock.
//
// It is not runWeaveStoryMutate because that helper hands the callback a card to
// MODIFY and writes it back; removal has to edit the slice itself.
func runWeaveStoryRemove(cmd *cobra.Command, id int64, force bool, flags *weaveOutputFlags) error {
	mode := flags.mode()
	op := "sprint rm"
	dir, err := weaveStoryDir(cmd, mode, op)
	if err != nil {
		return err
	}
	var title string
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		idx := -1
		for i, s := range q.Stories {
			if s != nil && s.ID == id {
				idx = i
				break
			}
		}
		if idx < 0 {
			return fmt.Errorf("sprint #%d not found", id)
		}
		s := q.Stories[idx]
		title = s.Title
		// Linked runs are never waivable. The card is the only record of which
		// repos this work touched; removing it strands them with no way back.
		if len(s.Runs) > 0 {
			return fmt.Errorf("sprint #%d has %d linked run(s) — `bashy sprint unlink` them first (not waivable: the card is the only record of which repos they belong to)",
				id, len(s.Runs))
		}
		if !force {
			if holder, stale, free := weaveStoryLeaseState(s); !free && !stale {
				return fmt.Errorf("sprint #%d lease is held by %s — coordinate, or --force", id, holder)
			}
			if sprintColumnOpen(s.Column) {
				return fmt.Errorf("sprint #%d is in %s — `bashy sprint move %d done` (or backlog) first, or --force",
					id, s.Column, id)
			}
		}
		// Close the room before the card goes: the contact is the only pointer
		// to it, so dropping the card first would leak the room forever,
		// advertising a channel for a sprint that no longer exists.
		_ = closeSprintRoom(s, weaveStoryConductorName(s, ""))
		q.Stories = append(q.Stories[:idx], q.Stories[idx+1:]...)
		return nil
	})
	if lockErr != nil {
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") {
			code = weavecli.ExitInvalidArg
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, op, code, lockErr))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, op, map[string]any{"sprint": id, "removed": true}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: sprint #%d removed — %s\n", op, id, title)
	return nil
}
