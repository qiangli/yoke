package ask

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// ApproveSchemaVersion tags the JSON envelopes `approve` prints.
const ApproveSchemaVersion = "bashy-approve-v1"

// ExitApprovalDenied is the exit status of `approve wait` for a deny, distinct
// from a timeout so a script can tell "refused" from "nobody answered".
const (
	ExitApprovalDenied  = 1
	ExitApprovalTimeout = 124
)

// ApprovalExit carries a specific process exit status out of a command. A host
// that maps errors to exit codes should honour Code.
type ApprovalExit struct {
	Code int
	Msg  string
}

func (e *ApprovalExit) Error() string { return e.Msg }

// NewApproveCmd returns the `approve` command tree — the host-agnostic entry
// point a front end mounts (`bashy approve`). It is a view over the ask store:
// no second queue, no second log.
func NewApproveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "approve",
		Short: "list and decide pending approvals (yielded @confirm, ACP permission requests, chat grants)",
		Long: `approve is the one queue of decisions waiting for a human.

An approval is raised when an action falls outside what an agent may do alone:
a Bash# @confirm that yielded (exit 6, input_required), an ACP permission
request from an agent whose permission mode is ask or review, or a chat grant.
Each carries a token — the hash of the exact argv — so a decision applies to
that operation and no other.

  bashy approve list
  bashy approve show 3fa9c1d2
  bashy approve allow 3fa9c1d2            # then resume with --confirm=TOKEN:yes
  bashy approve deny  3fa9c1d2 --reason "not this repo"
  bashy approve wait  3fa9c1d2 --timeout 5m

Decisions are recorded once (first decider wins) and announced on the room
timeline as approval events. The default permission mode, auto, never raises
an approval.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newApproveListCmd(), newApproveShowCmd(), newApproveAllowCmd(),
		newApproveDenyCmd(), newApproveWaitCmd(), newApproveRequestCmd())
	return cmd
}

type approvalView struct {
	SchemaVersion string           `json:"schema_version"`
	ID            string           `json:"id"`
	State         string           `json:"state"`
	Token         string           `json:"token"`
	Source        string           `json:"source"`
	Argv          []string         `json:"argv,omitempty"`
	Effects       []string         `json:"effects,omitempty"`
	Sprint        string           `json:"sprint,omitempty"`
	Session       string           `json:"session,omitempty"`
	Mode          string           `json:"mode,omitempty"`
	Prompt        string           `json:"prompt,omitempty"`
	Options       []ApprovalOption `json:"options,omitempty"`
	Requester     Requester        `json:"requester"`
	Created       time.Time        `json:"created"`
	Expires       time.Time        `json:"expires"`
	Decision      *Decision        `json:"decision,omitempty"`
	// Resume is the exact flag a yielded @confirm is re-run with.
	Resume string `json:"resume,omitempty"`
}

func viewOf(r Request) approvalView {
	v := approvalView{
		SchemaVersion: ApproveSchemaVersion,
		ID:            r.ID,
		State:         "pending",
		Prompt:        r.Prompt,
		Requester:     r.Requester,
		Created:       r.Created,
		Expires:       r.Expires,
	}
	if a := r.Approval; a != nil {
		v.Token, v.Source, v.Argv, v.Effects = a.Token, a.Source, a.Argv, a.Effects
		v.Sprint, v.Session, v.Mode, v.Options = a.Sprint, a.Session, a.Mode, a.Options
	}
	if d, ok := LoadDecision(r.ID); ok {
		v.State = d.Verdict
		v.Decision = &d
		v.Resume = ResumeFlag(v.Token, d)
	}
	return v
}

// ResumeFlag renders a decision as @confirm's resume form.
func ResumeFlag(token string, d Decision) string {
	answer := "no"
	if d.Allowed() {
		answer = "yes"
	}
	return "--confirm=" + token + ":" + answer
}

func newApproveListCmd() *cobra.Command {
	var asJSON, all bool
	cmd := &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "show pending approvals (--all includes recently decided)",
		Args:    cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			var rs []Request
			var err error
			if all {
				rs, err = ListApprovals()
			} else {
				rs, err = PendingApprovals()
			}
			if err != nil {
				return err
			}
			views := make([]approvalView, 0, len(rs))
			for _, r := range rs {
				views = append(views, viewOf(r))
			}
			w := c.OutOrStdout()
			if asJSON {
				return json.NewEncoder(w).Encode(map[string]any{
					"schema_version": ApproveSchemaVersion,
					"approvals":      views,
				})
			}
			if len(views) == 0 {
				fmt.Fprintln(w, "nothing pending")
				return nil
			}
			fmt.Fprintf(w, "%-10s %-9s %-8s %-8s %s\n", "ID", "STATE", "SOURCE", "TOKEN", "OPERATION")
			for _, v := range views {
				fmt.Fprintf(w, "%-10s %-9s %-8s %-8s %s\n", shortID(v.ID), v.State, v.Source, v.Token,
					clip(strings.Join(v.Argv, " "), 80))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&all, "all", false, "include approvals decided within the retention window")
	return cmd
}

func newApproveShowCmd() *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "show ID",
		Short: "show one approval: who asked, the exact argv, effects and token",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, err := findApproval(args[0])
			if err != nil {
				return err
			}
			v := viewOf(r)
			w := c.OutOrStdout()
			if asJSON {
				return json.NewEncoder(w).Encode(v)
			}
			fmt.Fprintf(w, "id:        %s\n", v.ID)
			fmt.Fprintf(w, "state:     %s\n", v.State)
			fmt.Fprintf(w, "source:    %s\n", v.Source)
			fmt.Fprintf(w, "token:     %s\n", v.Token)
			fmt.Fprintf(w, "argv:      %s\n", strings.Join(v.Argv, " "))
			if len(v.Effects) > 0 {
				fmt.Fprintf(w, "effects:   %s\n", strings.Join(v.Effects, ", "))
			}
			if v.Sprint != "" {
				fmt.Fprintf(w, "sprint:    %s\n", v.Sprint)
			}
			if v.Session != "" {
				fmt.Fprintf(w, "session:   %s\n", v.Session)
			}
			fmt.Fprintf(w, "requester: %s\n", requesterLabel(v.Requester))
			if v.Prompt != "" {
				fmt.Fprintf(w, "prompt:    %s\n", v.Prompt)
			}
			if r.Approval != nil {
				for _, o := range r.Approval.Options {
					fmt.Fprintf(w, "option:    %s — %s\n", o.ID, o.Name)
				}
			}
			if d := v.Decision; d != nil {
				fmt.Fprintf(w, "decision:  %s by %s via %s%s\n", d.Verdict, d.By, d.Via, alwaysSuffix(d))
				if v.Resume != "" {
					fmt.Fprintf(w, "resume:    %s\n", v.Resume)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newApproveAllowCmd() *cobra.Command {
	var always bool
	var token, reason, option string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "allow ID",
		Short: "allow an approval; prints the resume flag (--confirm=TOKEN:yes)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			if option != "" {
				r, err := findApproval(args[0])
				if err != nil {
					return err
				}
				if err := checkOption(r, option); err != nil {
					return err
				}
			}
			return decideCmd(c, args[0], Decision{Verdict: VerdictAllow, Always: always, Reason: reason, OptionID: option}, token, asJSON)
		},
	}
	cmd.Flags().BoolVar(&always, "always", false, "standing allow for this token (allow-always on ACP and chat grant)")
	cmd.Flags().StringVar(&option, "option", "", "pick one of the approval's offered choices by id (see `approve show`)")
	cmd.Flags().StringVar(&token, "token", "", "require the approval's token to match (proves you saw the same plan)")
	cmd.Flags().StringVar(&reason, "reason", "", "why (recorded)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newApproveDenyCmd() *cobra.Command {
	var token, reason string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "deny ID",
		Short: "deny an approval; prints the resume flag (--confirm=TOKEN:no)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return decideCmd(c, args[0], Decision{Verdict: VerdictDeny, Reason: reason}, token, asJSON)
		},
	}
	cmd.Flags().StringVar(&token, "token", "", "require the approval's token to match")
	cmd.Flags().StringVar(&reason, "reason", "", "why (recorded)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

// checkOption refuses an --option the approval did not offer, naming the
// ones it did.
func checkOption(r Request, option string) error {
	if r.Approval == nil || len(r.Approval.Options) == 0 {
		return fmt.Errorf("approve: %s offers no options; allow or deny it without --option", shortID(r.ID))
	}
	var ids []string
	for _, o := range r.Approval.Options {
		if o.ID == option {
			return nil
		}
		ids = append(ids, o.ID)
	}
	return fmt.Errorf("approve: %s has no option %q (options: %s)", shortID(r.ID), option, strings.Join(ids, ", "))
}

func decideCmd(c *cobra.Command, id string, d Decision, token string, asJSON bool) error {
	r, err := Decide(id, d, DecideOpts{Token: token})
	if err != nil {
		return err
	}
	v := viewOf(r)
	w := c.OutOrStdout()
	if asJSON {
		return json.NewEncoder(w).Encode(v)
	}
	fmt.Fprintln(w, v.Resume)
	fmt.Fprintf(c.ErrOrStderr(), "%s %s (token %s)\n", d.Verdict, shortID(r.ID), v.Token)
	return nil
}

func newApproveWaitCmd() *cobra.Command {
	var timeout time.Duration
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "wait ID",
		Short: "block until an approval is decided; exit 0 allow, 1 deny, 124 timeout",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, err := findApproval(args[0])
			if err != nil {
				return err
			}
			ctx := c.Context()
			if ctx == nil {
				ctx = context.Background()
			}
			if timeout > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}
			d, err := WaitDecision(ctx, r.ID)
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) {
					return &ApprovalExit{Code: ExitApprovalTimeout, Msg: fmt.Sprintf("approve: %s still undecided after %s", shortID(r.ID), timeout)}
				}
				return err
			}
			v := viewOf(r)
			v.State, v.Decision, v.Resume = d.Verdict, &d, ResumeFlag(v.Token, d)
			w := c.OutOrStdout()
			if asJSON {
				if err := json.NewEncoder(w).Encode(v); err != nil {
					return err
				}
			} else {
				fmt.Fprintln(w, v.Resume)
			}
			if !d.Allowed() {
				return &ApprovalExit{Code: ExitApprovalDenied, Msg: fmt.Sprintf("approve: %s denied by %s", shortID(r.ID), d.By)}
			}
			return nil
		},
	}
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "give up after this long (0 = wait for the approval's own expiry)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

// newApproveRequestCmd raises an approval from outside Go — the form a script, a
// dag smoke or a non-Go harness uses. Go producers call Raise directly.
func newApproveRequestCmd() *cobra.Command {
	var source, token, sprint, session string
	var effects []string
	var ttl time.Duration
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "request [flags] -- ARGV...",
		Short: "raise an approval for ARGV (prints its id)",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			r, err := Raise(ApprovalSpec{
				Source: source, Argv: args, Effects: effects, Sprint: sprint,
				Session: session, Token: token, TTL: ttl,
				Prompt: "approval requested for " + strings.Join(args, " "),
			})
			if err != nil {
				return err
			}
			w := c.OutOrStdout()
			if asJSON {
				return json.NewEncoder(w).Encode(viewOf(r))
			}
			fmt.Fprintln(w, r.ID)
			return nil
		},
	}
	cmd.Flags().StringVar(&source, "source", SourceConfirm, "where it was raised: confirm|acp|chat")
	cmd.Flags().StringVar(&token, "token", "", "token to carry through (default: hash of ARGV)")
	cmd.Flags().StringSliceVar(&effects, "effects", nil, "declared effects of the operation")
	cmd.Flags().StringVar(&sprint, "sprint", "", "sprint the request belongs to")
	cmd.Flags().StringVar(&session, "session", "", "live session the request belongs to")
	cmd.Flags().DurationVar(&ttl, "ttl", DefaultApprovalTTL, "how long it waits for a decision")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func findApproval(id string) (Request, error) {
	r, err := Find(id)
	if err != nil {
		return Request{}, err
	}
	if !r.IsApproval() {
		return Request{}, fmt.Errorf("approve: %s is a value request — use `bashy ask claim %s`", shortID(r.ID), shortID(r.ID))
	}
	return r, nil
}

func alwaysSuffix(d *Decision) string {
	if d.Always {
		return " (always)"
	}
	return ""
}

func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}
