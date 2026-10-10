package weave

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/policy/coord"
	"github.com/qiangli/yoke/pkg/principal"
)

// Baton is the CONDUCTOR handoff note for a local weave campaign: the intent,
// strategy, and next moves that the queue itself does NOT capture, so a fresh
// conductor can pick up from `weave baton` + a live `weave list` without reading
// code or docs. It is rewritten at each handoff stage (mid-sprint on trouble,
// end-of-sprint). Stored at <queueDir>/baton.json — per-campaign, system-wide.
type Baton struct {
	Goal        string    `json:"goal"`         // the campaign's north star + done-criteria
	Stage       string    `json:"stage"`        // current sprint/phase, e.g. "sprint 2 of 4"
	Plan        string    `json:"plan"`         // sprint plan / decomposition strategy
	Done        []string  `json:"done"`         // what's merged/verified (one line each)
	NextActions []string  `json:"next_actions"` // the next conductor's first moves
	Lessons     []string  `json:"lessons"`      // gotchas / routing decisions / tool notes
	Notes       string    `json:"notes"`        // free narrative
	WrittenBy   string    `json:"written_by"`
	WrittenAt   time.Time `json:"written_at"`
}

func batonPath(queueDir string) string { return filepath.Join(queueDir, "baton.json") }

// conductorLockTTL bounds how long a lock survives without a heartbeat. A live
// conductor refreshes it on every baton write/take; if it goes stale (crash,
// ratelimit drop), a successor may take over without --force.
const conductorLockTTL = 30 * time.Minute

// batonKind is the coord kind of the conductor lock, keyed by the queue dir.
const batonKind = "baton"

func init() {
	coord.RegisterKind(coord.Kind{
		Name: batonKind, Match: coord.MatchName, TTL: conductorLockTTL,
		Modes: []string{coord.ModeLease},
	})
}

