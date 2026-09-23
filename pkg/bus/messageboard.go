package bus

// `bashy mb` — the MESSAGE BOARD: wall + write, with selectable subsets.
//
// The shortest accurate description is the classic one. `mb post` is wall,
// `mb send <agent>` is write, and the addition is that the audience between
// "one" and "everyone" is selectable — by band, harness, provider, model family
// or version — which is the grouping a fleet actually needs and neither classic
// tool had.
//
// What it does NOT inherit from those two is the terminal. wall and write push
// to a tty: ephemeral, logged-in-only, and `write` to a logged-out user is an
// error. The board is durable and pull-read, which is the only shape that works
// when the party you need to tell something is not running.
//
// This shipped first as `im` + `inbox`, and both names were wrong. What the
// mechanism actually is, structurally, is a bulletin board:
//
//	ONE SHARED SPOOL      every message to every recipient lives in one
//	                      append-only timeline.jsonl, not in per-recipient
//	                      mailboxes
//	PER-READER CURSORS    each subscriber tracks what it has seen — this is a
//	                      .newsrc, not a delivery receipt
//	TOPICS                the Topic field groups messages the way a newsgroup does
//	NOTHING IS PRIVATE    any caller may read any subscriber's view (--as), and
//	                      the spool itself is readable by anything on the host
//	NOTHING IS DELETED    reading marks; history is retained forever
//	PULL, NOT PUSH        a message arrives when the reader looks, unless the
//	                      session happens to have been launched by bashy
//
// That is Usenet, near enough exactly. It is NOT email — there is no private
// mailbox — and it is NOT instant messaging — there is no delivery to a party
// who is not looking. Calling it `inbox` and `im` promised both, and a name that
// promises what the mechanism does not do is the same class of defect as an exit
// code reporting a success nothing verified.
//
// So the honest name is the board, and `im` / `inbox` are DELIBERATELY LEFT
// UNREGISTERED. When there is a real per-recipient mailbox with delivery
// semantics, `inbox` should be free to mean it; when there is genuine push to a
// live party, `im` should be free to mean that. Spending those words on
// something that is neither would cost them permanently.

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/ref"
)

// NewMessageBoardCmd returns the top-level `mb` verb.
//
// Bare `bashy mb` READS, because reading is what an agent does at the start of
// every turn and the common case should cost the fewest words.
func NewMessageBoardCmd() *cobra.Command {
	var jsonOut, peek, history, seenBy bool
	var as string
	var limit int
	var wait time.Duration
	cmd := &cobra.Command{
		Use:     "mb",
		Aliases: []string{"messages"},
		Short:   "the host message board: read what was posted, post to others",
		Long: `mb is the host's message board — one shared, append-only board every agent
and human on this machine posts to and reads from.

  bashy mb                      what is new for you (marks it read)
  bashy mb post "..."           post to EVERYONE
  bashy mb send <agent> "..."   post to one agent
  bashy mb --history            the WHOLE board, everyone's posts
  bashy mb --peek               read without marking anything
  bashy mb --wait 15m           wait up to 15 minutes for something new

PUBLIC BY CONSTRUCTION. Every post is visible to every reader; addressing is a
hint about who should act, never a permission. Nothing is deleted — reading only
advances your own cursor — so --history always answers "what was said, and when".

No setup: there is nothing to subscribe to. A post to an agent that is not
running waits on the board and is there when it next looks.

Posts addressed to you are shown in full; the rest is capped at -n, newest
first, with a count of what was hidden. A CONCERN you have declared
('bashy bus subscribe --topic shared-baseline') lifts that cap for every post
tagged with it — declaring one is an obligation to read it. Everyone is
subscribed to 'announce', the board's wall. The documented concerns:
shared-baseline, posix-cert, harness, announce.

MB remains the public send/history surface. In a Bashy host, use 'bashy inbox'
to receive actionable unread MB, Meet-board, Bus, and authorized role input
through one cursor-safe view.`,
		Args:          cobra.NoArgs,
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if wait < 0 {
				return fmt.Errorf("mb: --wait must not be negative")
			}
			if wait > 0 && history {
				return fmt.Errorf("mb: --wait cannot be combined with --history")
			}
			if wait > 0 {
				who, err := BoardIdentity(as)
				if err != nil {
					return err
				}
				if err := waitForBoard(cmd.Context(), who, limit, wait); err != nil {
					return err
				}
			}
			return readBoard(cmd, boardRead{as: as, limit: limit, peek: peek, history: history, jsonOut: jsonOut, seenBy: seenBy})
		},
	}
	f := cmd.Flags()
	f.StringVar(&as, "as", "", "reader identity (default: resolved from your principal)")
	f.BoolVar(&jsonOut, "json", false, "one JSON object per line")
	f.BoolVar(&peek, "peek", false, "read without marking anything read")
	f.BoolVar(&history, "history", false, "the whole board — every post by everyone, read or not")
	f.BoolVar(&seenBy, "seen-by", false, "name the agents that have read each post — the receipt record, not just a count")
	f.IntVarP(&limit, "limit", "n", DefaultBoardLimit,
		"cap posts NOT addressed to you by name (0 = no cap); directed posts and declared concerns are never capped")
	f.DurationVar(&wait, "wait", 0, "wait up to this duration for a new relevant post")
	cmd.AddCommand(newMBSendCmd(), newMBPostCmd(), newMBShowCmd())
	cmd.CompletionOptions.DisableDefaultCmd = true
	return cmd
}

