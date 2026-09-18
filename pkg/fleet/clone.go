package fleet

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// CLONING, AND WHY IT IS THE ANSWER TO "CAN I RUN TWO OF THESE?"
//
// An agent is a singleton identity: one conversation store, one kb attribution,
// one bus cursor, one API key. So "two instances of one agent" is not a thing
// that can be made to work by sharding resources — two concurrent tasks under
// one identity mix context, and an agent answering from another task's history
// is confidently, plausibly wrong. They would be "the same agent" only by
// spelling, which is exactly what makes them not the same.
//
// The two legitimate shapes, then:
//
//	SERIALIZE  — hand one agent N tasks and let it work them in turn. Same
//	             identity, same memory, one at a time. This is the default
//	             everywhere, and it needs no new concept.
//	CLONE      — mint a SECOND agent that starts where the first is now and
//	             diverges from there. Its own name, own store, own cursor, own
//	             kb attribution. Genuinely parallel, because genuinely separate.
//
// A clone is permanent (a new entry in `agents list`) or ephemeral (minted for
// one task, removed when it closes). Both are ordinary agent records; the only
// difference is who cleans them up.

// cloneNote is what the clone reports about its inherited context. The wording
// matters more than it looks: an operator deciding whether to re-brief a clone
// is deciding on this line.
const (
	noteFresh    = "fresh context — %s keeps its own state outside bashy, so there is nothing to branch"
	noteNoCloner = "fresh context — no context cloner is wired into this binary"
)

// CloneAgent mints a copy of an agent under a new name.
//
// What is inherited: the binding (tool + model, or the cascade), the band
// contract, role, ledger, instruction, functions, description. What is NOT:
// name, aliases and nick — those ARE the identity, and copying them is what
// would recreate the collision this exists to avoid.
func (c *Catalog) CloneAgent(parentName, newName string, ephemeral bool, task string) (Agent, error) {
	parent, ok := c.Agent(parentName)
	if !ok {
		return Agent{}, fmt.Errorf("fleet: no agent %q to clone — `bashy agent list`", parentName)
	}
	if err := validName(newName); err != nil {
		return Agent{}, err
	}

	clone := parent
	clone.Name = newName
	clone.Aliases = nil
	clone.Nick = "" // redrawn from the catalog, so a clone is never the parent's twin
	clone.AutoNick = ""
	clone.Derived = nil
	clone.Ring = 0
	clone.ClonedFrom = parent.Name
	clone.ClonedAt = time.Now().UTC().Format(time.RFC3339)
	clone.Ephemeral = ephemeral
	clone.Task = strings.TrimSpace(task)
	// A clone of a clone must not keep saying it is a clone of its GRANDparent.
	// An inherited description is a real one the operator wrote and is worth
	// keeping; the one this command generates is not, so it is regenerated.
	if clone.Description == "" || strings.HasPrefix(clone.Description, cloneOfPrefix) {
		clone.Description = cloneOfPrefix + parent.Name
	}
	return clone, nil
}

const cloneOfPrefix = "clone of "

// cloneContext branches the parent's conversation context onto the clone and
// says what it actually did. It never fails the clone: a minted agent with a
// fresh context is a usable agent, and an operator who is TOLD it is fresh can
// decide to re-brief it. What must not happen is silence.
func (c *Catalog) cloneContext(parent, clone Agent) string {
	if c.cfg.contextCloner == nil {
		return noteNoCloner
	}
	note, err := c.cfg.contextCloner(parent, clone)
	if err != nil {
		return "fresh context — " + err.Error()
	}
	if strings.TrimSpace(note) == "" {
		return fmt.Sprintf(noteFresh, parent.Tool)
	}
	return note
}

