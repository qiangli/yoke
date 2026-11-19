// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package handoff

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	yokegit "github.com/qiangli/yoke/git"
	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
	"github.com/qiangli/yoke/pkg/room"
)

// NewHandoffCmd builds `bashy handoff`: pause a live session and pass the work on.
func NewHandoffCmd() *cobra.Command {
	var (
		brief    string
		next     string
		to       string
		at       string
		park     bool
		asJSON   bool
		blockers []string
		role     string
	)
	cmd := &cobra.Command{
		Use:   "handoff",
		Short: "pause this session and hand the work to another agent, a scheduler, or tomorrow",
		Long: `handoff captures everything a successor needs and passes the work on.

It exists because every agentic tool's own /resume is a prison: Claude Code
resumes from its store, Codex from its own, each proprietary, each on ONE
machine, in ONE tool. bashy is the shell underneath all of them, so it can write
a session record that OUTLIVES the tool that made it.

What is captured:
  - the continuity brief (what you were doing, why, what you learned)
  - the next action, stated so a COLD agent in a DIFFERENT tool can act on it
  - the in-flight WORKING TREE: the diff against HEAD (staged and unstaged
    together) plus untracked files carried by content

That last part is the piece nothing else had. sprint handoff, weave baton and the
session lease all record PROSE — a successor inherited a narrative, not a diff.

Where it goes:
  --to <tool>            hand to another agentic tool NOW, in an isolated weave
                         workspace seeded with your in-flight work. You keep
                         watching: weave status/log, weave say (steer it),
                         weave attach (take the keyboard), weave kill.
  --to schedule --at T   hand to a future wake-up; the brief arrives WITH the job
  (default)              park it. Nobody is named; the work waits, intact, for
                         'bashy resume'. Stopping for the day is a handoff too.

The record is a FILE. It travels — scp it, mesh it, paste it in an issue.`,
		Example: `  # stop for the day; anyone (or any tool) can pick it up
	  bashy handoff -m "refactored the atlas; next: wire the sdlc view" --next "run go test ./..."

  # carry a typed tool result directly; the record remains a portable file
  bashy tool cmd run claude:plan --json -- "plan the migration" | bashy handoff -m - --next "implement the cited plan"

  # hand the work to codex, keep watching from here
  bashy handoff --to codex -m "chunking work; the fixture race is fixed"
  bashy weave status          # where is it?
  bashy weave say --issue 9 "skip the container lane for now"
  bashy weave attach --issue 9

  # wake an agent tomorrow morning with the task in hand
  bashy handoff --to schedule --at 09:00 -m "resume the naming pass"`,
		RunE: func(cmd *cobra.Command, args []string) error {
			if brief == "-" {
				body, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return fmt.Errorf("read handoff brief: %w", err)
				}
				brief = strings.TrimSpace(string(body))
			}
			cwd, err := os.Getwd()
			if err != nil {
				return err
			}
			root, err := repoRoot(cwd)
			if err != nil {
				return fmt.Errorf("handoff needs a git repo: %w", err)
			}

			if strings.TrimSpace(brief) == "" {
				// A handoff with no brief is a trap: the successor gets a diff it
				// cannot interpret. Refuse loudly rather than write a useless record.
				return fmt.Errorf("a handoff needs a brief (-m): the successor may be a " +
					"different tool on a different machine and knows NOTHING about what you were doing")
			}

			work, err := CaptureWork(root)
			if err != nil {
				return err
			}

			self, _ := principal.NewResolver(fleet.New(), principal.DefaultEnv()).Self()
			now := time.Now().UTC()
			rec := &Record{
				ID:         NewID(now, root),
				CreatedAt:  now,
				From:       self,
				Project:    resolveProject(root),
				Continuity: brief,
				NextAction: next,
				Blockers:   blockers,
				Role:       role,
				Work:       work,
				// Every new record declares its transfer metadata, even with
				// zero effects: a replacement must be able to tell "the
				// sidecar is missing" apart from "there never was one".
				Transfer: &TransferDecl{SchemaVersion: TransferSchemaVersion},
			}

			switch {
			case to != "" && to != "schedule":
				// THE ENUM. `--to` accepts only the agent tools and bindings that
				// actually exist on THIS host, and it resolves the word a human says
				// ("codex") to the thing it denotes here ("codex-gpt-5.5").
				//
				// This is what makes "bashy handoff this to codex" unambiguous BY
				// CONSTRUCTION rather than by prompting: in this position "codex"
				// cannot mean a vendor's product, because that is not a legal value.
				// A closed value set grounds a word far harder than any description
				// can -- and unlike a glossary it cannot go stale, because it IS the
				// registry.
				resolved, err := resolveAgent(to)
				if err != nil {
					return err
				}
				rec.Dispatch = Dispatch{Disposition: DispatchAgent, To: resolved}
			case to == "schedule":
				rec.Dispatch = Dispatch{Disposition: DispatchSchedule, To: at}
			default:
				_ = park
				rec.Dispatch = Dispatch{Disposition: DispatchPark}
			}

			path, err := Save(DefaultDir(), rec)
			if err != nil {
				return err
			}
			// The annotation sidecar starts life next to its record so the two
			// travel as a pair (same directory, same scp/commit). It carries no
			// annotations yet — identity, holder and status stay record-derived —
			// but its presence from birth means a replacement never has to guess
			// where the transfer for an exported handoff lives.
			if _, serr := SaveTransfer(DefaultDir(), Summarize(rec)); serr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "handoff: warning: transfer sidecar not written: %v\n", serr)
			}
			recordEvent(rec, "created", refName(self), rec.Continuity)

			// A role (seat) handoff is SINGULAR: retire any prior UNCLAIMED
			// handoff of the SAME role in this project, so a bare `bashy resume`
			// finds exactly one live seat instead of an ambiguous pile.
			if role != "" {
				when := rec.CreatedAt
				if prior, perr := Pending(DefaultDir(), projectRoots(root)); perr == nil {
					for _, p := range prior {
						if p.ID == rec.ID || p.Role != role {
							continue
						}
						p.SupersededAt, p.SupersededBy = &when, rec.ID
						_, _ = Save(DefaultDir(), p)
					}
				}
			}

			if asJSON {
				b, _ := json.MarshalIndent(rec, "", "  ")
				fmt.Fprintln(cmd.OutOrStdout(), string(b))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "handoff: %s\n", rec.ID)
				fmt.Fprintf(cmd.OutOrStdout(), "  record:  %s\n", path)
				fmt.Fprintf(cmd.OutOrStdout(), "  project: %s (%d root(s))\n", rec.Project.Name, len(rec.Project.Roots))
				if work.Clean {
					fmt.Fprintf(cmd.OutOrStdout(), "  work:    clean (nothing in flight)\n")
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "  work:    %d diff line(s), %d untracked file(s) — captured\n",
						strings.Count(work.Diff, "\n"), len(work.Untracked))
				}
				switch rec.Dispatch.Disposition {
				case DispatchPark:
					fmt.Fprintf(cmd.OutOrStdout(), "  next:    parked — resume with `bashy resume` (any tool, any machine)\n")
				case DispatchSchedule:
					fmt.Fprintf(cmd.OutOrStdout(), "  next:    scheduled for %s\n", rec.Dispatch.To)
				case DispatchAgent:
					fmt.Fprintf(cmd.OutOrStdout(), "  next:    handing to %s\n", rec.Dispatch.To)
				}
			}

			// Dispatch is deliberately ADVISORY in v1: we print the exact command
			// rather than spawning behind the user's back. Launching another agent
			// is a spend/exec effect, and a handoff often runs when a session is
			// being killed — the last thing that should happen then is an
			// unattended process the human did not see start.
			switch rec.Dispatch.Disposition {
			case DispatchAgent:
				fmt.Fprintf(cmd.OutOrStdout(), "\nTo hand it over: START %s interactively (you), then have it run `bashy resume --claim`.\n", rec.Dispatch.To)
			case DispatchSchedule:
				fmt.Fprintf(cmd.OutOrStdout(), "\nTo arm the wake-up:\n")
				fmt.Fprintf(cmd.OutOrStdout(), "  bashy schedule add --at %s --prompt \"bashy resume %s\"\n", at, rec.ID)
			}
			if !asJSON {
				fmt.Fprintln(cmd.OutOrStdout(), handoffHelpHint)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&brief, "message", "m", "", "the continuity brief a successor reads first (required; '-' reads stdin)")
	cmd.Flags().StringVar(&next, "next", "", "the one next action, stated for a cold agent in another tool")
	cmd.Flags().StringSliceVar(&blockers, "blocker", nil, "a blocker the successor must know about (repeatable)")
	cmd.Flags().StringVar(&role, "as", "", "hand off a ROLE, not just the task: the skill the successor assumes (e.g. 'steward', 'conductor'). It loads the skill, becomes that role, and decides how to drive — including whether to delegate the work back.")
	cmd.Flags().StringVar(&to, "to", "", "successor: an agent tool (codex, claude, …) or 'schedule'")
	cmd.Flags().StringVar(&at, "at", "", "when, for --to schedule (e.g. 09:00, 30m)")
	cmd.Flags().BoolVar(&park, "park", false, "park the work for anyone to resume (the default)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the record")
	return cmd
}