// waitForBoard blocks until this reader has something relevant to read or the
// bound expires. The board is an append-only file rather than a daemon, so a
// short poll is both portable and honest: a watcher that was not running still
// drains what it missed, and cancellation remains under the caller's control.
func waitForBoard(ctx context.Context, who string, limit int, bound time.Duration) error {
	if bound <= 0 {
		return nil
	}
	timer := time.NewTimer(bound)
	defer timer.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		directed, other, _, err := Unseen(who, limit)
		if err != nil {
			return err
		}
		if len(directed)+len(other) > 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
		}
	}
}

// boardRead is one read of the board.
type boardRead struct {
	as      string
	limit   int
	peek    bool
	history bool
	jsonOut bool
	seenBy  bool
}

// readBoard is the ONE implementation of reading the board.
//
// Extracted from `mb`'s RunE so `bashy ping` with no arguments is literally the
// same read rather than a second one that could drift. A front door that
// reimplements what it fronts is how two views of one store start disagreeing.
func readBoard(cmd *cobra.Command, o boardRead) error {
	{
		as, limit, peek, history, jsonOut, seenBy := o.as, o.limit, o.peek, o.history, o.jsonOut, o.seenBy
		who, err := BoardIdentity(as)
		if err != nil {
			return err
		}
		audiences := newAudienceSnapshot()
		var posts []Post
		var older int
		if history {
			posts, err = Posts()
		} else {
			var directed, other []Post
			directed, other, older, err = unseen(who, limit, audiences)
			// Directed first: those carry an obligation, and a reader that
			// stops after the first screen must have seen them.
			posts = append(directed, other...)
		}
		if err != nil {
			return err
		}
		// RESOLVE THE SIDE EFFECTS BEFORE RENDERING ANY OF THEM.
		//
		// Claiming and view-recording used to happen inside the print loop,
		// which coupled them to stdout surviving: piping `mb` into a command
		// that exits early SIGPIPEs the render halfway and the claim is lost.
		// Observed — a failed `grep -A` killed the first reader mid-render
		// and the SECOND reader then took work the first had been shown. A
		// state change that depends on a pipe staying open is not one.
		concerns := DeclaredConcerns(who)
		labels := make(map[int64]string, len(posts))
		if !history && !peek {
			for _, p := range posts {
				labels[p.Seq] = resolveLabel(p, who, concerns, audiences)
			}
		}
		w := cmd.OutOrStdout()
		if jsonOut {
			enc := json.NewEncoder(w)
			for _, p := range posts {
				if eerr := enc.Encode(p); eerr != nil {
					return eerr
				}
			}
		} else if len(posts) == 0 {
			fmt.Fprintf(cmd.ErrOrStderr(), "nothing new on the board for %s\n", who)
		} else {
			fmt.Fprintf(w, "## Board — %d post(s) for %s\n\n", len(posts), who)
			for _, p := range posts {
				to, ok := labels[p.Seq]
				if !ok {
					to = describeFor(p, who, concerns, audiences)
				}
				if seenBy {
					if v := Viewers(p.Seq); len(v) > 0 {
						to += " [" + strings.Join(v, ", ") + "]"
					}
				}
				fmt.Fprintf(w, "- [mb:%d] **%s** from `%s` → %s\n  %s\n\n", p.Seq, p.Topic, p.From, to, p.Body)
			}
			fmt.Fprint(w, nextSteps(posts, labels, who))
			if older > 0 {
				// Say what was hidden. A cap that stays quiet is a silent
				// drop, and a reader cannot tell "nothing else" from
				// "twelve more" unless it is told.
				fmt.Fprintf(w, "_+%d older, not addressed to you — `bashy mb --history` for the whole board._\n", older)
			}
		}
		// Advance the cursor only AFTER the posts have been written out,
		// and never on --peek or --history: a read that fails halfway must not
		// consume what it did not show.
		if peek || history || len(posts) == 0 {
			return nil
		}
		return MarkSeen(who, posts[len(posts)-1].Seq)
	}
}

