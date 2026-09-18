// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package handoff

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"
	"github.com/qiangli/yoke/pkg/principal"
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

  # hand the work to codex, keep watching from here
  bashy handoff --to codex -m "chunking work; the fixture race is fixed"
  bashy weave status          # where is it?
  bashy weave say --issue 9 "skip the container lane for now"
  bashy weave attach --issue 9

  # wake an agent tomorrow morning with the task in hand
  bashy handoff --to schedule --at 09:00 -m "resume the naming pass"`,
		RunE: func(cmd *cobra.Command, args []string) error {
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
	cmd.Flags().StringVarP(&brief, "message", "m", "", "the continuity brief a successor reads first (required)")
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
		list    bool
		asJSON  bool
		show    bool
		message string
		prune   bool
		all     bool
		claim   bool
		cancel  bool
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
				if rec.ResumedAt != nil {
					return fmt.Errorf("%s is already claimed; nothing to cancel", rec.ID)
				}
				now := time.Now().UTC()
				self, _ := principal.NewResolver(fleet.New(), principal.DefaultEnv()).Self()
				rec.CancelledAt, rec.CancelledBy = &now, &self
				if _, err := Save(dir, rec); err != nil {
					return err
				}
				fmt.Fprintf(out, "cancelled %s (kept for the record; `resume --prune` to delete)\n", rec.ID)
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

			// --claim: the ONLY side-effecting path — apply the work and stamp.
			if !rec.Work.Clean {
				if err := Apply(rec.Work, root); err != nil {
					return err
				}
				fmt.Fprintf(out, "\n── work ──\nrestored into %s\n", root)
			}
			now := time.Now().UTC()
			self, _ := principal.NewResolver(fleet.New(), principal.DefaultEnv()).Self()
			rec.ResumedAt, rec.ResumedBy = &now, &self
			if _, err := Save(dir, rec); err != nil {
				return err
			}
			fmt.Fprintf(out, "\nCLAIMED by %s. Re-run `bashy resume` to confirm the seat is held.\n", refName(self))
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
	roots := []string{root}
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		return roots
	}
	seen := map[string]bool{root: true}
	for _, line := range strings.Split(string(data), "\n") {
		i := strings.Index(line, "=>")
		if i < 0 {
			continue
		}
		rhs := strings.TrimSpace(line[i+2:])
		if sp := strings.IndexAny(rhs, " \t"); sp >= 0 {
			rhs = rhs[:sp]
		}
		if !strings.HasPrefix(rhs, "../") {
			continue
		}
		parts := strings.Split(filepath.ToSlash(rhs), "/")
		if len(parts) < 2 || parts[1] == "" || parts[1] == ".." {
			continue
		}
		abs := filepath.Clean(filepath.Join(root, "..", parts[1]))
		if fi, err := os.Stat(abs); err == nil && fi.IsDir() && !seen[abs] {
			seen[abs] = true
			roots = append(roots, abs)
		}
	}
	return roots
}

func repoRoot(dir string) (string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
