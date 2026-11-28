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

A typed "needs you" request is raised the same way, without an argv:

  bashy approve request --kind decision --title "Ship the hotfix?" \
      --option ship:"Ship now" --option hold --recommend ship \
      --ref sprint:741 --deadline 4h --risk deploy
  bashy approve allow 3fa9c1d2 --option ship --reason "tests green"

It goes first to the human's delegate when one covers it (see
` + "`approve delegate`" + `), and to the human otherwise; risk tags deploy,
security, credentials, money, delete and access stay with the human unless
the human allows them. A delegate's answer is recorded on the human's behalf;
the human can --override it or revoke the delegation at any time.

Decisions are recorded once (first decider wins) and announced on the room
timeline as approval events. The default permission mode, auto, never raises
an approval.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(newApproveListCmd(), newApproveShowCmd(), newApproveAllowCmd(),
		newApproveDenyCmd(), newApproveWaitCmd(), newApproveRequestCmd(), newApproveDelegateCmd())
	return cmd
}

type approvalView struct {
	SchemaVersion string    `json:"schema_version"`
	ID            string    `json:"id"`
	State         string    `json:"state"`
	Token         string    `json:"token"`
	Source        string    `json:"source"`
	Argv          []string  `json:"argv,omitempty"`
	Effects       []string  `json:"effects,omitempty"`
	Sprint        string    `json:"sprint,omitempty"`
	Session       string    `json:"session,omitempty"`
	Mode          string    `json:"mode,omitempty"`
	Prompt        string    `json:"prompt,omitempty"`
	Requester     Requester `json:"requester"`
	Created       time.Time `json:"created"`
	Expires       time.Time `json:"expires"`
	Decision      *Decision `json:"decision,omitempty"`
	// Resume is the exact flag a yielded @confirm is re-run with.
	Resume string `json:"resume,omitempty"`
	// Need, Options and NeedState describe a typed needs request.
	Need      *Need            `json:"need,omitempty"`
	Options   []ApprovalOption `json:"options,omitempty"`
	NeedState string           `json:"need_state,omitempty"`
	Prior     []Decision       `json:"prior_decisions,omitempty"`
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
		v.Sprint, v.Session, v.Mode = a.Sprint, a.Session, a.Mode
		if a.Need != nil {
			v.Need, v.Options = a.Need, a.Options
			v.NeedState = NeedState(r, time.Now())
			v.Prior = PriorDecisions(r.ID)
		}
	}
	if d, ok := LoadDecision(r.ID); ok {
		v.State = d.Verdict
		v.Decision = &d
		if v.Need == nil {
			v.Resume = ResumeFlag(v.Token, d)
		}
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
	var asJSON, all, onBehalf bool
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
				v := viewOf(r)
				if onBehalf && (v.Decision == nil || v.Decision.OnBehalfOf == "") {
					continue
				}
				views = append(views, v)
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
				what := strings.Join(v.Argv, " ")
				state := v.State
				if v.Need != nil {
					what = v.Need.Kind + ": " + v.Need.Title
					if v.Decision != nil && v.Decision.OnBehalfOf != "" {
						what += " (" + v.Decision.By + " for " + v.Decision.OnBehalfOf + ")"
					}
					if v.Decision == nil {
						state = v.NeedState
					}
				}
				fmt.Fprintf(w, "%-10s %-9s %-8s %-8s %s\n", shortID(v.ID), state, v.Source, v.Token,
					clip(what, 80))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	cmd.Flags().BoolVar(&all, "all", false, "include approvals decided within the retention window")
	cmd.Flags().BoolVar(&onBehalf, "on-behalf", false, "only decisions a delegate made on the human's behalf (with --all)")
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
			if n := v.Need; n != nil {
				fmt.Fprintf(w, "kind:      %s\n", n.Kind)
				fmt.Fprintf(w, "title:     %s\n", n.Title)
				fmt.Fprintf(w, "need:      %s (route %s)\n", v.NeedState, n.Route)
				for _, o := range v.Options {
					mark := ""
					if o.ID == n.Recommend {
						mark = " (recommended)"
					}
					fmt.Fprintf(w, "option:    %s\n", strings.TrimSpace(o.ID+" "+o.Name)+mark)
				}
				for _, ref := range n.Refs {
					fmt.Fprintf(w, "ref:       %s\n", ref)
				}
				if len(n.Risk) > 0 {
					fmt.Fprintf(w, "risk:      %s\n", strings.Join(n.Risk, ", "))
				}
				if !n.Deadline.IsZero() {
					fmt.Fprintf(w, "deadline:  %s\n", n.Deadline.Format(time.RFC3339))
				}
				fmt.Fprintf(w, "human:     %s\n", n.Human)
				if n.Delegate != "" {
					fmt.Fprintf(w, "delegate:  %s\n", n.Delegate)
				}
			}
			fmt.Fprintf(w, "requester: %s\n", requesterLabel(v.Requester))
			if v.Prompt != "" {
				fmt.Fprintf(w, "prompt:    %s\n", v.Prompt)
			}
			if d := v.Decision; d != nil {
				fmt.Fprintf(w, "decision:  %s by %s via %s%s%s\n", d.Verdict, d.By, d.Via, alwaysSuffix(d), needSuffix(d))
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
	var always, override bool
	var token, reason, option string
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "allow ID",
		Short: "allow an approval; prints the resume flag (--confirm=TOKEN:yes); --option answers a needs request",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return decideCmd(c, args[0], Decision{Verdict: VerdictAllow, Always: always, Reason: reason, OptionID: option}, token, asJSON, override)
		},
	}
	cmd.Flags().StringVar(&option, "option", "", "the option chosen (needs requests)")
	cmd.Flags().BoolVar(&override, "override", false, "replace the standing decision on a needs request (the human only)")
	cmd.Flags().BoolVar(&always, "always", false, "standing allow for this token (allow-always on ACP and chat grant)")
	cmd.Flags().StringVar(&token, "token", "", "require the approval's token to match (proves you saw the same plan)")
	cmd.Flags().StringVar(&reason, "reason", "", "why (recorded)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func newApproveDenyCmd() *cobra.Command {
	var token, reason string
	var asJSON, override bool
	cmd := &cobra.Command{
		Use:   "deny ID",
		Short: "deny an approval; prints the resume flag (--confirm=TOKEN:no)",
		Args:  cobra.ExactArgs(1),
		RunE: func(c *cobra.Command, args []string) error {
			return decideCmd(c, args[0], Decision{Verdict: VerdictDeny, Reason: reason}, token, asJSON, override)
		},
	}
	cmd.Flags().BoolVar(&override, "override", false, "replace the standing decision on a needs request (the human only)")
	cmd.Flags().StringVar(&token, "token", "", "require the approval's token to match")
	cmd.Flags().StringVar(&reason, "reason", "", "why (recorded)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit JSON")
	return cmd
}