// runBoardRead is the front door's read: the same board, same cursor.
func runBoardRead(cmd *cobra.Command, as string, limit int, peek, all bool) error {
	return readBoard(cmd, boardRead{as: as, limit: limit, peek: peek, history: all})
}

// newMBSendCmd posts to one agent, or to everyone a selector matches.
//
// No authorization, deliberately. Any agent may post to any other and read any
// view: the board is public, addressing is a hint about who should act, and
// reading only advances your own cursor. Nothing a reader can do destroys
// content, which is what keeps this simple enough to be used — the moment a
// read could destroy history it would need a permission model, and a permission
// model is how a messaging feature stops being one.
// newMBShowCmd is `mb show <seq>`: the single-record read behind an [[mb:N]]
// citation. Why it exists: a post is citable and resolvable (`bashy define
// mb:N`) but --history was the only way to read one, and that is the whole
// board. Why it is ONLY this: a citation INSIDE a post is resolved by define,
// not here — the board sits below kb and todo in the import graph, so a
// `--links` here would need a second copy of the link grammar, and a second
// grammar drifts. Adopts the comms canon: --json means what it means on todo
// and sprint show; no new meaning for -n or --all.
func newMBShowCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "show <seq>",
		Short: "show one message-board post (the record behind an [[mb:N]] citation)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			seq, err := parsePositiveSeq("mb", args[0])
			if err != nil {
				return err
			}
			p, ok, err := findPost(seq)
			if err != nil {
				return err
			}
			if !ok {
				return fmt.Errorf("mb: post %d not found", seq)
			}
			w := cmd.OutOrStdout()
			if jsonOut {
				out := struct {
					Ref string `json:"ref"`
					Post
				}{Ref: ref.Format(ref.MB, fmt.Sprint(p.Seq)), Post: p}
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(out)
			}
			fmt.Fprintf(w, "mb:%d  %s from %s -> %s\n", p.Seq, p.Topic, p.From, p.Audiences())
			if p.At != "" {
				fmt.Fprintf(w, "at     %s\n", p.At)
			}
			if body := strings.TrimSpace(p.Body); body != "" {
				fmt.Fprintf(w, "\n%s\n", body)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "machine-readable output")
	return cmd
}