// nextCloneName derives a free name from the parent's, so the common case needs
// no naming decision: `agents clone elif` gives elif2, then elif3.
func (c *Catalog) nextCloneName(parent string) (string, error) {
	for i := 2; i < 1000; i++ {
		candidate := parent + strconv.Itoa(i)
		if _, taken := c.Agent(candidate); !taken {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("fleet: could not derive a free name from %q — name the clone explicitly", parent)
}

// cloneNameUnsafe matches what validName rejects, plus whitespace: a task id
// becomes part of a filename, so it is sanitized rather than trusted.
var cloneNameUnsafe = regexp.MustCompile(`[^A-Za-z0-9_-]+`)

// taskCloneName names an ephemeral clone after the work it was minted for, so
// `agents list --all` reads as a work list rather than a pile of elif7s.
func taskCloneName(parent, task string) string {
	t := strings.Trim(cloneNameUnsafe.ReplaceAllString(strings.TrimSpace(task), "-"), "-")
	if t == "" {
		return ""
	}
	return parent + "-" + t
}

func newAgentsClone(opts []Option) *cobra.Command {
	var ephemeral, fresh, force bool
	var task string
	c := &cobra.Command{
		Use:   "clone <parent> [<name>]",
		Short: "Mint a second agent that starts where another one is now",
		Long: "Mint a second agent that starts where another one is now.\n\n" +
			"An agent is ONE identity: one conversation store, one kb attribution,\n" +
			"one bus cursor. Two concurrent tasks under one identity mix context and\n" +
			"produce confidently wrong answers, so an agent is never started twice —\n" +
			"it is either handed its tasks in turn, or cloned.\n\n" +
			"A clone inherits its parent's context AS OF NOW and diverges from there.\n" +
			"It gets its own name, its own store, its own cursor. Whether the context\n" +
			"could actually be branched depends on the tool, and the command always\n" +
			"says which it got — a clone reported as inheriting context it did not\n" +
			"inherit is the failure this whole model exists to prevent.",
		Example: "  bashy agent clone elif                  # -> elif2, inherits elif's context\n" +
			"  bashy agent clone elif reviewer         # a named second opinion\n" +
			"  bashy agent clone elif --ephemeral --task 412\n" +
			"  bashy agent clone elif backup --fresh   # same binding, no history",
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			cat := New(opts...)
			parentName := args[0]

			parent, ok := cat.Agent(parentName)
			if !ok {
				return fmt.Errorf("fleet: no agent %q to clone — `bashy agent list`", parentName)
			}

			name := ""
			switch {
			case len(args) == 2:
				name = args[1]
			case ephemeral:
				name = taskCloneName(parent.Name, task)
				if name == "" {
					return fmt.Errorf("fleet: an ephemeral clone needs --task <id> (it is named after its work) " +
						"or an explicit name")
				}
			default:
				derived, err := cat.nextCloneName(parent.Name)
				if err != nil {
					return err
				}
				name = derived
			}

			clone, err := cat.CloneAgent(parent.Name, name, ephemeral, task)
			if err != nil {
				return err
			}
			if err := cat.claimName(KindAgent, clone.Name, nil, force); err != nil {
				return err
			}
			if err := cat.SaveAgent(clone); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "%s → %s (cloned from %s)\n", clone.Name, clone.MatrixKey(), parent.Name)

			note := "fresh context — --fresh was given"
			if !fresh {
				note = cat.cloneContext(parent, clone)
			}
			fmt.Fprintln(out, note)

			if ephemeral {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"ephemeral: hidden from `agents list` (use --all), remove with `bashy agent rm %s`\n", clone.Name)
			}
			for _, w := range cat.crossKindWarnings(KindAgent, clone.Name, nil) {
				fmt.Fprintln(cmd.ErrOrStderr(), "warning:", w)
			}
			return nil
		},
	}
	c.Flags().BoolVar(&ephemeral, "ephemeral", false, "mint for one task; hidden from `agents list` and meant to be removed when the task closes")
	c.Flags().StringVar(&task, "task", "", "the work this clone is for (names an ephemeral clone, recorded on any clone)")
	c.Flags().BoolVar(&fresh, "fresh", false, "do not branch the parent's context — same binding, no history")
	c.Flags().BoolVar(&force, "force", false, "take a name that already belongs to another entry")
	return c
}
