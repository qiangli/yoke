package weave

import (
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
)

// newWeaveCommentCmd appends an entry to an issue's history thread. It
// is what lets the conductor reference a handoff/spec in the issue and
// have the working agent append feedback/progress/blockers to the same
// thread — so the back-and-forth is a durable record on the task, not a
// human relaying notes. Agents auto-sign with $WEAVE_AGENT.
func newWeaveCommentCmd() *cobra.Command {
	var flags weaveOutputFlags
	var kind, author, message string
	cmd := &cobra.Command{
		Use:   `comment <issue> ["<text>"]`,
		Short: "Append a comment to an issue's history thread",
		Long: `comment appends an entry to the issue's append-only history thread —
the durable back-channel between the conductor and the working agent.

  bashy weave comment 4 "spec in docs/p3-handoff.md; gate is 'make test'"
  bashy weave comment 4 --kind blocker "need the access-gate decision"
  bashy weave comment 4 --kind decision "going with the gloo path"

--kind   note|progress|blocker|decision|review (default note). A newest
         "blocker" surfaces the issue as blocked in 'weave list'/'status'.
--author who is speaking (default: $WEAVE_AGENT if set — so an agent
         signs its own name — else "conductor").`,
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("issue required")
			}
			if message == "" && len(args) < 2 {
				return fmt.Errorf("text required (positional or -m)")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			body := message
			if body == "" {
				body = strings.Join(args[1:], " ")
			}
			return runWeaveComment(cmd, id, author, kind, body, &flags)
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "note", "note|progress|blocker|decision|review")
	cmd.Flags().StringVar(&author, "author", "", "comment author (default $WEAVE_AGENT or \"conductor\")")
	cmd.Flags().StringVarP(&message, "message", "m", "", "comment text (alternative to the positional arg)")
	flags.attach(cmd)
	return cmd
}

func runWeaveComment(cmd *cobra.Command, id int64, author, kind, body string, flags *weaveOutputFlags) error {
	mode := flags.mode()
	if strings.TrimSpace(author) == "" {
		if a := strings.TrimSpace(os.Getenv("WEAVE_AGENT")); a != "" {
			author = a
		}
	}
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave comment",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	lockErr := withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, id)
		if it == nil {
			return fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))
		}
		weaveAppendComment(it, author, kind, body)
		return nil
	})
	if lockErr != nil {
		code := weavecli.ExitGenericFail
		if strings.Contains(lockErr.Error(), "not found") {
			code = weavecli.ExitInvalidArg
		}
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave comment", code, lockErr))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave comment", map[string]any{
			"issue": id, "kind": kind, "body": body,
		}))
	}
	fmt.Fprintf(cmd.OutOrStdout(), "weave comment: run #%d +%s\n", id, kind)
	return nil
}

// newWeaveCommentsCmd prints an issue's history thread.
func newWeaveCommentsCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   `comments <issue>`,
		Short: "Show an issue's history thread",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) < 1 {
				return fmt.Errorf("issue required")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("issue must be an integer: %q", args[0])
			}
			return runWeaveComments(cmd, id, &flags)
		},
	}
	flags.attach(cmd)
	return cmd
}

func runWeaveComments(cmd *cobra.Command, id int64, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave comments",
			weavecli.ExitPrecondFail, err))
	}
	dir, _ := weaveQueueDir(root)
	q, err := loadWeaveQueue(dir)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave comments",
			weavecli.ExitGenericFail, err))
	}
	it := findWeaveItem(q, id)
	if it == nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave comments",
			weavecli.ExitInvalidArg, fmt.Errorf("run #%d not found%s", id, weaveOtherActiveQueuesHintSuffix(dir))))
	}
	if mode == weavecli.OutputJSON {
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave comments", map[string]any{
			"issue": id, "owner": it.Owner, "comments": it.Comments,
		}))
	}
	out := cmd.OutOrStdout()
	owner := it.Owner
	if owner == "" {
		owner = "(unassigned)"
	}
	fmt.Fprintf(out, "run #%d [%s] owner=%s — %s\n", it.ID, it.State, owner, it.Title)
	if len(it.Comments) == 0 {
		fmt.Fprintln(out, "  (no comments)")
		return nil
	}
	for _, c := range it.Comments {
		fmt.Fprintf(out, "  [%s] %s (%s): %s\n",
			c.At.Format("01-02 15:04"), c.Author, c.Kind, c.Body)
	}
	return nil
}