func decideCmd(c *cobra.Command, id string, d Decision, token string, asJSON, override bool) error {
	var r Request
	var err error
	if override {
		r, err = Override(id, d, DecideOpts{Token: token})
	} else {
		r, err = Decide(id, d, DecideOpts{Token: token})
	}
	if err != nil {
		return err
	}
	v := viewOf(r)
	w := c.OutOrStdout()
	if asJSON {
		return json.NewEncoder(w).Encode(v)
	}
	if v.Need != nil {
		fmt.Fprintf(w, "%s %s%s\n", v.State, shortID(r.ID), needSuffix(v.Decision))
		return nil
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
			v.State, v.Decision = d.Verdict, &d
			if v.Need == nil {
				v.Resume = ResumeFlag(v.Token, d)
			}
			w := c.OutOrStdout()
			if asJSON {
				if err := json.NewEncoder(w).Encode(v); err != nil {
					return err
				}
			} else if v.Need != nil {
				fmt.Fprintf(w, "%s %s%s\n", d.Verdict, shortID(r.ID), needSuffix(&d))
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
	var kind, title, recommend, deadline, urgency, key string
	var options, refs, risk []string
	cmd := &cobra.Command{
		Use:   "request [flags] -- ARGV... | request --kind K --title T [--option ID[:LABEL]]...",
		Short: "raise an approval for ARGV, or a typed needs request with --kind (prints its id)",
		Args: func(c *cobra.Command, args []string) error {
			if kind != "" {
				if len(args) > 0 {
					return errors.New("approve: a --kind request takes no ARGV")
				}
				return nil
			}
			return cobra.MinimumNArgs(1)(c, args)
		},
		RunE: func(c *cobra.Command, args []string) error {
			if kind != "" {
				spec := NeedSpec{Kind: kind, Title: title, Recommend: recommend, Refs: refs,
					Urgency: urgency, Risk: risk, Key: key, Sprint: sprint}
				for _, o := range options {
					id, label, _ := strings.Cut(o, ":")
					if strings.TrimSpace(id) == "" {
						return fmt.Errorf("approve: empty --option %q", o)
					}
					spec.Options = append(spec.Options, ApprovalOption{ID: id, Name: label})
				}
				if deadline != "" {
					t, err := parseDeadline(deadline, time.Now())
					if err != nil {
						return err
					}
					spec.Deadline = t
				}
				r, err := RaiseNeed(spec)
				if err != nil {
					return err
				}
				w := c.OutOrStdout()
				if asJSON {
					return json.NewEncoder(w).Encode(viewOf(r))
				}
				fmt.Fprintln(w, r.ID)
				return nil
			}
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
	cmd.Flags().StringVar(&kind, "kind", "", "raise a typed needs request: decision|approval|action|info")
	cmd.Flags().StringVar(&title, "title", "", "the question, as the human will read it (with --kind)")
	cmd.Flags().StringArrayVar(&options, "option", nil, "an option ID[:LABEL] (repeatable, with --kind)")
	cmd.Flags().StringVar(&recommend, "recommend", "", "the recommended option ID (with --kind)")
	cmd.Flags().StringArrayVar(&refs, "ref", nil, "a reference: sprint:N, story:ID, commit:SHA, a path or URL (repeatable)")
	cmd.Flags().StringVar(&deadline, "deadline", "", "answer by: RFC 3339 time or a duration from now, e.g. 4h (with --kind)")
	cmd.Flags().StringVar(&urgency, "urgency", "normal", "low|normal|urgent (with --kind)")
	cmd.Flags().StringArrayVar(&risk, "risk", nil, "a risk tag, e.g. deploy, security, credentials, money, delete, access (repeatable)")
	cmd.Flags().StringVar(&key, "key", "", "idempotency key: raising the same key again returns the open request, and key:K names it in show/allow/deny/wait (default kind+title)")
	return cmd
}

func parseDeadline(s string, now time.Time) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	if d, err := time.ParseDuration(s); err == nil && d > 0 {
		return now.Add(d), nil
	}
	return time.Time{}, fmt.Errorf("approve: --deadline %q is neither an RFC 3339 time nor a positive duration", s)
}

func needSuffix(d *Decision) string {
	if d == nil {
		return ""
	}
	s := ""
	if d.OptionID != "" {
		s += " option " + d.OptionID
	}
	if d.OnBehalfOf != "" {
		s += " (by " + d.By + " on behalf of " + d.OnBehalfOf + ")"
	}
	if d.Overrides != "" {
		s += " (overrides " + d.Overrides + ")"
	}
	return s
}

// newApproveDelegateCmd is the human's one verb over their delegation: show it,
// set it, or revoke it.
func newApproveDelegateCmd() *cobra.Command {
	var to string
	var kinds, sprints, allowRisk []string
	var ttl time.Duration
	var revoke, asJSON bool
	cmd := &cobra.Command{
		Use:   "delegate [--to NAME [--kind K]... [--sprint N]... [--allow-risk R]... [--ttl D]] | delegate --revoke",
		Short: "show, set or revoke the agent that answers needs requests on your behalf",
		Long: `delegate designates ONE agent (normally the host steward's agent) that answers
needs requests on your behalf inside a scope: request kinds and sprints (empty
means all). Requests tagged deploy, security, credentials, money, delete or
access always come to you, unless you list the tag with --allow-risk. A
delegate's answers are recorded as made on your behalf; list them with
` + "`approve list --all --on-behalf`" + `, override one with --override, and
revoke the delegation at once with --revoke. Only a human can run this.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			w := c.OutOrStdout()
			var d Delegation
			var err error
			switch {
			case revoke:
				d, err = RevokeDelegation()
			case to != "":
				spec := Delegation{To: to, Kinds: kinds, Sprints: sprints, AllowRisk: allowRisk}
				if ttl > 0 {
					spec.Expires = time.Now().Add(ttl)
				}
				d, err = SetDelegation(spec)
			default:
				var ok bool
				if d, ok = ActiveDelegation(time.Now()); !ok {
					if asJSON {
						return json.NewEncoder(w).Encode(map[string]any{"schema_version": ApproveSchemaVersion, "delegation": nil})
					}
					fmt.Fprintln(w, "no delegate: every needs request comes to you")
					return nil
				}
			}
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(w).Encode(map[string]any{"schema_version": ApproveSchemaVersion, "delegation": d})
			}
			state := "active"
			if !d.Revoked.IsZero() {
				state = "revoked"
			}
			fmt.Fprintf(w, "%s %s\n", state, d.scopeLine())
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "the agent that answers on your behalf")
	cmd.Flags().StringSliceVar(&kinds, "kind", nil, "request kinds it may answer (default all)")
	cmd.Flags().StringSliceVar(&sprints, "sprint", nil, "sprints it may answer for (default all)")
	cmd.Flags().StringSliceVar(&allowRisk, "allow-risk", nil, "non-delegable risk tags you allow it to answer anyway")
	cmd.Flags().DurationVar(&ttl, "ttl", 0, "end the delegation after this long (default: until revoked)")
	cmd.Flags().BoolVar(&revoke, "revoke", false, "revoke the delegation now")
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
