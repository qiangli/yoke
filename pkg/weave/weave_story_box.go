package weave

// THE TIME-BOX — the half of a sprint the kanban column cannot express.
//
// A column says WHERE work is (backlog, doing, done). Nothing says how
// long it has. That gap is not cosmetic when the worker is an agent: a session
// has no natural end. It does not get hungry, notice the light change, or feel
// a day turning into an evening. Left alone it will follow the next reasonable
// thread, and the one after that, and each step is defensible while the whole
// drifts far from what was agreed.
//
// So a sprint gets a START and a CUTOFF, and the cutoff is a DECISION POINT
// rather than a kill switch. Refusing to work past it would be wrong — the
// gate might be one fix from green, and stopping there wastes the run. What
// the cutoff buys is that the moment ARRIVES VISIBLY instead of passing
// unnoticed: every sprint command says how long is left, and says OVERDUE
// after, so continuing is a choice somebody makes rather than a default nobody
// observed.
//
// # Cadence comes from the record, not the plan
//
// `stop` writes what actually happened next to what was planned. That is the
// only thing that makes a cadence real: a two-hour box that consistently runs
// four hours is not a two-hour cadence, it is a four-hour cadence with a
// misleading label, and only the record can tell you which one you have.
//
// # Many boxes at once, each on its own clock
//
// The box lives on the CARD, never in a global "current sprint". There is no
// single active sprint anywhere in this package, and that is deliberate: real
// work runs several initiatives side by side on different rhythms — a 45-minute
// fix box beside a 4-hour migration box beside something opened yesterday and
// still going. Each start/stop cycle is independent, and starting one says
// nothing about any other.
//
// The one thing `start` refuses is restarting THE SAME card while its box is
// still running, because that would discard the original estimate — the single
// number the record exists to keep honest.
//
// # Why this is separate from the conductor lease
//
// The lease already carries a timestamp, and reusing it would have been
// tempting. It answers a different question. The lease asks "is a conductor
// still alive?" — a liveness heartbeat that a graceful handoff clears and a
// successor takes over. The box asks "should this work still be running?" —
// which stays true across a handoff, because the commitment belongs to the
// sprint and not to whoever is currently holding it. Collapsing them would
// mean a conductor switch silently reset the deadline.

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/fleet"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/yoke/pkg/role"
)

// DefaultSprintBox is the cadence when none is given. Two hours is chosen to
// be a REVIEW interval rather than a day's work: long enough to finish
// something and short enough that a wrong direction is caught while it is
// still cheap to abandon.
const DefaultSprintBox = 2 * time.Hour

// weaveStoryBox is a sprint's time commitment.
//
// StoppedAt being nil is what makes a sprint "running", so the zero value is
// correctly "never started" rather than "started at the epoch".
type weaveStoryBox struct {
	StartedAt time.Time  `json:"started_at"`
	Cutoff    time.Time  `json:"cutoff"`
	StoppedAt *time.Time `json:"stopped_at,omitempty"`
	// Planned is stored rather than derived from Cutoff-StartedAt so that
	// extending a running box keeps the ORIGINAL commitment visible. What was
	// promised and what was taken are different facts, and a cadence tuned from
	// the second one alone would only ever ratchet upward.
	Planned time.Duration `json:"planned"`
	// Draining records when a graceful stop began. It survives a FAILED drain,
	// which is the point: workers are already parked, and the sprint stays open
	// in this state until the gate goes green. A conductor returning to it can
	// see the stop was already attempted rather than starting the reasoning over.
	Draining *time.Time `json:"draining,omitempty"`
	// GateRan / GatePassed are the evidence the stop was clean. Both false means
	// UNVERIFIED, never "fine" — absence of evidence is not success.
	GateRan    bool   `json:"gate_ran,omitempty"`
	GatePassed bool   `json:"gate_passed,omitempty"`
	GateCmd    string `json:"gate_cmd,omitempty"`
}

// Running reports a box that has started and not stopped.
func (b *weaveStoryBox) Running() bool {
	return b != nil && !b.StartedAt.IsZero() && b.StoppedAt == nil
}

// Remaining is time left before the cutoff; negative once past it.
func (b *weaveStoryBox) Remaining(now time.Time) time.Duration {
	if b == nil {
		return 0
	}
	return b.Cutoff.Sub(now)
}

// Overdue reports a running box past its cutoff.
func (b *weaveStoryBox) Overdue(now time.Time) bool {
	return b.Running() && now.After(b.Cutoff)
}

// Elapsed is how long the box has actually been open — to the stop if it
// stopped, to now if it is still running.
func (b *weaveStoryBox) Elapsed(now time.Time) time.Duration {
	if b == nil || b.StartedAt.IsZero() {
		return 0
	}
	if b.StoppedAt != nil {
		return b.StoppedAt.Sub(b.StartedAt)
	}
	return now.Sub(b.StartedAt)
}

// Status is the one-line marker every sprint surface shows, so the cutoff is
// never something a caller has to remember to ask about.
//
// An empty string means there is nothing to say — a sprint that was never
// boxed reads exactly as it did before this existed.
func (b *weaveStoryBox) Status(now time.Time) string {
	switch {
	case b == nil || b.StartedAt.IsZero():
		return ""
	case b.StoppedAt != nil:
		return fmt.Sprintf("stopped after %s (planned %s)",
			roundDur(b.Elapsed(now)), roundDur(b.Planned))
	case b.Overdue(now):
		// Stated as time PAST the cutoff, not as a negative remaining: "overdue
		// by 40m" is a fact somebody can act on, "-40m left" is arithmetic they
		// have to finish themselves.
		return fmt.Sprintf("OVERDUE by %s (box was %s)",
			roundDur(now.Sub(b.Cutoff)), roundDur(b.Planned))
	default:
		return fmt.Sprintf("%s left of %s", roundDur(b.Remaining(now)), roundDur(b.Planned))
	}
}