// ConductorLock is the single-driver guard for a campaign: only its holder
// should drive the sprint, so two conductors never double-drive one queue. It
// is a VIEW of the coord claim baton:<queueDir>; Epoch is the claim's fencing
// token, bumped on each take — a stale-epoch holder (an old conductor that
// resumed after a takeover) is refused by coord.
type ConductorLock struct {
	Holder      string    `json:"holder"`
	Epoch       int       `json:"epoch"`
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`

	claim *coord.Claim
}

func batonRef(queueDir string) coord.Ref { return coord.Ref{Kind: batonKind, Name: queueDir} }

func conductorPrincipal(holder string) principal.Ref { return principal.Ref{Name: holder} }

func conductorLockOf(c *coord.Claim) *ConductorLock {
	return &ConductorLock{Holder: c.Holder.Name, Epoch: int(c.Epoch),
		AcquiredAt: c.AcquiredAt, HeartbeatAt: c.Heartbeat, claim: c}
}

func loadConductorLock(queueDir string) (*ConductorLock, bool) {
	claims, err := coord.List(coord.DefaultDir())
	if err != nil {
		return nil, false
	}
	for _, c := range claims {
		if c.Kind == batonKind && c.Resource == queueDir {
			return conductorLockOf(c), true
		}
	}
	return nil, false
}

// stale reports a lock a successor may take without --force. An unknown lock
// (no heartbeat recorded) is not stale: nothing says its holder is gone.
func (l *ConductorLock) stale(now time.Time) bool {
	if l.claim == nil {
		return now.Sub(l.HeartbeatAt) > conductorLockTTL
	}
	return l.claim.Stale(now)
}

// acquireConductorLock claims the lock for holder through the coord ledger. It
// succeeds if the lock is free, held by holder already, lapsed, or force is
// set — bumping the fencing epoch on a real change of holder. On refusal it
// returns the current holder's lock and a *coord.Conflict so the caller can
// report who is already driving.
func acquireConductorLock(queueDir, holder string, force bool) (*ConductorLock, error) {
	g, err := coord.AcquireRef(context.Background(), coord.Request{
		Ref: batonRef(queueDir), Holder: conductorPrincipal(holder),
		Intent: "conductor", Mode: coord.ModeLease, Force: force,
	})
	if err != nil {
		var conflict *coord.Conflict
		if errors.As(err, &conflict) {
			return conductorLockOf(conflict.Claim), err
		}
		return nil, err
	}
	return conductorLockOf(g.Claim), nil
}

// heartbeatConductorLock refreshes holder's lock. epoch 0 means "my current
// lock"; a non-zero epoch that is no longer the lock's is coord.ErrFenced.
func heartbeatConductorLock(queueDir, holder string, epoch uint64) error {
	_, err := coord.Refresh(context.Background(), batonRef(queueDir), conductorPrincipal(holder), epoch)
	return err
}

// releaseConductorLock drops the lock. An empty holder releases whoever holds
// it; releasing an absent lock is a no-op.
func releaseConductorLock(queueDir, holder string, epoch uint64) error {
	if holder == "" {
		cur, ok := loadConductorLock(queueDir)
		if !ok {
			return nil
		}
		holder = cur.Holder
	}
	return coord.ReleaseRef(context.Background(), batonRef(queueDir), conductorPrincipal(holder), epoch)
}

func loadBaton(queueDir string) (*Baton, bool) {
	b, err := os.ReadFile(batonPath(queueDir))
	if err != nil {
		return nil, false
	}
	var bt Baton
	if json.Unmarshal(b, &bt) != nil {
		return nil, false
	}
	return &bt, true
}

func saveBaton(queueDir string, bt *Baton) error {
	if err := ensureWeaveQueueDirPath(queueDir); err != nil {
		return err
	}
	bt.WrittenAt = time.Now()
	b, err := json.MarshalIndent(bt, "", "  ")
	if err != nil {
		return err
	}
	return weaveWriteFile(batonPath(queueDir), b, 0o644)
}

// renderBaton formats the baton as a self-contained markdown handoff brief — the
// thing a fresh conductor reads first. It ends with the exact live-state
// commands so the new conductor reconciles intent (this note) with current
// reality (the queue), then resumes.
func renderBaton(bt *Baton) string {
	var s strings.Builder
	fmt.Fprintf(&s, "# Conductor baton")
	if bt.Stage != "" {
		fmt.Fprintf(&s, " — %s", bt.Stage)
	}
	s.WriteString("\n\n")
	if bt.Goal != "" {
		fmt.Fprintf(&s, "## Goal\n%s\n\n", bt.Goal)
	}
	if bt.Plan != "" {
		fmt.Fprintf(&s, "## Plan\n%s\n\n", bt.Plan)
	}
	writeList := func(title string, items []string) {
		if len(items) == 0 {
			return
		}
		fmt.Fprintf(&s, "## %s\n", title)
		for _, it := range items {
			fmt.Fprintf(&s, "- %s\n", it)
		}
		s.WriteString("\n")
	}
	writeList("Done (merged/verified)", bt.Done)
	writeList("Next actions", bt.NextActions)
	writeList("Lessons / routing", bt.Lessons)
	if bt.Notes != "" {
		fmt.Fprintf(&s, "## Notes\n%s\n\n", bt.Notes)
	}
	by := bt.WrittenBy
	if by == "" {
		by = "unknown"
	}
	fmt.Fprintf(&s, "_baton written by %s at %s_\n\n", by, bt.WrittenAt.Local().Format(time.RFC3339))
	s.WriteString("## Reconcile with live state before resuming\n")
	s.WriteString("- `bashy weave list`               — issue states (todo/working/submitted/merged/failed)\n")
	s.WriteString("- `bashy weave fleet --probe`       — which tools are available right now\n")
	s.WriteString("- `bashy weave fleet interview --all` — per-tool launch contracts + role ratings\n")
	s.WriteString("- `bashy weave guide`              — the conductor playbook\n")
	return s.String()
}

func newWeaveBatonCmd() *cobra.Command {
	var flags weaveOutputFlags
	cmd := &cobra.Command{
		Use:   "baton",
		Short: "Show THIS repo's single-driver lock + campaign handoff note (cross-repo handoff: `bashy sprint`)",
		Long: `baton is the CONDUCTOR handoff note for THIS campaign. The current
conductor writes it ('weave baton write …') at each handoff stage — mid-sprint
when it must drop (ratelimit / token overuse / failure) or at end-of-sprint — so
the next conductor resumes from the intent + strategy + next moves WITHOUT
reading code or docs, then reconciles against live 'weave list'. (For the
cloudbox shared-session lease handoff, see 'weave handoff'.)`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runWeaveBatonShow(cmd, &flags)
		},
	}
	flags.attach(cmd)
	cmd.AddCommand(newWeaveBatonWriteCmd())
	cmd.AddCommand(newWeaveBatonTakeCmd())
	cmd.AddCommand(newWeaveBatonReleaseCmd())
	return cmd
}

// newWeaveBatonTakeCmd: `weave baton take --as <name> [--force]` — claim the
// conductor lock so no two conductors drive the same campaign. Refuses if
// another conductor is actively holding it (heartbeat within the TTL) unless
// --force. Prints the baton on success so the new conductor picks up at once.
func newWeaveBatonTakeCmd() *cobra.Command {
	var flags weaveOutputFlags
	var as string
	var force bool
	cmd := &cobra.Command{
		Use:   "take",
		Short: "Claim the conductor lock (single-driver guard) and show the baton",
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			if as == "" {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton take", weavecli.ExitGenericFail,
					fmt.Errorf("--as <conductor-name> is required")))
			}
			dir, err := weaveQueueDirForCwd()
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton take", weavecli.ExitGenericFail, err))
			}
			l, err := acquireConductorLock(dir, as, force)
			if err != nil {
				var conflict *coord.Conflict
				if !errors.As(err, &conflict) {
					return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton take", weavecli.ExitGenericFail, err))
				}
				if mode != weavecli.OutputJSON {
					fmt.Fprintf(cmd.OutOrStdout(), "REFUSED — %s is conducting (last heartbeat %s). Use --force only if they are truly gone.\n",
						l.Holder, l.HeartbeatAt.Local().Format("15:04:05"))
				}
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton take", weavecli.ExitPrecondFail,
					fmt.Errorf("conductor lock held by %s", l.Holder)))
			}
			if mode != weavecli.OutputJSON {
				fmt.Fprintf(cmd.OutOrStdout(), "conductor lock acquired by %s (epoch %d)\n\n", l.Holder, l.Epoch)
				if bt, ok := loadBaton(dir); ok {
					fmt.Fprint(cmd.OutOrStdout(), renderBaton(bt))
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "no baton yet — write one with `weave baton write` as you go.")
				}
			}
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave baton take",
				map[string]any{"holder": l.Holder, "epoch": l.Epoch}))
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&as, "as", "", "Conductor name claiming the lock")
	cmd.Flags().BoolVar(&force, "force", false, "Take over even if another conductor holds a live lock")
	return cmd
}

// newWeaveBatonReleaseCmd: `weave baton release [--as <name>]` — drop the
// conductor lock on a clean handoff.
func newWeaveBatonReleaseCmd() *cobra.Command {
	var flags weaveOutputFlags
	var as string
	var epoch uint64
	cmd := &cobra.Command{
		Use:   "release",
		Short: "Release the conductor lock (clean handoff)",
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			dir, err := weaveQueueDirForCwd()
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton release", weavecli.ExitGenericFail, err))
			}
			if err := releaseConductorLock(dir, as, epoch); err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton release", weavecli.ExitPrecondFail, err))
			}
			if mode != weavecli.OutputJSON {
				fmt.Fprintln(cmd.OutOrStdout(), "conductor lock released")
			}
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave baton release", nil))
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&as, "as", "", "Conductor name releasing (only releases if it matches the holder)")
	cmd.Flags().Uint64Var(&epoch, "epoch", 0, "Epoch you hold; a stale one is refused (0 = current)")
	return cmd
}

// weaveQueueDirForCwd resolves the queue dir for the current repo.
func weaveQueueDirForCwd() (string, error) {
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return "", err
	}
	return weaveQueueDir(root)
}

func runWeaveBatonShow(cmd *cobra.Command, flags *weaveOutputFlags) error {
	mode := flags.mode()
	cwd, _ := os.Getwd()
	root, err := weaveRepoRoot(cwd)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton", weavecli.ExitPrecondFail, err))
	}
	dir, err := weaveQueueDir(root)
	if err != nil {
		return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton", weavecli.ExitGenericFail, err))
	}
	bt, hasBaton := loadBaton(dir)
	lock, hasLock := loadConductorLock(dir)
	if mode == weavecli.OutputJSON {
		out := map[string]any{"baton": nil}
		if hasBaton {
			out["baton"] = bt
		}
		if hasLock {
			out["conductor"] = lock
		}
		if c, _ := loadAutopilotRoom(dir); c != nil {
			out["contact"] = c
		}
		return ec(emitOK(cmd.OutOrStdout(), mode, "weave baton", out))
	}
	now := time.Now()
	if hasLock && lock.Holder != "" {
		st := "active"
		if lock.stale(now) {
			st = "STALE — takeable without --force"
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Conductor: %s (epoch %d, %s; last heartbeat %s)\n",
			lock.Holder, lock.Epoch, st, lock.HeartbeatAt.Local().Format("15:04:05"))
		// WHERE to reach them, next to WHO they are. Baton is what an agent
		// reads before touching this repo, so the two facts belong on adjacent
		// lines — knowing a campaign has a driver and not how to ask them
		// anything is most of the way to interrupting it instead.
		if c, _ := loadAutopilotRoom(dir); c != nil {
			fmt.Fprintf(cmd.OutOrStdout(), "Reach them: %s\n", c.String())
		}
		fmt.Fprintln(cmd.OutOrStdout())
	} else {
		fmt.Fprint(cmd.OutOrStdout(), "Conductor: none — `weave baton take --as <name>` to claim\n\n")
	}
	if hasBaton {
		fmt.Fprint(cmd.OutOrStdout(), renderBaton(bt))
	} else {
		fmt.Fprintln(cmd.OutOrStdout(), "no baton yet — the conductor writes one with `weave baton write …`")
	}
	return ec(emitOK(cmd.OutOrStdout(), mode, "weave baton", nil))
}

func newWeaveBatonWriteCmd() *cobra.Command {
	var flags weaveOutputFlags
	var bt Baton
	var next, lessons, done []string
	var epoch uint64
	cmd := &cobra.Command{
		Use:   "write",
		Short: "Write/update the conductor handoff note for this campaign",
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			cwd, _ := os.Getwd()
			root, err := weaveRepoRoot(cwd)
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton write", weavecli.ExitPrecondFail, err))
			}
			dir, err := weaveQueueDir(root)
			if err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton write", weavecli.ExitGenericFail, err))
			}
			// Merge onto any existing baton so partial updates accrue.
			cur, _ := loadBaton(dir)
			if cur == nil {
				cur = &Baton{}
			}
			if bt.Goal != "" {
				cur.Goal = bt.Goal
			}
			if bt.Stage != "" {
				cur.Stage = bt.Stage
			}
			if bt.Plan != "" {
				cur.Plan = bt.Plan
			}
			if bt.Notes != "" {
				cur.Notes = bt.Notes
			}
			if bt.WrittenBy != "" {
				cur.WrittenBy = bt.WrittenBy
			}
			if len(done) > 0 {
				cur.Done = append(cur.Done, done...)
			}
			if len(next) > 0 {
				cur.NextActions = next // next actions REPLACE (they're the current to-do)
			}
			if len(lessons) > 0 {
				cur.Lessons = append(cur.Lessons, lessons...)
			}
			// Writing the baton is a heartbeat — it refreshes the conductor lock
			// so the holder's lock doesn't go stale while they are actively
			// driving (and supervising → recording is the rhythm). A fenced
			// writer was taken over and must not overwrite its successor's note.
			if cur.WrittenBy != "" {
				if err := heartbeatConductorLock(dir, cur.WrittenBy, epoch); errors.Is(err, coord.ErrFenced) {
					return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton write", weavecli.ExitPrecondFail, err))
				}
			}
			if err := saveBaton(dir, cur); err != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "weave baton write", weavecli.ExitGenericFail, err))
			}
			if mode != weavecli.OutputJSON {
				fmt.Fprintln(cmd.OutOrStdout(), "baton written — next conductor: `bashy weave baton`")
			}
			return ec(emitOK(cmd.OutOrStdout(), mode, "weave baton write", map[string]any{"written_by": cur.WrittenBy}))
		},
	}
	flags.attach(cmd)
	cmd.Flags().StringVar(&bt.Goal, "goal", "", "Campaign north star + done-criteria")
	cmd.Flags().StringVar(&bt.Stage, "stage", "", "Current stage, e.g. 'sprint 2 of 4'")
	cmd.Flags().StringVar(&bt.Plan, "plan", "", "Sprint plan / decomposition strategy")
	cmd.Flags().StringVar(&bt.Notes, "notes", "", "Free narrative")
	cmd.Flags().StringVar(&bt.WrittenBy, "by", "", "Who is handing off (tool/agent name)")
	cmd.Flags().Uint64Var(&epoch, "epoch", 0, "Conductor epoch you hold; a stale one is refused (0 = current)")
	cmd.Flags().StringArrayVar(&done, "done", nil, "Append a merged/verified item (repeatable)")
	cmd.Flags().StringArrayVar(&next, "next", nil, "Next action for the new conductor (repeatable; replaces prior)")
	cmd.Flags().StringArrayVar(&lessons, "lesson", nil, "Append a lesson/routing note (repeatable)")
	return cmd
}