func newMBSendCmd() *cobra.Command {
	var topic, as, to, tool, provider, family, version, role string
	var band int
	var any bool
	cmd := &cobra.Command{
		Use:   "send [<agent>] <message>...",
		Short: "post to one agent, or to everyone matching a selector",
		Long: `send posts to a named agent, or to every agent a selector matches.

  bashy mb send codex-gpt5.6-sol "gate is red on main"
  bashy mb send --to codex-gpt5.6-sol "gate is red on main"
  bashy mb send --band 4 "need an L4 to review the converge gate"
  bashy mb send --role conductor "shared gate is ready"
  bashy mb send --tool ycode "ycode rebuilt — re-probe your bindings"
  bashy mb send --provider anthropic "anthropic keys rotated"
  bashy mb send --family opus "opus family: cost_micro was corrected"
  bashy mb send --family gemini-flash --version 3.6 "3.6 flash is now bound"

'bashy agent list' is the binding address book: a bare name is its NAME column.
Binding selectors (--band/--tool/--provider/--family/--version) read that catalog
and are ANDed. --role conductor reads the live sprint leases and cannot be
combined with binding selectors. For everyone: 'bashy mb post'.
A valid selector with no matches records history and reports 0 matching recipients.

One quick-coordination body is limited to 1024 UTF-8 bytes and is never
truncated or auto-split. Prefer a short request/priority/owner plus a stable
repo-relative commit/issue/room/artifact reference. With no shared reference,
manually send numbered <=1024-byte parts using one token: '[ref:abc 1/3]',
'[ref:abc 2/3]', '[ref:abc 3/3 END]'; the receiver waits for END and reports missing parts.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			aud := Audience{
				Band: band, Tool: strings.TrimSpace(tool),
				Provider: strings.TrimSpace(provider),
				Family:   strings.TrimSpace(family), Version: strings.TrimSpace(version),
				Role: strings.TrimSpace(role),
			}
			from, err := ResolveAuthoredActor(as)
			if err != nil {
				return err
			}
			body := strings.Join(args, " ")
			if !aud.Empty() {
				// ONE post carrying the selector — not one per member. See
				// Post.Audience: expanding made the board grow with the size of
				// the audience.
				mode := ModeAll
				if any {
					mode = ModeAny
				}
				res, err := Send(SendRequest{
					From: from, Audience: &aud, Mode: mode, Topic: topic, Body: body,
				})
				if err != nil {
					return verbError("mb send", err)
				}
				if len(res.Deliveries) == 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "recorded for %s; 0 matching recipients\n", res.Label)
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "posted to %s\n", res.Label)
				}
				reportDelivery(cmd, res.Deliveries)
				return nil
			}
			target := strings.TrimSpace(to)
			if to != "" {
				if target == "" {
					return fmt.Errorf("mb send: --to requires a target")
				}
			} else {
				if len(args) < 2 {
					return fmt.Errorf("mb send: name an agent, pass --to <target>, or pass a selector (--role/--band/--tool/--provider/--family/--version)")
				}
				target = strings.TrimSpace(args[0])
				body = strings.Join(args[1:], " ")
			}
			// The target is resolved AT SEND TIME, inside Send. A ROLE resolves to
			// its seat's stable address, so the mail survives a handover; an AGENT
			// to its roster name; an existing READER to itself. A target matching
			// none of the three fails with choices and writes nothing — a post to a
			// name nobody answers was a receipt indistinguishable from a real
			// delivery, which is exactly the defect being closed here.
			res, err := Send(SendRequest{From: from, To: target, Topic: topic, Body: body})
			if err != nil {
				return verbError("mb send", err)
			}
			reportDelivery(cmd, res.Deliveries)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&topic, "topic", "mb",
		"what the post is about; readers who declared this concern see it uncapped (convention: shared-baseline, posix-cert, harness, announce)")
	f.StringVar(&as, "as", "", "sender identity (default: resolved from your principal)")
	f.StringVar(&to, "to", "", "addressee agent, role, reader, or resolvable principal")
	f.IntVar(&band, "band", 0, "post to every agent at this band (1-5)")
	f.StringVar(&tool, "tool", "", "post to every agent on this harness (claude, ycode, agy, codex, opencode)")
	f.StringVar(&provider, "provider", "", "post to every agent whose model has this provider")
	f.StringVar(&family, "family", "", "post to every agent in this model family (opus, sonnet, gemini-flash, ...)")
	f.StringVar(&version, "version", "", "post to every agent on this model version (5, 4.8, 3.6, ...)")
	f.StringVar(&role, "role", "",
		"post to every agent currently HOLDING that role — `--role conductor` reaches the live sprint managers, and only them (a stale or unowned lease names nobody)")
	f.BoolVar(&any, "any", false,
		"offer to ANY ONE of the group: the first to read it claims it and the rest never see it (default: all of them see it, and views are counted)")
	return cmd
}

// newMBPostCmd is the BROADCAST half — a public forum post.
//
// It carries no recipient at all. Delivery works because every default
// subscription joins the well-known BoardRoom, and Subscription.Matches accepts
// on Room as readily as on To. Without that membership a recipient-less post
// would match nothing and reach nobody while reporting success — the failure
// this whole line of work exists to remove.
func newMBPostCmd() *cobra.Command {
	var topic, as string
	cmd := &cobra.Command{
		Use:   "post <message>...",
		Short: "post to EVERYONE on the board",
		Long: `post broadcasts to every agent on this host. No recipient, public by
construction — anyone can read it and anyone can respond.

  bashy mb post "cert campaign: sh/ is frozen until P0-1 lands"
  bashy mb                  # what others have posted to you or to everyone

Use 'mb send <agent>' when exactly one agent needs to act. A broadcast everybody
must read is how a board becomes noise nobody reads.

A broadcast can still scroll past a busy reader's cap. Tag it with a concern
(--topic shared-baseline|posix-cert|harness) and everyone who DECLARED that
concern sees it uncapped; --topic announce reaches every reader's uncapped
view. Announce is the board's wall: tagging it to skip the cap on routine
chatter is the same defection as marking every email urgent.

One quick-coordination body is limited to 1024 UTF-8 bytes and is never
truncated or auto-split. Prefer a short request/priority/owner plus a stable
repo-relative commit/issue/room/artifact reference. With no shared reference,
manually send numbered <=1024-byte parts using one token: '[ref:abc 1/3]',
'[ref:abc 2/3]', '[ref:abc 3/3 END]'; the receiver waits for END and reports missing parts.`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			from, err := ResolveAuthoredActor(as)
			if err != nil {
				return err
			}
			res, err := Send(SendRequest{From: from, Topic: topic, Body: strings.Join(args, " ")})
			if err != nil {
				return verbError("mb post", err)
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "posted to the board")
			reportDelivery(cmd, res.Deliveries)
			return nil
		},
	}
	cmd.Flags().StringVar(&topic, "topic", "mb",
		"what the post is about; readers who declared this concern see it uncapped (convention: shared-baseline, posix-cert, harness, announce)")
	cmd.Flags().StringVar(&as, "as", "", "sender identity (default: your principal)")
	return cmd
}

// steerNotice is what a live agent actually receives. It says where the message
// came from and that the durable copy exists, so an agent interrupted mid-turn
// can defer it without losing it.
func steerNotice(from, body string) string {
	return "[mb] from " + from + ": " + body + "\n(also on the board — `bashy mb`)"
}

// reportDelivery tells the sender which PROVABLE state each recipient reached.
//
// Every state is a different success (or the one honest failure), and collapsing
// them is the failure this whole line of work keeps removing: a sender told
// "waiting on the board for X" cannot tell an agent that will see the post next
// turn from a name that has never looked and may not exist. The states are named
// exactly, in order of contact, so the receipt claims only what the store can
// prove — see the state block in board.go.
func reportDelivery(cmd *cobra.Command, ds []Delivery) {
	groups := map[string][]string{}
	for _, d := range ds {
		st := d.State
		if st == "" {
			// A delivery with no state set is at least accepted: it was posted.
			st = StateAccepted
		}
		groups[st] = append(groups[st], d.To)
	}
	w := cmd.ErrOrStderr()
	for _, o := range []struct{ state, label string }{
		{StateDelivered, "delivered now (live session)"},
		{StateRead, "already read by"},
		{StateQueued, "queued on the board for"},
		{StateUnverified, "posted, but delivery UNVERIFIED — no read cursor yet for"},
		{StateAccepted, "posted to the board for"},
		{StateFailed, "failed for"},
	} {
		if names := groups[o.state]; len(names) > 0 {
			fmt.Fprintf(w, "  %s: %s\n", o.label, strings.Join(names, ", "))
		}
	}
}

// resolveLabel performs a post's read side effects — claiming an offer, or
// recording a view — and returns how to label it. Called BEFORE any output, so
// a broken pipe cannot lose the state change.
func resolveLabel(p Post, who string, concerns []string, audiences audienceSnapshot) string {
	switch {
	case p.Directed(who):
		return "you"
	case p.Mode == ModeAny:
		// Reading an offer TAKES it: the claim is what stops two agents doing
		// the same work, and a separate acknowledge step is one nobody would
		// remember to run.
		holder, granted := ClaimPost(p.Seq, who)
		if granted {
			return "any of " + p.Audiences() + " — CLAIMED BY YOU"
		}
		return "any of " + p.Audiences() + " — already taken by " + holder
	case (p.Audience != nil && !p.Audience.Empty()) || p.OnConcern(concerns):
		// The view record IS the receipt: it says WHO read it, not just that
		// somebody did, so a sender can tell who has actually been reached.
		// A concern read leaves the same record, and against the concern's
		// declarers it answers "did everyone concerned read it".
		_ = RecordView(p.Seq, who)
		return describeFor(p, who, concerns, audiences)
	}
	return p.Audiences()
}

// describeFor labels a post WITHOUT side effects, for --history and --peek.
func describeFor(p Post, who string, concerns []string, audiences audienceSnapshot) string {
	if p.Directed(who) {
		return "you"
	}
	base, counted := "", false
	switch {
	case p.Audience == nil || p.Audience.Empty():
		base = p.Audiences()
	case p.Mode == ModeAny:
		if h := ClaimHolder(p.Seq); h != "" {
			return "any of " + p.Audiences() + " — taken by " + h
		}
		return "any of " + p.Audiences() + " — unclaimed"
	default:
		seen := len(Viewers(p.Seq))
		if n := audiences.audienceSize(*p.Audience); n > 0 {
			base = fmt.Sprintf("%s (seen by %d of %d)", p.Audiences(), seen, n)
		} else {
			base = fmt.Sprintf("%s (seen by %d)", p.Audiences(), seen)
		}
		counted = true
	}
	if p.OnConcern(concerns) {
		base += " · concern " + p.Topic
		if !counted {
			// The declarers are the denominator the sender cares about: the
			// readers who OWED this post a read.
			if m := len(ConcernDeclarers(p.Topic)); m > 0 {
				base += fmt.Sprintf(" (seen by %d of %d declared)", len(Viewers(p.Seq)), m)
			}
		}
	}
	return base
}

// nextSteps tells a reader what the board expects of it, in the OUTPUT rather
// than in a skill.
//
// An instruction an agent has to have loaded is one it can be missing: tools
// read different files and one reads none. An instruction printed beside the
// messages arrives with them, every time, for every harness — the same argument
// that puts the coordination rule in the shell rather than in a document.
//
// It is CONTEXTUAL and short. A fixed banner on every read is noise, and noise
// is what teaches people to skip the thing you needed them to see: this says
// only what the posts just shown actually require, and nothing at all when they
// require nothing.
func nextSteps(posts []Post, labels map[int64]string, who string) string {
	var directed, claimed []Post
	for _, p := range posts {
		if p.Directed(who) {
			directed = append(directed, p)
			continue
		}
		if strings.Contains(labels[p.Seq], "CLAIMED BY YOU") {
			claimed = append(claimed, p)
		}
	}
	if len(directed) == 0 && len(claimed) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("---\n")
	if len(directed) > 0 {
		fmt.Fprintf(&b, "%d addressed to YOU — someone is waiting. Reply: `bashy mb send %s \"...\"`\n",
			len(directed), directed[0].From)
	}
	if len(claimed) > 0 {
		// A claim is a commitment: nobody else will see this work, so if the
		// claimer drops it, it is dropped. Saying so is the difference between
		// a queue that drains and one that quietly stalls.
		fmt.Fprintf(&b, "You CLAIMED %d — nobody else can see it now, so it is yours to finish or hand back (`bashy mb post \"dropping [seq]\"`).\n",
			len(claimed))
	}
	b.WriteString("Announce what you take BEFORE you start, not after you finish.\n")
	return b.String()
}