// roundDur trims a duration to something a human reads at a glance. Sub-minute
// precision on a two-hour box is noise, but it matters on a box measured in
// minutes, so the unit follows the magnitude.
func roundDur(d time.Duration) string {
	if d < 0 {
		d = -d
	}
	if d < time.Minute {
		return d.Round(time.Second).String()
	}
	d = d.Round(time.Minute)
	h, m := int(d/time.Hour), int((d%time.Hour)/time.Minute)
	switch {
	case h == 0:
		return fmt.Sprintf("%dm", m)
	case m == 0:
		// "4h", not Go's "4h0m0s" — a cadence is read at a glance dozens of
		// times a day, and the zero components are pure noise in every one.
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dh%dm", h, m)
	}
}

// newSprintStartCmd opens a sprint's time-box.
func newSprintStartCmd() *cobra.Command {
	var flags weaveOutputFlags
	var forDur time.Duration
	var as, instruction string
	cmd := &cobra.Command{
		Use:   "start <sprint>",
		Short: "Open a sprint's time-box: start the clock and set a cutoff",
		Long: "start commits a sprint to a length of time.\n\n" +
			"The cutoff is a DECISION POINT, not a kill switch. Nothing refuses to run\n" +
			"past it — the gate might be one fix from green, and stopping there wastes\n" +
			"the run. What it buys is that the moment ARRIVES VISIBLY: every sprint\n" +
			"command reports the time left, and says OVERDUE after, so continuing is a\n" +
			"choice somebody makes rather than a default nobody noticed.\n\n" +
			"This matters most when the worker is an agent. A session has no natural\n" +
			"end — it will follow the next reasonable thread, and the one after that,\n" +
			"each step defensible while the whole drifts from what was agreed.\n\n" +
			"A sprint in `backlog` also moves to `doing`, because starting the clock and\n" +
			"starting the work are the same act.",
		Example: "  bashy sprint start 3 --owner AGENT_NAME --instruction \"implement the agreed plan\"\n" +
			"  bashy sprint start 3 --owner AGENT_NAME --for 45m\n" +
			"  bashy sprint start 3 --owner AGENT_NAME --for 4h",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			if forDur <= 0 {
				return fmt.Errorf("--for must be positive (got %s)", forDur)
			}
			now := time.Now().UTC()
			cwd, _ := os.Getwd()
			return runSprintOwnerLifecycle(cmd, &flags, id, "sprint start", "start managed sprint owner", func() error {
				if !cmd.Flags().Changed("owner") || strings.TrimSpace(as) == "" {
					return fmt.Errorf("--owner is required, and there are two cases.\n" +
						"  YOU are the manager (you were told to take this sprint): use your OWN name.\n" +
						"    `bashy agent list` to find it, or register one:\n" +
						"    `bashy agent add <name> --tool <tool> --model <model>`\n" +
						"    then re-run with --owner <name>. That is not a guess; it is your identity.\n" +
						"  You are appointing SOMEONE ELSE: choose a NAME from `bashy agent list`\n" +
						"    and ask the user rather than guessing on their behalf.")
				}
				who := strings.TrimSpace(as)
				if err := validateSprintClaimant(who); err != nil {
					return err
				}
				who, _ = canonicalFleetAgentName(who)
				before, err := sprintOwnerSnapshot(id)
				if err != nil {
					return err
				}
				if err := sprintOrientationError(before); err != nil {
					return err
				}
				if before.currentBox().Running() {
					return fmt.Errorf("sprint #%d is already running (%s) — `sprint stop %d` first, or `sprint extend %d --by <dur>`",
						id, before.currentBox().Status(now), id, id)
				}
				if prev, stale, free := weaveStoryLeaseState(before); !free && !stale && prev != who {
					return fmt.Errorf("sprint #%d is held by %s — `sprint take %d` to assume delivery first", id, prev, id)
				}
				expectedOwner := strings.TrimSpace(before.Owner)
				if expectedOwner != "" && !strings.EqualFold(expectedOwner, who) {
					if err := retireSprintOwnerSession(cmd.Context(), id, expectedOwner, cwd); err != nil {
						return fmt.Errorf("cannot transfer sprint #%d manager from %s to %s: %w", id, expectedOwner, who, err)
					}
				}
				sessionNote, launched, err := ensureSprintOwnerSession(cmd.Context(), id, who, instruction, cwd, forDur)
				if err != nil {
					if launched {
						_ = retireSprintOwnerSession(cmd.Context(), id, who, cwd)
					}
					return fmt.Errorf("sprint #%d manager instruction was not delivered: %w", id, err)
				}
				err = runWeaveStoryMutate(cmd, id, "sprint start", &flags, func(s *weaveStory) (string, error) {
					// Starting is the point at which ownership becomes active. Require the
					// caller to name that owner on THIS command: neither ambient process
					// identity nor a manager recorded by an earlier edit/take is consent to
					// start work now.
					if !cmd.Flags().Changed("owner") || strings.TrimSpace(as) == "" {
						return "", fmt.Errorf("--owner is required, and there are two cases.\n" +
							"  YOU are the manager (you were told to take this sprint): use your OWN name.\n" +
							"    `bashy agent list` to find it, or register one:\n" +
							"    `bashy agent add <name> --tool <tool> --model <model>`\n" +
							"    then re-run with --owner <name>. That is not a guess; it is your identity.\n" +
							"  You are appointing SOMEONE ELSE: choose a NAME from `bashy agent list`\n" +
							"    and ask the user rather than guessing on their behalf.")
					}
					// Restarting a RUNNING box would silently discard the original
					// commitment, which is the one number the record exists to keep
					// honest. Extending is a different, explicit act.
					if s.currentBox().Running() {
						return "", fmt.Errorf("sprint #%d is already running (%s) — `sprint stop %d` first, or `sprint extend %d --by <dur>`",
							id, s.currentBox().Status(now), id, id)
					}
					// A BOX IN FLIGHT MUST HAVE AN OWNER. The conductor holding the
					// lease is the sprint's scrum master: accountable for its
					// delivery, for calling the stop, and for the decision when the
					// cutoff arrives. A running box nobody holds is how a deadline
					// passes with everyone assuming someone else was watching — and
					// with several sprints running at once, that is the normal way
					// to lose one rather than an unlucky one.
					//
					// So start CLAIMS a free (or stale) lease, and refuses to take a
					// live one from someone else: quietly reassigning delivery
					// ownership is not something a start command should do.
					if strings.TrimSpace(s.Owner) != expectedOwner {
						return "", fmt.Errorf("sprint #%d sprint manager changed concurrently from %s to %s", id, expectedOwner, s.Owner)
					}
					if prev, stale, free := weaveStoryLeaseState(s); !free && !stale && prev != who {
						return "", fmt.Errorf("sprint #%d is held by %s — `sprint take %d` to assume delivery first",
							id, prev, id)
					}
					// AN OWNER MUST BE AN ADDRESS. Refuse a name that resolves to
					// no agent: every coordination surface (mb, chat, inbox, ping)
					// keys on this string, and one that names nobody is worse than
					// none — it is printed as a contact and silently never answers.
					// CLAIM-TIME: the seat must be RUNNING, not merely declared.
					// A sprint seated to a name with no process behind it accepts
					// room messages and inbox mail that nobody will ever read.
					s.Lease = &weaveStoryLease{Holder: who, At: now}
					s.Owner = who
					s.Boxes = append(s.Boxes, weaveStoryBox{StartedAt: now, Cutoff: now.Add(forDur), Planned: forDur})
					// A room is opened automatically, because an OPTIONAL room is
					// empty exactly when it is needed: at the moment somebody
					// urgent arrives and the conductor is mid-turn elsewhere.
					// Failure is not fatal — a conductor with no intercom still
					// has a box to run, and the surfaces say "no contact" rather
					// than implying one.
					roomNote := ""
					if s.Contact == nil {
						c, cerr := openSprintRoom(s, who)
						switch {
						case cerr != nil:
							// Reported, never swallowed. A contact that silently
							// failed to open reads identically to one nobody has
							// tried to use yet, and the difference matters at
							// exactly the moment someone needs to reach in.
							roomNote = fmt.Sprintf("; no room (%v)", cerr)
						default:
							s.Contact = c
							roomNote = "; " + c.String()
						}
					} else {
						roomNote = "; " + s.Contact.String()
					}
					moved := ""
					if s.Column == "backlog" {
						s.Column = "doing"
						moved = " (backlog → doing)"
					}
					weaveStoryAppend(s, who, kindStage,
						fmt.Sprintf("started a %s box%s, cutoff %s", roundDur(forDur), moved, now.Add(forDur).Format(time.RFC3339)))
					// The same advisory `take` gives. `start` is the more common
					// entry point — it is how a sprint BEGINS — and it was the one
					// that said nothing, so an agent seated by it had to already
					// know how mail reaches it.
					return fmt.Sprintf("sprint #%d started%s — %s, cutoff %s; conducted by %s%s\n%s\n%s",
						id, moved, roundDur(forDur), now.Add(forDur).Format("15:04 MST"), who, roomNote+sessionNote,
						sprintReadyLine(id, who), sprintOrientationLine(s)), nil
				})
				if err != nil && launched {
					_ = retireSprintOwnerSession(cmd.Context(), id, who, cwd)
				}
				return err
			})
		},
	}
	cmd.Flags().DurationVar(&forDur, "for", DefaultSprintBox, "how long this sprint gets")
	// ONE FLAG, DOMAIN TITLES. --owner is the single spelling across meet,
	// sprint and todo; here it is called the PROJECT MANAGER, because that is
	// what a sprint's owner is. --as remains reserved for acting identity on
	// authorship and inbox commands; it is not an ownership flag.
	//
	// The conductor:<n> wire address is NOT renamed. It is resolved by
	// bus.RegisterHostRoles at read time and story f93fdf47810c exists because
	// it once resolved to nothing; the spoken word changes, the address does not.
	role.AttachOwner(cmd.Flags(), &as, role.ProjectManager,
		"accountable for delivery from start to end; required explicitly on every start")
	cmd.Flags().StringVar(&instruction, "instruction", "", "launch or reuse the sprint manager's managed session and deliver this request once")
	flags.attach(cmd)
	return cmd
}