// NewResumeCmd builds `bashy resume`: the counterpart, and the more important
// half — it runs COLD, in a session that may be a different tool on a different
// machine, and it must work when the agent knows nothing.
func NewResumeCmd() *cobra.Command {
	var (
		list          bool
		asJSON        bool
		show          bool
		message       string
		prune         bool
		all           bool
		claim         bool
		cancel        bool
		transfer      bool
		declareEffect string
		effectDetail  string
		reconcileID   string
		reconcileAs   string
		noEffects     bool
	)
	cmd := &cobra.Command{
		Use:   "resume [id|path]",
		Short: "pick up a handed-off session — any tool, any machine",
		Long: `resume reads a handoff record and continues the work.

It is the counterpart of ` + "`bashy handoff`" + ` and the half that has to be
flawless, because it runs COLD: the agent invoking it has no memory, may be a
DIFFERENT tool than the one that handed off, and may be on a different machine.

With no argument it finds the pending handoff for this project — by path-set
intersection, so a session that handed off across several repos is found from
any one of them.

  bashy resume                 # SHOW the current handoff + whether it is claimed (read-only, idempotent)
  bashy resume --claim         # TAKE it: apply the work and record you hold it (the ONLY side effect)
  bashy resume --claim -m "X"  # take it, with a fresh steer from the human
  bashy resume --all           # every handoff for this project, with status
  bashy resume --cancel <id>   # retire an unclaimed one (kept for the record)
  bashy resume --prune         # delete DONE handoffs (resumed/superseded/cancelled)
  bashy resume --transfer [id] # recovery brief: candidate, evidence, decisions, next safe steps (read-only)
  bashy resume --declare-effect "push image" --detail "docker push ran before the host died" [id]
  bashy resume --reconcile-effect fx-1 --as applied [id]   # only after re-checking the outside world
  bashy resume --no-external-effects [id]  # the work touched nothing outside the tree

Bare 'resume' NEVER claims — run it before and after a pickup to confirm an agent
took the seat. Claiming requires --claim. A role (seat) handoff is singular: a
new one supersedes the prior unclaimed seat, so bare 'resume' shows exactly one
live seat. Task handoffs may be many; if several are current it lists them.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			dir := DefaultDir()
			cwd, _ := os.Getwd()
			root, _ := repoRoot(cwd)
			if root == "" {
				root = cwd
			}

			if prune {
				n, err := Prune(dir)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "resume: pruned %d retired handoff(s) (superseded/cancelled/stale) — live seats kept\n", n)
				return nil
			}

			out := cmd.OutOrStdout()
			roots := projectRoots(root)

			// --cancel <id>: explicitly retire an unclaimed handoff. Kept for the
			// record (only --prune deletes), hidden from the default view.
			if cancel {
				rec, err := pick(dir, root, args)
				if err != nil {
					return err
				}
				// Cancellation and pickup share one critical section: a cancel
				// must not be acknowledged then erased by the pickup stamp.
				g, lock, err := coord.AcquireAttachedRef(cmd.Context(), coord.Request{
					Ref: ClaimRef(rec.ID), Holder: coord.Self(), Mode: coord.ModeAttached,
					Intent: "resume --cancel " + rec.ID,
				})
				if err != nil {
					return err
				}
				defer func() { _ = coord.ReleaseAttached(coord.DefaultDir(), g.Claim, lock) }()
				// The selected snapshot may predate a completed pickup.
				if cur, ok := stored(dir, rec.ID); ok {
					rec = cur
				}
				if rec.ResumedAt != nil {
					return fmt.Errorf("%s is already claimed; nothing to cancel", rec.ID)
				}
				now := time.Now().UTC()
				self, _ := principal.NewResolver(fleet.New(), principal.DefaultEnv()).Self()
				rec.CancelledAt, rec.CancelledBy = &now, &self
				if _, err := Save(dir, rec); err != nil {
					return err
				}
				recordEvent(rec, "cancelled", refName(self), "")
				fmt.Fprintf(out, "cancelled %s (kept for the record; `resume --prune` to delete)\n", rec.ID)
				return nil
			}

			// --declare-effect / --reconcile-effect / --no-external-effects: the
			// agent-usable S5 effect plane. The interrupted worker (or the
			// replacement, before retrying) names each outside effect the
			// work may have caused, then records the OBSERVED outcome after
			// re-checking the outside world. Declaring is how the retry gate
			// learns the scope; reconciling is how it opens. Every mutation
			// re-saves the sidecar AND the record's declaration together, so
			// the pair stays a whole export — a record never again travels
			// with a silently stale scope.
			if declareEffect != "" || reconcileID != "" || noEffects {
				rec, err := pick(dir, root, args)
				if err != nil {
					return err
				}
				// Serialize the entire read/modify/write with pickup, including
				// its final effect check and durable ownership stamp.
				self := coord.Self()
				ref := ClaimRef(rec.ID)
				g, lock, err := coord.AcquireAttachedRef(cmd.Context(), coord.Request{
					Ref: ref, Holder: self, Mode: coord.ModeAttached, Intent: "resume effect mutation " + rec.ID,
				})
				if err != nil {
					return err
				}
				defer func() { _ = coord.ReleaseAttached(coord.DefaultDir(), g.Claim, lock) }()
				// Re-read after acquisition so concurrent declarations cannot
				// overwrite each other or erase a pickup stamp.
				if cur, ok := stored(dir, rec.ID); ok {
					rec = cur
				}
				if reason := retiredReason(rec); reason != "" {
					return retiredErr(rec)
				}
				sdir := transferDir(dir, args)
				side, lerr := LoadTransfer(sdir, rec.ID)
				if lerr != nil {
					if rec.Declared() {
						return fmt.Errorf("handoff %s declares transfer metadata but %s is missing or unreadable: re-export the record WITH its sidecar (same directory, same scp/commit) instead of recording against half an export: %v",
							rec.ID, transferSidecar(sdir, rec.ID), lerr)
					}
					side = &Transfer{HandoffID: rec.ID}
				} else if rec.Declared() {
					if err := checkSidecar(rec, side); err != nil {
						return err
					}
				}
				switch {
				case declareEffect != "":
					fx := side.DeclareEffect(declareEffect, effectDetail)
					if err := persistDecl(dir, sdir, rec, side); err != nil {
						return err
					}
					fmt.Fprintf(out, "declared %s: %s — reconcile it (applied|absent) before the owning step is retried\n", fx.ID, fx.Op)
				case reconcileID != "":
					st := EffectStatus(strings.TrimSpace(reconcileAs))
					self, _ := principal.NewResolver(fleet.New(), principal.DefaultEnv()).Self()
					if err := side.ReconcileEffect(reconcileID, st, refName(self)); err != nil {
						return err
					}
					if _, err := SaveTransfer(sdir, side); err != nil {
						return err
					}
					fmt.Fprintf(out, "reconciled %s as %s\n", reconcileID, st)
				default: // --no-external-effects
					side.NoExternalEffects = true
					if err := persistDecl(dir, sdir, rec, side); err != nil {
						return err
					}
					fmt.Fprintf(out, "recorded: no external effects for %s — the retry gate is open on scope\n", rec.ID)
				}
				return nil
			}

			// --list / --all: the register view. Default lists CURRENT only; --all
			// lists every record with its status.
			if list || all {
				recs, err := List(dir)
				if err != nil {
					return err
				}
				shown := 0
				for _, r := range recs {
					if !(intersects(r.Project.Roots, roots) || intersects([]string{r.Project.Primary}, roots)) {
						continue
					}
					if !all && !r.Live() {
						continue
					}
					fmt.Fprintf(out, "%-13s seat=%-9s owner=%-10s %s  %s\n",
						r.Status(), r.SeatRole(), ownerOf(r), r.ID, firstLine(r.Continuity))
					shown++
				}
				if shown == 0 {
					if all {
						fmt.Fprintln(out, "resume: no handoffs for this project")
					} else {
						fmt.Fprintln(out, "resume: no live handoff (--all shows superseded/cancelled/stale too)")
					}
				} else {
					fmt.Fprintln(out, resumeHelpHint)
				}
				return nil
			}

			// With no id, "resume" means the SEAT: prefer the one live role handoff
			// (steward/conductor) over task handoffs, so `resume` answers "who holds
			// the seat" even when tasks coexist. Tasks are reached by id or --all.
			var rec *Record
			if len(args) == 0 {
				rec = liveSeat(dir, roots)
			}
			if rec == nil {
				var err error
				rec, err = pick(dir, root, args)
				if err != nil {
					// No live unclaimed handoff. If a recent one was CLAIMED, report
					// that — so `resume` after a pickup tells you who holds it.
					if held := mostRecentClaimed(dir, roots); held != nil {
						fmt.Fprintf(out, "STATUS: active — the %s seat is held by %s since %s\n  (%s; no handoff in flight — `bashy resume --all` for the register)\n",
							held.SeatRole(), ownerOf(held), held.ResumedAt.Format(time.RFC3339), held.ID)
						return nil
					}
					return err
				}
			}

			// --transfer: the S5 recovery brief for the replacement —
			// candidate identity, changed files, verification evidence,
			// pending decisions and approvals, cancellations, next safe
			// steps, the effect gate, and the ownership claims. Read-only:
			// it composes the live record with its annotation sidecar and
			// never stamps or applies. Identity and holder always come from
			// the record, so a stale sidecar cannot misdirect the takeover.
			if transfer {
				// A declared transfer whose sidecar is missing or damaged
				// refuses here: rendering a partial brief would hide the
				// uncertain effects the replacement most needs to see.
				tr, err := ComposeChecked(transferDir(dir, args), rec)
				if err != nil {
					return err
				}
				if asJSON {
					b, _ := json.MarshalIndent(tr, "", "  ")
					fmt.Fprintln(out, string(b))
					return nil
				}
				fmt.Fprintln(out, Render(tr, rec))
				return nil
			}

			if show || asJSON {
				b, _ := json.MarshalIndent(rec, "", "  ")
				fmt.Fprintln(out, string(b))
				return nil
			}

			// A claimed record is reported read-only (this is how you confirm a
			// pickup): bare `resume` NEVER stamps or applies.
			if rec.Status() == "active" {
				fmt.Fprintf(out, "STATUS: active — the %s seat is held by %s since %s (%s)\n",
					rec.SeatRole(), ownerOf(rec), rec.ResumedAt.Format(time.RFC3339), rec.ID)
				if claim {
					return fmt.Errorf("already held by %s", ownerOf(rec))
				}
				fmt.Fprintln(out, resumeHelpHint)
				return nil
			}

			// UNCLAIMED — show the brief. Read-only unless --claim.
			fmt.Fprintf(out, "STATUS: %s — the %s seat is UNCLAIMED  (%s, from %s, %s)\n\n",
				rec.Status(), rec.SeatRole(), rec.ID, refName(rec.From), rec.CreatedAt.Format(time.RFC3339))
			if message != "" {
				fmt.Fprintf(out, "── the human says (on pickup) ──\n%s\n\n", strings.TrimSpace(message))
			}
			if rec.Role != "" {
				fmt.Fprintf(out, "── role ──\nAssume the '%s' role: run `bashy skill show %s` and follow it. You are handed the SEAT — decide how to drive (including whether to delegate it back).\n\n", rec.Role, rec.Role)
			}
			fmt.Fprintf(out, "── continuity ──\n%s\n\n", strings.TrimSpace(rec.Continuity))
			if rec.NextAction != "" {
				fmt.Fprintf(out, "── next action ──\n%s\n\n", rec.NextAction)
			}
			for _, b := range rec.Blockers {
				fmt.Fprintf(out, "── blocker ──\n%s\n\n", b)
			}
			if rec.Work.Clean {
				fmt.Fprintln(out, "── work ──\nnothing was in flight; the tree was clean.")
			} else {
				fmt.Fprintf(out, "── work ──\n%d diff line(s), %d untracked file(s) — applied on --claim.\n",
					strings.Count(rec.Work.Diff, "\n"), len(rec.Work.Untracked))
			}

			if !claim {
				fmt.Fprintf(out, "\nSTATUS: unclaimed — this was READ-ONLY. To TAKE it (apply the work and record you hold it): `bashy resume --claim`\n")
				fmt.Fprintln(out, resumeHelpHint)
				return nil
			}

			// --claim: the ONLY side-effecting path — apply the work and stamp,
			// both INSIDE a coord claim on the handoff id (see claim.go). Two
			// successors racing for one parked seat therefore produce one winner
			// and one refusal naming it, instead of two stamps over each other.
			//
			// The S5 retry gate runs BEFORE the take. Declared-but-uncertain
			// external effects refuse the pickup: the replacement reconciles
			// them first (re-check the outside world, mark applied or absent)
			// and retries. An undeclared scope only warns — legacy handoffs
			// carry no effect declaration, and warning (not refusing) keeps
			// them takeable. A declared transfer whose sidecar is missing or
			// damaged refuses here too: the pickup must not proceed having
			// silently dropped uncertain-effect state. A task id, run state
			// or launch receipt never opens the gate; only observed outcomes
			// do (see transfer.go). Retired records (cancelled, superseded)
			// are refused inside Claim.
			tr, err := ComposeChecked(transferDir(dir, args), rec)
			if err != nil {
				return err
			}
			if uncertain := tr.UncertainEffects(); len(uncertain) > 0 {
				names := make([]string, 0, len(uncertain))
				for _, e := range uncertain {
					names = append(names, e.ID+":"+e.Op)
				}
				return fmt.Errorf("pickup refused: %d external effect(s) uncertain — "+
					"reconcile before retry: %s", len(names), strings.Join(names, ", "))
			}
			if len(tr.Effects) == 0 && !tr.NoExternalEffects {
				if rec.Declared() {
					fmt.Fprintf(cmd.ErrOrStderr(), "handoff: warning: external-effect scope was never "+
						"declared — proceeding, but reconcile any outside effects of %s before retrying them\n", rec.ID)
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "handoff: warning: legacy handoff with no declared transfer metadata — "+
						"proceeding, but reconcile any outside effects of %s before retrying them\n", rec.ID)
				}
			}
			//
			// coord.Self() rather than the bare resolver: it MINTS an episode
			// when the ambient session has none, so a human-launched tool is a
			// distinguishable holder of both the claim and the stamp.
			self := coord.Self()
			claimed, err := claimFrom(cmd.Context(), dir, transferDir(dir, args), rec, self, func() error {
				if rec.Work.Clean {
					return nil
				}
				if err := Apply(rec.Work, root); err != nil {
					return err
				}
				fmt.Fprintf(out, "\n── work ──\nrestored into %s\n", root)
				return nil
			})
			if err != nil {
				return err
			}
			recordEvent(claimed, "claimed", refName(*claimed.ResumedBy), claimed.Continuity)
			fmt.Fprintf(out, "\nCLAIMED by %s. Re-run `bashy resume` to confirm the seat is held.\n", refName(*claimed.ResumedBy))
			return nil
		},
	}
	cmd.Flags().BoolVar(&claim, "claim", false, "TAKE the handoff: apply its work and record that you hold it. This is the ONLY side-effecting form — bare `resume` is read-only.")
	cmd.Flags().BoolVar(&all, "all", false, "list ALL handoffs for this project with their status (current/resumed/superseded/cancelled/stale)")
	cmd.Flags().BoolVar(&cancel, "cancel", false, "retire an unclaimed handoff by id — kept for the record, hidden from the default view")
	cmd.Flags().StringVarP(&message, "message", "m", "", "an instruction from the human at pickup, shown to the successor (use with --claim)")
	cmd.Flags().BoolVar(&list, "list", false, "list CURRENT handoffs (one line each)")
	cmd.Flags().BoolVar(&prune, "prune", false, "delete DONE handoffs (resumed, superseded, or cancelled), leaving live ones")
	cmd.Flags().BoolVar(&show, "show", false, "print the record as JSON (read-only)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit the record as JSON")
	cmd.Flags().BoolVar(&transfer, "transfer", false, "print the S5 recovery transfer for the replacement: candidate identity, changed files, evidence, pending decisions, cancellations, next safe steps, effect gate, ownership (read-only; --json emits the transfer)")
	cmd.Flags().StringVar(&declareEffect, "declare-effect", "", "record one outside effect the work may have caused (an op name), with optional --detail; the effect starts uncertain and gates the retry until reconciled")
	cmd.Flags().StringVar(&effectDetail, "detail", "", "detail for --declare-effect: what ran, where, before what interruption")
	cmd.Flags().StringVar(&reconcileID, "reconcile-effect", "", "record the observed outcome of one declared effect (its fx-N id) after re-checking the outside world; requires --as applied|absent")
	cmd.Flags().StringVar(&reconcileAs, "as", "", "observed outcome for --reconcile-effect: applied or absent (only these two open the gate)")
	cmd.Flags().BoolVar(&noEffects, "no-external-effects", false, "explicitly declare that the interrupted work touched nothing outside the candidate tree, opening the retry gate on scope")
	return cmd
}

// mostRecentClaimed returns the newest handoff for the project that has been
// claimed (resumed) and not since superseded/cancelled — so `bashy resume`, run
// after a pickup, can report who now holds the seat instead of just "none".
func mostRecentClaimed(dir string, roots []string) *Record {
	recs, err := List(dir)
	if err != nil {
		return nil
	}
	var best *Record
	for _, r := range recs {
		if r.ResumedAt == nil || r.SupersededAt != nil || r.CancelledAt != nil {
			continue
		}
		if !(intersects(r.Project.Roots, roots) || intersects([]string{r.Project.Primary}, roots)) {
			continue
		}
		if best == nil || r.ResumedAt.After(*best.ResumedAt) {
			best = r
		}
	}
	return best
}

// liveSeat returns the newest live role (seat) handoff for the project — one
// that is transferring (parked, awaiting claim) or active (held). It is what a
// bare `resume` resolves to, so "resume" answers the seat question directly even
// when task handoffs coexist. Nil if there is no live seat.
func liveSeat(dir string, roots []string) *Record {
	recs, err := List(dir)
	if err != nil {
		return nil
	}
	var best *Record
	for _, r := range recs {
		if r.Role == "" {
			continue
		}
		if s := r.Status(); s != "transferring" && s != "active" {
			continue
		}
		if !(intersects(r.Project.Roots, roots) || intersects([]string{r.Project.Primary}, roots)) {
			continue
		}
		if best == nil || r.CreatedAt.After(best.CreatedAt) {
			best = r
		}
	}
	return best
}

// resumeHelpHint / handoffHelpHint are the discoverability footers printed under
// each command's output so a reader always knows how to reach the rest of the
// flags — and how the two halves fit together — without guessing.
const resumeHelpHint = "→ `bashy resume --help` for all options (--claim take it · --all register · --cancel · --prune)"
const handoffHelpHint = "→ `bashy handoff --help` for options (--as <role> seat · --next · --blocker). The successor picks it up with `bashy resume` (look) then `bashy resume --claim` (take)."

// ownerOf names who holds a record: the claimant's grounded identity for an
// active one (stamped from who ran --claim — not free text, so it cannot be
// faked or forgotten), else "—".
func ownerOf(r *Record) string {
	if r.Status() == "active" && r.ResumedBy != nil {
		return refName(*r.ResumedBy)
	}
	return "—"
}

// declRecordSave and declSidecarSave are the two halves of a declaration
// write, split for fault injection in tests. Production code always calls
// them through persistDecl, which orders them crash-safe (see below).
var declRecordSave = func(dir string, rec *Record) (string, error) { return Save(dir, rec) }
var declSidecarSave = func(dir string, t *Transfer) (string, error) { return SaveTransfer(dir, t) }

// persistDecl writes the Record declaration FIRST, then its sidecar. A crash
// or failure between the two writes must never leave a declared effect behind
// in a sidecar the record does not name (silently dropped on a record-only
// export, resuming as legacy): it leaves a declared record whose sidecar is
// missing or mismatched, and ComposeChecked refuses that pair instead of
// resuming without the uncertain-effect state.
func persistDecl(recordDir, sidecarDir string, rec *Record, side *Transfer) error {
	rec.Transfer = side.Decl()
	if _, err := declRecordSave(recordDir, rec); err != nil {
		return err
	}
	if _, err := declSidecarSave(sidecarDir, side); err != nil {
		return err
	}
	return nil
}

// transferDir resolves where the transfer sidecar for a picked record lives:
// next to the record for one that arrived by path (scp/mesh attachment),
// in the store otherwise. The sidecar travels WITH an exported handoff —
// same directory, same scp — so a replacement reading a record from a path
// finds its annotations beside it rather than in a store it cannot see.
func transferDir(dir string, args []string) string {
	if len(args) == 1 && (strings.ContainsRune(args[0], filepath.Separator) || strings.HasSuffix(args[0], ".json")) {
		if abs, err := filepath.Abs(args[0]); err == nil {
			return filepath.Dir(abs)
		}
	}
	return dir
}

func pick(dir, root string, args []string) (*Record, error) {
	if len(args) == 1 {
		a := args[0]
		if strings.HasSuffix(a, ".json") || strings.ContainsRune(a, filepath.Separator) {
			return Load(a) // arrived by scp / mesh: a path, not an id
		}
		return Load(filepath.Join(dir, a+".json"))
	}
	recs, err := Pending(dir, projectRoots(root))
	if err != nil {
		return nil, err
	}
	switch len(recs) {
	case 0:
		return nil, fmt.Errorf("no pending handoff for this project (%s). "+
			"`bashy resume --list` shows what is pending; pass a path for a record that arrived from elsewhere", root)
	case 1:
		return recs[0], nil
	default:
		var b strings.Builder
		fmt.Fprintf(&b, "%d pending handoffs — name one:\n", len(recs))
		for _, r := range recs {
			fmt.Fprintf(&b, "  %s  %s\n", r.ID, firstLine(r.Continuity))
		}
		return nil, fmt.Errorf("%s", b.String())
	}
}

// resolveProject is the interim project resolver: the repo plus its go.mod
// sibling replaces. It is deliberately the SAME inference weave already does, so
// the two agree about what "this project" means. The shared, multi-ecosystem
// resolver (submodules, go.work, workspaces) is tracked separately; when it
// lands, both call it.
func resolveProject(root string) Project {
	roots := projectRoots(root)
	inferred := []string{"git-root"}
	if len(roots) > 1 {
		inferred = append(inferred, "go.mod-replace")
	}
	return Project{
		Name:     filepath.Base(root),
		Primary:  root,
		Roots:    roots,
		Inferred: inferred,
	}
}

// ProjectRoots resolves the project as a PATH SET: the repo plus the sibling
// repos it actually depends on. Exported because discovery needs it — `bashy
// context --json` asks "is there a handoff pending for this project?", and the
// honest answer requires knowing that a project spans repos. A session that
// handed off while working across bashy + sh + coreutils must be found by an
// agent that later opens ANY ONE of them.
func ProjectRoots(root string) []string { return projectRoots(root) }

func projectRoots(root string) []string {
	return RepoMembers(root)
}

func repoRoot(dir string) (string, error) {
	// One door (sprint 252 S252.6): --show-toplevel is natively routed.
	out, err := yokegit.RunChecked(context.Background(), dir, []string{"rev-parse", "--show-toplevel"})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func refName(r principal.Ref) string {
	if r.Name != "" {
		return r.Name
	}
	if r.Kind != "" {
		return string(r.Kind)
	}
	return "unattributed"
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 72 {
		s = s[:69] + "…"
	}
	return s
}

// resolveAgent turns the word a human SAYS into the binding it denotes on this
// host, and refuses anything that denotes nothing.
//
// It accepts a tool name ("codex" -> its binding here), a binding name
// ("codex-gpt-5.5"), or an alias. On failure it prints the legal values, because a
// closed set is only useful if the caller can see it -- an agent that is told
// "invalid" without being told "valid: ..." will guess, and guessing is the failure
// this exists to prevent.
func resolveAgent(name string) (string, error) {
	cat := fleet.New()
	agents, _ := cat.Agents()

	lower := strings.ToLower(strings.TrimSpace(name))
	var legal []string
	seen := map[string]bool{}

	// A binding, named exactly (or by alias).
	for _, a := range agents {
		if strings.EqualFold(a.Name, lower) {
			return a.Name, nil
		}
		for _, al := range a.Aliases {
			if strings.EqualFold(al, lower) {
				return a.Name, nil
			}
		}
	}
	// A TOOL -- the word people actually say. Resolve to its binding here.
	for _, a := range agents {
		if strings.EqualFold(a.Tool, lower) {
			return a.Name, nil
		}
		if !seen[a.Tool] {
			seen[a.Tool] = true
			legal = append(legal, a.Tool)
		}
	}
	sort.Strings(legal)
	return "", fmt.Errorf("%q is not an agent on this host.\n\nValid: %s (or a binding: try `bashy agent list`).\n"+
		"On this machine those words name a CLI plus the model bound to it -- not a vendor's product.",
		name, strings.Join(legal, ", "))
}

// recordEvent puts a handoff transition on the host event log (bashy events).
// Best effort: the record is already saved, which is the transaction. The
// brief is redacted here — the timeline is streamed to session members, so a
// vault value must not ride it even when the caller built the brief from
// text Save never saw.
func recordEvent(rec *Record, verb, actor, brief string) {
	room.Record(room.EventHandoff, actor, "handoff:"+rec.ID, verb, redactText(brief))
}