// newSprintInstructCmd sends a later instruction to the current manager. It
// deliberately has no --owner flag: changing ownership and talking to the
// current owner are different operations, and this command must not combine
// them or silently launch a second identity.
func newSprintInstructCmd() *cobra.Command {
	var flags weaveOutputFlags
	var instruction string
	cmd := &cobra.Command{
		Use:   "instruct <sprint>",
		Short: "Deliver one instruction to an active sprint's managed sprint manager",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			if strings.TrimSpace(instruction) == "" {
				return fmt.Errorf("--instruction is required")
			}
			return runSprintOwnerLifecycle(cmd, &flags, id, "sprint instruct", "instruct managed sprint owner", func() error {
				dir, err := weaveStoryDir(cmd, flags.mode(), "sprint instruct")
				if err != nil {
					return err
				}
				q, err := readWeaveQueue(dir)
				if err != nil {
					return err
				}
				s := findWeaveStory(q, id)
				if s == nil {
					return fmt.Errorf("sprint #%d not found", id)
				}
				box := s.currentBox()
				if !sprintColumnOpen(s.Column) || box == nil || !box.Running() {
					return fmt.Errorf("sprint #%d is not active", id)
				}
				owner := strings.TrimSpace(s.Owner)
				if owner == "" {
					return fmt.Errorf("sprint #%d has no sprint manager", id)
				}
				cwd, _ := os.Getwd()
				note, _, err := ensureSprintOwnerSession(cmd.Context(), id, owner, instruction, cwd, time.Until(box.Cutoff))
				if err != nil {
					return fmt.Errorf("sprint #%d instruction was not delivered: %w", id, err)
				}
				contact := "none"
				if s.Contact != nil {
					contact = s.Contact.String()
				}
				if flags.mode() == weavecli.OutputJSON {
					return ec(emitOK(cmd.OutOrStdout(), flags.mode(), "sprint instruct", map[string]any{
						"sprint": id, "owner": owner, "contact": contact, "delivery": strings.TrimPrefix(note, "; "),
					}))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "sprint instruct: sprint #%d; owner %s; meet %s%s\n", id, owner, contact, note)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&instruction, "instruction", "", "exact request to deliver once to the current sprint manager")
	flags.attach(cmd)
	return cmd
}

// newSprintStopCmd closes the box and records what actually happened.
func newSprintStopCmd() *cobra.Command {
	return newSprintCloseCmd(false)
}

// newSprintEndCmd closes the lifecycle, not merely the current time-box.
// It deliberately has no --force escape hatch: "done" must mean that linked
// work is parked and repositories are wrapped. A supplied gate is also binding,
// but a sprint that has no applicable gate may still end and records that fact
// as unverified.
func newSprintEndCmd() *cobra.Command {
	return newSprintCloseCmd(true)
}

func newSprintCloseCmd(ending bool) *cobra.Command {
	var flags weaveOutputFlags
	var note, gateCmd, gateDir string
	var gateTimeout time.Duration
	var force, noVerify bool
	verb := "stop"
	short := "Close a sprint's time-box and record planned vs actual"
	if ending {
		verb = "end"
		short = "Finish a sprint after workers, repositories, and gates are consistent"
	}
	op := "sprint " + verb
	long := "stop DRAINS a sprint: it parks the workers and records the current state,\n"
	if ending {
		long = "end DRAINS and closes the sprint lifecycle: it parks linked workers, requires wrapped repositories, runs any supplied gate, moves the card to done, and releases the conductor lease.\n\n"
	}
	cmd := &cobra.Command{
		Use:   verb + " <sprint>",
		Short: short,
		Long: long +
			"and only then closes the clock.\n\n" +
			"The reason is preemption. Something more urgent arrives, this work must\n" +
			"yield, and it must yield in a state somebody can pick up cold. A stop is not\n" +
			"graceful because it was polite — it is graceful because three things are TRUE\n" +
			"when it finishes: no worker is still running (workspace and branch preserved,\n" +
			"exactly as `weave pause` leaves them), any supplied gate has passed, and there\n" +
			"is a continuity record saying where it left off. Without a gate, the record\n" +
			"says UNVERIFIED and lifecycle tracking still proceeds.\n\n" +
			"A RED GATE REFUSES TO CLOSE THE SPRINT, and that is the feature. The workers\n" +
			"are already parked so nothing is burning, and the remaining job is narrow:\n" +
			"fix the regression and run stop again. Closing over a red gate would file the\n" +
			"sprint as done and hand the next one damage it cannot tell from its own.\n\n" +
			"SPRINT WORK GOES THROUGH WEAVE, NEVER DIRECT EDITS. That is what makes a\n" +
			"hard stop survivable: unmerged branches cannot break the tree, so parking\n" +
			"them costs nothing — and a red gate therefore means something ALREADY\n" +
			"LANDED, which is the next sprint's inheritance either way. The escape hatch\n" +
			"is never --force; it is to leave the weave unmerged or `weave abandon` it,\n" +
			"losing one branch instead of a repo.\n\n" +
			"NO GATE IS NOT A PASS. Without --gate nothing is verified, and stop records\n" +
			"that state without turning optional test policy into a lifecycle blocker.\n\n" +
			"It also writes what ACTUALLY happened beside what was\n" +
			"planned.\n\n" +
			"That record is the only thing that makes a cadence real. A two-hour box\n" +
			"that consistently runs four hours is not a two-hour cadence — it is a\n" +
			"four-hour cadence with a misleading label, and only the record tells you\n" +
			"which one you have. Tune the next `--for` from this, not from intent.\n\n" +
			"Stopping does not move the sprint's column: the clock and the work are\n" +
			"different questions, and a box can close with the work unfinished. That is\n" +
			"a normal outcome and worth seeing as one.",
		Example: "  bashy sprint stop 3 --gate 'go build ./... && go test ./...'\n" +
			"  bashy sprint stop 3 --gate 'make test' --note \"paused for the incident\"\n" +
			"  bashy sprint stop 3 --no-verify        # nothing to gate; on the record",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			return runSprintOwnerLifecycle(cmd, &flags, id, op, "close managed sprint owner", func() error {
				now := time.Now().UTC()
				if gateTimeout <= 0 {
					return fmt.Errorf("gate timeout must be positive")
				}
				var rep drainReport
				var closedOwner string
				cwd, _ := os.Getwd()
				var reclaimed string
				if ending {
					// ZERO RESIDUE, IN THREE STEPS, OUTSIDE THE LOCK. (1) every
					// linked run must have a decision; (2) the settled ones come
					// down through the one guarded teardown; (3) the mutation below
					// looks again and refuses if anything sprint-owned is left.
					var refusal error
					var reclaimFailures []string
					if err := weaveStoryRead(cmd, &flags, op, id, func(s *weaveStory) {
						if undisposed := sprintUndisposedRuns(s); len(undisposed) > 0 {
							refusal = fmt.Errorf("sprint #%d cannot end — %d linked run(s) have no disposition:\n  %s",
								id, len(undisposed), strings.Join(undisposed, "\n  "))
							return
						}
						reclaimed, reclaimFailures = sprintReclaimSummary(sprintPruneRunArtifacts(s))
					}); err != nil {
						return err
					}
					if refusal != nil {
						return refusal
					}
					if len(reclaimFailures) > 0 {
						return fmt.Errorf("sprint #%d cannot end — cleanup failed:\n  %s", id, strings.Join(reclaimFailures, "\n  "))
					}
				}
				err = runWeaveStoryMutate(cmd, id, op, &flags, func(s *weaveStory) (string, error) {
					b := s.currentBox()
					unboxedEnd := ending && len(s.Boxes) == 0
					if b == nil && !unboxedEnd {
						// Saying "stopped" about a sprint that was never running
						// would be a small lie of exactly the kind this feature is
						// meant to remove.
						return "", fmt.Errorf("sprint #%d has no running box — `sprint start %d` opens one", id, id)
					}
					// PARK THE WORKERS FIRST. Whatever the gate says next, nothing
					// should still be writing to the tree while it is judged — a
					// gate racing a live agent measures neither.
					if b != nil && b.Draining == nil {
						b.Draining = &now
					}
					paused, problems := pauseLinkedRepos(s)
					rep.Paused = paused
					rep.Failures = problems

					// THE MINIMUM BAR: it compiles and the tests pass. A sprint
					// that stops over a regression is not parked, it is a trap —
					// the next sprint inherits damage it cannot tell from its own.
					if strings.TrimSpace(gateCmd) != "" && !flags.quietF && !flags.jsonF {
						fmt.Fprintf(cmd.ErrOrStderr(), "%s: running gate (timeout %s): %s\n", op, gateTimeout, gateCmd)
					}
					gateCtx, cancelGate := context.WithTimeout(cmd.Context(), gateTimeout)
					out := runDrainGate(gateCtx, gateDir, gateCmd)
					cancelGate()
					rep.GateRan, rep.GatePassed, rep.GateCmd = out.Ran, out.Passed, out.Command
					if b != nil {
						b.GateRan, b.GatePassed, b.GateCmd = out.Ran, out.Passed, out.Command
					}

					// CLOSING CONDITIONS: committed, pushed, pinned. A green gate
					// says the code works; it says nothing about whether the work
					// was PUT anywhere the next sprint will find it.
					var repoPath = func(run sprintRun) (string, bool) {
						dir, err := weaveQueueDirForSprintRun(run)
						if err != nil {
							return "", false
						}
						return weaveRepoRootForQueue(dir)
					}
					repos := checkClosingConditions(s, currentBoard, repoPath)
					rep.Repos = repos
					var unclean []string
					for i := range repos {
						if !repos[i].OK() {
							unclean = append(unclean, repos[i].Describe())
						}
					}

					if !force {
						if len(unclean) > 0 {
							return "", fmt.Errorf("sprint #%d NOT stopped — the repos are not wrapped up:\n  %s\n"+
								"  commit, push, and bump pins, then `sprint stop %d` again (--force records it unwrapped)",
								id, strings.Join(unclean, "\n  "), id)
						}
						if len(problems) > 0 {
							return "", fmt.Errorf("sprint #%d NOT stopped — could not park: %s\n  fix, then `sprint stop %d` again (or --force to close anyway)",
								id, strings.Join(problems, "; "), id)
						}
						if out.Ran && !out.Passed {
							// The refusal is the feature. Workers are parked, so
							// nothing is burning; the remaining job is narrow.
							tail := out.Output
							if len(tail) > 600 {
								tail = tail[len(tail)-600:]
							}
							return "", fmt.Errorf("sprint #%d NOT stopped — the gate FAILED, so this is not a good handoff state.\n"+
								"  workers are parked; fix the regression, then `sprint stop %d` again.\n"+
								"  gate: %s (exit %d)\n%s",
								id, id, out.Command, out.ExitCode, tail)
						}
					}

					msg := ""
					if unboxedEnd {
						msg = "ended without a recorded time-box; " + drainEvidenceSummary(&rep)
					} else {
						b.StoppedAt = &now
						elapsed := b.Elapsed(now)
						verdict := "within the box"
						if elapsed > b.Planned {
							verdict = fmt.Sprintf("OVER by %s", roundDur(elapsed-b.Planned))
						} else if d := b.Planned - elapsed; d > time.Minute {
							verdict = fmt.Sprintf("under by %s", roundDur(d))
						}
						msg = fmt.Sprintf("stopped after %s (planned %s) — %s; %s",
							roundDur(elapsed), roundDur(b.Planned), verdict, drainSummary(&rep, elapsed, b.Planned))
					}
					if strings.TrimSpace(note) != "" {
						msg += ": " + strings.TrimSpace(note)
					}
					// STOP PARKS THE WORK; IT DOES NOT VACATE THE SEAT.
					//
					// A manager told to "stop everything and wrap up" runs this,
					// reads success, and reports done — while still holding the
					// lease, so the next manager's take is refused and the operator
					// has to diagnose it. Naming the remaining step here is the
					// difference between a wrap-up and a half-finished handover.
					if !ending && s.Lease != nil && strings.TrimSpace(s.Lease.Holder) != "" {
						msg += "\n  the SEAT is still yours — `bashy sprint handoff " +
							strconv.FormatInt(id, 10) + " -m '<where it stands>'` releases it " +
							"so another manager can take over; keep it if you are resuming"
					}
					closedOwner = strings.TrimSpace(s.Owner)
					if ending {
						// "done" must mean done. End deliberately carries no
						// --force, so open stories and dirty repositories remain
						// HARD refusals. Test/review artifacts are optional; when no
						// gate was supplied the lifecycle records UNVERIFIED rather
						// than manufacturing a pass.
						if err := sprintUnansweredGate(s, "end"); err != nil {
							return "", err
						}
						if err := sprintStoryClosureAudit(s); err != nil {
							return "", err
						}
						if err := sprintCoverageGate(s, false, ""); err != nil {
							return "", err
						}
						if hy := sprintCheckHygiene(s); !hy.Clean() {
							return "", fmt.Errorf(
								"sprint #%d cannot end — it is not clean:\n  %s\n  `bashy sprint prune %d` for the full state and the command that fixes each",
								id, strings.Join(hy.Problems, "\n  "), id)
						}
						if residual := sprintResidual(s); len(residual) > 0 {
							return "", fmt.Errorf(
								"sprint #%d cannot end — %d sprint-owned artifact(s) remain after cleanup:\n  %s",
								id, len(residual), strings.Join(residual, "\n  "))
						}
						msg += "; " + reclaimed + "; residual: 0"
						from := s.Column
						who := weaveStoryConductorName(s, "")
						s.Column = "done"
						_ = closeSprintRoom(s, who)
						s.Lease = nil
						msg += fmt.Sprintf("; lifecycle ended (%s → done), conductor lease released", from)
						weaveStoryAppend(s, who, kindStage, msg)
						// Ending is irreversible and closes the card, so if the
						// seat could not be woken by mail the reader should be
						// told here too — see sprintSeatDeliveryAdvisory.
						return fmt.Sprintf("sprint #%d %s%s", id, msg,
							sprintSeatDeliveryAdvisory(who)), nil
					}
					weaveStoryAppend(s, weaveConductorName(""), kindStage, msg)
					return fmt.Sprintf("sprint #%d %s", id, msg), nil
				})
				if err != nil {
					return err
				}
				if sessionNote := releaseSprintOwnerSession(cmd.Context(), id, closedOwner, cwd); sessionNote != "" {
					fmt.Fprintf(cmd.ErrOrStderr(), "%s: %s\n", op, strings.TrimPrefix(sessionNote, "; "))
				}
				return nil
			})
		},
	}
	if ending {
		cmd.Long = `end closes the sprint lifecycle only after it establishes a clean handoff state.

It parks every linked working agent in a resumable weave state, refuses missing
or half-allocated linked runs, verifies linked repositories are committed,
pushed, and pinned, and requires every story to be closed. When --gate is
supplied it must pass; without one, end succeeds and records the lifecycle as
unverified. It then closes the time box, moves the card to done, closes the
conductor room, and releases the lease.

There is deliberately no --force. Use sprint stop when the intent is only to
close the current cadence cycle.`
		cmd.Example = "  bashy sprint end 3\n" +
			"  bashy sprint end 3 --gate 'go test ./...'\n" +
			"  bashy sprint end 3 --gate 'make test' --note \"release accepted\""
	}
	cmd.Flags().StringVar(&note, "note", "", "what the box actually produced")
	cmd.Flags().StringVar(&gateCmd, "gate", "", "the command proving the tree still builds and passes")
	cmd.Flags().StringVar(&gateDir, "gate-dir", "", "where to run the gate (default: cwd)")
	cmd.Flags().DurationVar(&gateTimeout, "gate-timeout", 15*time.Minute, "maximum time allowed for the gate")
	if !ending {
		cmd.Flags().BoolVar(&force, "force", false, "close even over a red gate or an unparked worker — recorded as not clean")
		cmd.Flags().BoolVar(&noVerify, "no-verify", false, "deprecated compatibility flag; omitting --gate already records an unverified close")
	}
	flags.attach(cmd)
	return cmd
}

// newSprintExtendCmd lengthens a running box WITHOUT rewriting the original
// commitment.
//
// Extending exists so that the honest move — "this needs longer" — is one
// command, rather than something people route around by restarting the box and
// quietly losing the first estimate. Planned is deliberately left alone: the
// gap between what was promised and what was taken is the whole signal.
func newSprintExtendCmd() *cobra.Command {
	var flags weaveOutputFlags
	var by time.Duration
	cmd := &cobra.Command{
		Use:     "extend <sprint>",
		Short:   "Push a running sprint's cutoff back, keeping the original estimate on record",
		Example: "  bashy sprint extend 3 --by 30m",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := strconv.ParseInt(args[0], 10, 64)
			if err != nil {
				return fmt.Errorf("sprint must be an integer: %q", args[0])
			}
			if by <= 0 {
				return fmt.Errorf("--by must be positive (got %s)", by)
			}
			now := time.Now().UTC()
			return runWeaveStoryMutate(cmd, id, "sprint extend", &flags, func(s *weaveStory) (string, error) {
				b := s.currentBox()
				if b == nil {
					return "", fmt.Errorf("sprint #%d has no running box to extend", id)
				}
				b.Cutoff = b.Cutoff.Add(by)
				weaveStoryAppend(s, weaveConductorName(""), "system",
					fmt.Sprintf("extended by %s (original estimate %s stands)", roundDur(by), roundDur(b.Planned)))
				return fmt.Sprintf("sprint #%d extended by %s — %s", id, roundDur(by), b.Status(now)), nil
			})
		},
	}
	cmd.Flags().DurationVar(&by, "by", 30*time.Minute, "how much longer")
	flags.attach(cmd)
	return cmd
}

// newSprintStatusCmd is THE STEWARD'S VIEW: every sprint on this host at once,
// answered as a set rather than one card at a time.
//
// The kanban is organised for the person doing one thing — it groups by column,
// which is where a sprint is. A steward asks a different question, and it is
// always about the whole: what is on the clock right now, what has run past
// what it promised, and what has been left open. Those sprints are scattered
// across columns by construction, so the board can show them and still not
// answer it.
//
// It reads, and changes nothing. A steward deciding to stop or extend does that
// deliberately, per sprint; a status view that also acted would make the survey
// and the intervention the same keystroke.
// managerAgent is a lease holder JOINED to the fleet record that says what it
// IS. The sprint stores a NAME and only a name — deliberately, because pinning
// a binding into the sprint would rot the moment the agent is re-bound — so the
// binding is resolved at read time from the catalog that owns it.
//
// An UNRESOLVABLE holder is a FINDING, not a blank. A sprint whose manager is
// not in the fleet is precisely the state an operator must see: the name was
// mistyped, the agent was removed, or the seat was taken by something nothing
// can push to. The row keeps its name and says the join failed.
type managerAgent struct {
	Name     string `json:"name"`
	Resolved bool   `json:"resolved"`
	Tool     string `json:"tool,omitempty"`
	Model    string `json:"model,omitempty"`
	Binding  string `json:"binding,omitempty"`
	Band     int    `json:"band,omitempty"`
	Nick     string `json:"nick,omitempty"`
	Reason   string `json:"reason,omitempty"`
}

// resolveManager is a CATALOG READ and nothing more.
//
// It must not probe: `sprint tick` already states the cost — a probe is a real
// headless turn per row, and installed is NOT signed in. It must not write:
// `sprint status` reports and changes nothing, so resolution can never become a
// liveness signal for a seat nobody is driving.
func resolveManager(cat *fleet.Catalog, name string) *managerAgent {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil
	}
	m := &managerAgent{Name: name}
	a, ok := cat.Agent(name)
	if !ok {
		m.Reason = "not in the fleet — `bashy agent list` does not name it"
		return m
	}
	m.Resolved = true
	m.Tool, m.Model, m.Binding, m.Nick = a.Tool, a.Model, a.MatrixKey(), a.NickName()
	// The band is the MODEL's peg, inherited — an agent never carries its own,
	// except a cascade, whose served band is its contract.
	if a.BandSource == fleet.BandCascade && a.Band > 0 {
		m.Band = a.Band
	} else if _, _, mod, err := cat.Binding(a.Name); err == nil {
		m.Band = mod.Band
	}
	return m
}

// label renders the join for the text view, in one short parenthetical that
// says which of the three states this is.
func (m *managerAgent) label() string {
	if m == nil {
		return ""
	}
	if !m.Resolved {
		return "  (UNRESOLVED: " + m.Reason + ")"
	}
	parts := m.Binding
	if m.Band > 0 {
		parts += " L" + strconv.Itoa(m.Band)
	}
	if m.Nick != "" && !strings.EqualFold(m.Nick, m.Name) {
		parts += " · " + m.Nick
	}
	return "  (" + parts + ")"
}

func newSprintStatusCmd() *cobra.Command {
	var flags weaveOutputFlags
	// WHY A FLAG IN TEXT AND ALWAYS-ON IN JSON. The envelope is consumed by
	// tools, where an extra object is free and additive. The text view is
	// width-constrained and the holder line already carries a name, a liveness
	// mark and a contact string; a binding on every row by default would push
	// the contact off a normal terminal. So JSON always answers "what is this
	// manager", and the terminal answers it when asked.
	var withAgents bool
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Every sprint on this host: what is on the clock, what is over, what is idle",
		Long: "status is the steward's cross-sprint view — the whole host in one answer.\n\n" +
			"The board groups by kanban column, which is where each sprint IS. This\n" +
			"groups by clock, which is what a steward is accountable for: several\n" +
			"initiatives run at once on different cadences, and the ones needing a\n" +
			"decision are scattered across columns where no single column shows them.\n\n" +
			"It reports and changes nothing — stopping or extending stays a deliberate\n" +
			"act on a named sprint.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode := flags.mode()
			dir, err := weaveStoryDir(cmd, mode, "sprint status")
			if err != nil {
				return err
			}
			q, lerr := loadWeaveQueue(dir)
			if lerr != nil {
				return ec(weavecli.EmitError(cmd.ErrOrStderr(), mode, "sprint status", weavecli.ExitGenericFail, lerr))
			}
			now := time.Now().UTC()

			type row struct {
				ID      int64  `json:"id"`
				Title   string `json:"title"`
				Epic    string `json:"epic,omitempty"`
				Column  string `json:"column"`
				Status  string `json:"box_status,omitempty"`
				Cycles  int    `json:"cycles,omitempty"`
				Overdue bool   `json:"overdue,omitempty"`
				Holder  string `json:"lease_holder,omitempty"`
				Contact string `json:"contact,omitempty"`
				Stale   bool   `json:"lease_stale,omitempty"`
				// Agent is the holder joined to its fleet record; nil when the
				// sprint is unowned. Always present in JSON, so a consumer never
				// has to make a second lookup per row.
				Agent *managerAgent `json:"agent,omitempty"`
			}
			// ONE catalog for the whole sweep. Building it per row would re-read
			// every asset file once per sprint.
			cat := fleet.New()
			// ONE CONDUCTOR IS ACCOUNTABLE FOR EVERY IN-PROGRESS SPRINT — that
			// is the rule `start` enforces by claiming the lease. But a lease
			// is a heartbeat with a TTL, so it can go STALE while the box is
			// still running: the conductor died (SIGKILL, token exhaustion,
			// a closed laptop) and delivery is now nobody's.
			//
			// That state is invisible if it is filed under "running" — it
			// looks exactly like healthy work. It is the steward's most
			// actionable signal, because it is the only one where nothing will
			// improve until a person acts: an overdue sprint at least has
			// someone to make the call.
			var onClock, over, idle, done, orphaned []row
			for _, s := range q.Stories {
				h, stale, free := weaveStoryLeaseState(s)
				if free {
					h = ""
				}
				r := row{ID: s.ID, Title: s.Title, Epic: s.Epic, Column: s.Column,
					Status: s.lastBox().Status(now), Cycles: len(s.Boxes), Contact: s.Contact.String(), Overdue: s.currentBox().Overdue(now), Holder: h, Stale: stale}
				r.Agent = resolveManager(cat, h)
				switch {
				case s.currentBox().Running() && (stale || free):
					orphaned = append(orphaned, r)
				case s.currentBox().Overdue(now):
					over = append(over, r)
				case s.currentBox().Running():
					onClock = append(onClock, r)
				case len(s.Boxes) > 0:
					// Stopped, but it HAS run — a distinct state from never
					// started. A steward restarting work looks here, and a
					// sprint whose cycles keep ending short is visible only
					// from this group.
					done = append(done, r)
				case s.Column != "done":
					idle = append(idle, r)
				}
			}
			sortRows := func(rs []row) { sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID }) }
			sortRows(onClock)
			sortRows(over)
			sortRows(idle)
			sortRows(done)
			sortRows(orphaned)

			if mode == weavecli.OutputJSON {
				return ec(emitOK(cmd.OutOrStdout(), mode, "sprint status", map[string]any{
					"now": now, "overdue": over, "on_clock": onClock, "idle": idle, "stopped": done, "unowned_delivery": orphaned,
					"resources": sprintResources(cmd, 0),
				}))
			}

			out := cmd.OutOrStdout()
			renderSprintResources(out, sprintResources(cmd, 0))
			line := func(r row) {
				lease := "  [unowned]"
				if r.Holder != "" {
					mark := "✓"
					if r.Stale {
						mark = "STALE"
					}
					lease = fmt.Sprintf("  [%s %s]", r.Holder, mark)
					if withAgents {
						lease += r.Agent.label()
					}
				}
				st := ""
				if r.Status != "" {
					st = "  " + r.Status
				}
				cyc := ""
				if r.Cycles > 1 {
					// The count is the tell that a sprint keeps being picked up
					// and put down — which a single planned-vs-actual cannot show.
					cyc = fmt.Sprintf("  ×%d", r.Cycles)
				}
				contact := ""
				if r.Contact != "" {
					contact = "  → " + r.Contact
				}
				fmt.Fprintf(out, "  #%d %s (%s)%s%s%s%s\n", r.ID, weaveTruncate(r.Title, 40), r.Column, st, cyc, lease, contact)
			}
			// SWEEP BEFORE REPORTING. A room whose holder is dead advertises a
			// channel to somebody who will never read it, and an unanswered
			// room costs more than an absent one — it consumes the time of
			// whoever trusted it. No verb can cover this case: the holder died
			// and ran nothing.
			if swept := sweepDeadRooms(q.Stories, now, currentActor()); len(swept) > 0 {
				fmt.Fprintf(out, "swept %d abandoned room(s): %s\n\n", len(swept), strings.Join(swept, ", "))
			}

			// Unowned delivery first: a running sprint with no live conductor
			// is the one state that cannot resolve itself.
			if len(orphaned) > 0 {
				fmt.Fprintf(out, "RUNNING, NO LIVE CONDUCTOR (%d) — delivery is unowned; `sprint take <id>`\n", len(orphaned))
				for _, r := range orphaned {
					line(r)
				}
				fmt.Fprintln(out)
			}
			if len(over) > 0 {
				fmt.Fprintf(out, "PAST CUTOFF (%d) — the conductor named on each decides: stop, or extend\n", len(over))
				for _, r := range over {
					line(r)
				}
				fmt.Fprintln(out)
			}
			fmt.Fprintf(out, "ON THE CLOCK (%d)\n", len(onClock))
			if len(onClock) == 0 {
				fmt.Fprintln(out, "  — nothing running; `sprint start <id> --owner NAME --for <dur>`")
			}
			for _, r := range onClock {
				line(r)
			}
			if len(idle) > 0 {
				fmt.Fprintf(out, "\nOPEN, NOT ON THE CLOCK (%d)\n", len(idle))
				for _, r := range idle {
					line(r)
				}
			}
			if len(done) > 0 {
				fmt.Fprintf(out, "\nSTOPPED — restartable (%d)\n", len(done))
				for _, r := range done {
					line(r)
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&withAgents, "agents", false,
		"resolve each manager to its fleet record (tool:model, band); always present in --json")
	flags.attach(cmd)
	return cmd
}
