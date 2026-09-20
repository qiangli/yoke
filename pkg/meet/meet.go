package meet

import (
	"bufio"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/qiangli/yoke/pkg/bus"
	"github.com/qiangli/yoke/pkg/capability"
	"github.com/qiangli/yoke/pkg/kb"
	"github.com/qiangli/yoke/pkg/role"
	"github.com/qiangli/yoke/pkg/room"
	"github.com/qiangli/yoke/pkg/scope"
	"github.com/spf13/cobra"
)

// referenceMD is the full `bashy meet` guide, embedded into the binary so it is
// available wherever bashy runs (agents use bashy as a tool, not from this repo).
// Surfaced by `bashy meet reference`.
//
//go:embed reference.md
var referenceMD string

// meetDepthEnv bounds recursion. `meet` spawns agent CLIs, and those agents can
// call `bashy meet` themselves — so a panelist could convene a panel, whose
// panelists convene panels, forking exponentially and unboundedly. The depth
// marker is exported into every spawned agent's environment; convening from
// inside a meeting is refused.
const meetDepthEnv = "BASHY_MEET_DEPTH"

func meetDepth() int {
	n, _ := strconv.Atoi(strings.TrimSpace(os.Getenv(meetDepthEnv)))
	return n
}

// guardDepth refuses to convene a meeting from inside one.
func guardDepth() error {
	if d := meetDepth(); d >= 1 {
		return fmt.Errorf("meet: refusing to convene a meeting from inside a meeting (%s=%d).\n"+
			"      A participant must contribute a turn, not convene its own panel — that recursion is unbounded.\n"+
			"      If you are an agent that needs a second opinion, say so in your turn and let the facilitator decide", meetDepthEnv, d)
	}
	return nil
}

// markDepth stamps the environment inherited by every agent this process spawns.
func markDepth() { _ = os.Setenv(meetDepthEnv, strconv.Itoa(meetDepth()+1)) }

// NewMeetCmd returns the `bashy meet` command tree.
func NewMeetCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meet",
		Short: "multi-participant deliberation session with a notes-only secretary",
		Long: "Run a turn-taking planning meeting across agentic CLIs and a human.\n" +
			"A dedicated notes-only secretary keeps the minutes and files them to\n" +
			"docs/meetings/ on close. Agents can convene a one-shot panel with\n" +
			"`bashy meet consult`. Run `bashy meet reference` for the full guide.\n" +
			"'meet observe' tails one transcript; 'bashy inbox --watch' follows actionable input across channels.",
	}
	cmd.CompletionOptions.DisableDefaultCmd = true
	cmd.AddCommand(
		newOpenCmd(), newConsultCmd(), newDMCmd(), newReadCmd(), newTellCmd(), newDispatchCmd(), newRoundCmd(),
		newPollCmd(), newAskCmd(), newInviteCmd(), newKickCmd(),
		newConvergeCmd(), newCloseCmd(), newAbandonCmd(), newAmendCmd(), newApplyCmd(),
		newShowCmd(), newContributionsCmd(), newListCmd(), newResumeCmd(), newReferenceCmd(),
		newObserveCmd(), newServeCmd(), newServiceCmd(), newSayCmd(),
	)
	return cmd
}

func newReferenceCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "reference",
		Short: "print the full bashy meet guide (embedded in the binary)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			fmt.Fprint(cmd.OutOrStdout(), referenceMD)
			return nil
		},
	}
}

func humanName() string {
	if u := strings.TrimSpace(os.Getenv("USER")); u != "" {
		return u
	}
	return "you"
}

func drivability(name string) string {
	// Shared operability gate with the capability router (pkg/capability): the
	// codex login-shell caveat is surfaced by shellRouting below, not here.
	// The name is resolved to its harness first — a participant seated by
	// nickname is still driven by a binary, and LookPath does not know the
	// nickname.
	if ok, _ := capability.Operable(capability.ResolveTool(name)); !ok {
		return "not installed"
	}
	return "installed"
}

// shellRouting reports whether a participant's shell commands will run through
// bashy. The chat launcher force-injects SHELL/CLAUDE_CODE_SHELL + a PATH shim
// into every spawned agent (on unless BASHY_FORCE_AGENT_SHELL=0), which covers
// every agent EXCEPT codex — it reads the /etc/passwd login shell, not the
// environment, so it needs `bashy install-agent codex` (chsh) instead.
func shellRouting(name string) string {
	if os.Getenv("BASHY_FORCE_AGENT_SHELL") == "0" {
		return "shell: system (forcing disabled)"
	}
	// Match on the harness, not on what the seat was called. `codex`,
	// `codex-gpt-5.5`, and the nickname drawn for that binding are all the
	// same binary reading the same /etc/passwd — a caveat that only fires for
	// one spelling of the name is a caveat that does not fire.
	if capability.ResolveTool(name) == "codex" {
		return "shell: ⚠ codex reads /etc/passwd — run `bashy install-agent codex` (chsh) to route via bashy"
	}
	return "shell: bashy ✓ (env-forced)"
}

func printPreview(w io.Writer, st *State) {
	fmt.Fprintln(w, "meet: resolved session")
	fmt.Fprintf(w, "  id           %s\n", st.ID)
	if st.Room > 0 {
		fmt.Fprintf(w, "  room         %d   ← `bashy meet observe %d` to watch it live\n", st.Room, st.Room)
	}
	fmt.Fprintf(w, "  initiator    %s\n", st.initiatorLabel())
	if st.recorded() {
		fmt.Fprintf(w, "  secretary    %s  records only, decides nothing — %s · %s\n",
			st.secretary(), drivability(st.secretary()), shellRouting(st.secretary()))
	} else {
		fmt.Fprintln(w, "  secretary    (none — this room keeps no minutes; nothing extracts its decisions)")
	}
	if st.chaired() {
		fmt.Fprintf(w, "  facilitator  %s  directs, never argues — %s · %s\n", st.chair(), drivability(st.chair()), shellRouting(st.chair()))
	} else {
		fmt.Fprintln(w, "  facilitator  (none — round-robin; the human directs)")
	}
	for i, p := range st.Participants {
		label := "participants"
		if i > 0 {
			label = "            "
		}
		fmt.Fprintf(w, "  %s %s  %s · %s\n", label, seatLabel(p), drivability(p), shellRouting(p))
	}
	if len(st.Participants) == 0 {
		fmt.Fprintln(w, "  participants (none)")
	}
	fmt.Fprintf(w, "  human        %s\n", st.Human)
	if len(st.Context) > 0 {
		fmt.Fprintf(w, "  context      %s\n", strings.Join(st.Context, ", "))
	}
	dir, _ := storeDir(st.ID)
	fmt.Fprintf(w, "  store        %s/\n", redactHome(dir))
	fmt.Fprintf(w, "  minutes →    %s\n", redactHome(minutesPath(st)))
	fmt.Fprintf(w, "  turn model   %s · decisions=%s\n", st.turnModel(), st.decisionMode())
	for _, warn := range attendeeWarnings(st) {
		fmt.Fprintf(w, "  ⚠ %s\n", warn)
	}
}

// attendeeWarnings applies the meet attendee gate (see
// dhnt/docs/agentic-capability-taxonomy.md §meet attendee requirements): flag
// non-routable participants (operability) and a roster past the Self-MoA sweet
// spot (diversity). Advisory — it warns, it does not refuse.
func attendeeWarnings(st *State) []string {
	var out []string
	for _, p := range st.Participants {
		if ok, reason := capability.Operable(capability.ResolveTool(p)); !ok {
			out = append(out, fmt.Sprintf("%s is not routable: %s", p, reason))
		}
	}
	if n := len(st.Participants); n > 4 {
		out = append(out, fmt.Sprintf("%d participants exceeds the 2–4 Self-MoA sweet spot — trim redundant seats (same tool/model dilutes signal)", n))
	}
	return out
}

// sessionFlags are shared by `start` and `consult`.
type sessionFlags struct {
	topic        string
	secretary    string
	chair        string
	out          string
	turnTimeout  string
	decisionMode string
	initiator    string
	// human is who occupies the room's HUMAN seat. It is not a flag — `start` and
	// `consult` leave it empty and get the OS user, which is the only identity a
	// terminal has. A transport that authenticated its caller sets it, because
	// through the tunnel the person opening the room is a cloudbox account and the
	// OS user is merely whoever the server happens to run as. See Create.
	human        string
	minTurnChars int
	maxTurns     int
	maxStalls    int
	minBand      int
	steerable    bool
	board        bool
	shared       bool // --shared: attendees may be <name>@<host> on another host
	participants []string
	agenda       []string
	context      []string
	rosterNotes  []string // what --min-band seated, and what it could not
}

func (sf *sessionFlags) bind(cmd *cobra.Command) {
	f := cmd.Flags()
	f.StringVar(&sf.topic, "topic", "", "meeting topic (required)")
	f.StringArrayVar(&sf.participants, "participant", nil, "participant agent — decides content (repeatable)")
	f.IntVar(&sf.minBand, "min-band", 0, "seat every operable agent at this capability band or above (1-4), instead of naming them")
	f.StringVar(&sf.secretary, "secretary", "claude",
		"secretary agent — records, decides nothing; never a participant or the facilitator. "+
			"Pass --secretary \"\" for a room that keeps no minutes: a conversation rather than a meeting")
	// ONE FLAG, DOMAIN TITLES. A meeting's owner is its FACILITATOR: the seat
	// that directs the discussion, judges done-ness, and answers for the room.
	role.AttachOwner(f, &sf.chair, role.Facilitator,
		"directs the discussion, judges done-ness, and answers for an open meeting")
	f.StringArrayVar(&sf.agenda, "agenda", nil, "agenda item (repeatable)")
	f.StringArrayVar(&sf.context, "context", nil, "file every participant reads before its first turn (repeatable)")
	f.IntVar(&sf.maxTurns, "max-turns", defaultMaxTurns, "hard ceiling on participant turns under a facilitator")
	f.IntVar(&sf.maxStalls, "max-stalls", defaultMaxStalls, "consecutive looping/no-progress turns before the facilitator re-plans")
	f.StringVar(&sf.out, "out", "docs", "filing target: docs | kb | <path>")
	f.StringVar(&sf.turnTimeout, "turn-timeout", "20m", "per-turn agent timeout (e.g. 20m); a wedged agent can't hang the round")
	f.StringVar(&sf.decisionMode, "decision-mode", "infer", "infer: the secretary may record a converged decision (tagged); explicit: only stated decisions")
	f.StringVar(&sf.initiator, "initiator", "", "who convened the meeting and must confirm it may end; must be someone at the table (default: the human)")
	f.IntVar(&sf.minTurnChars, "min-turn-chars", 0, "a reply shorter than N chars counts as `short`, not a contribution")
	f.BoolVar(&sf.steerable, "steerable", false,
		"hold each speaker OPEN for its whole turn, so `meet say` reaches it mid-answer. "+
			"Without this a turn is a headless one-shot: it runs the prompt and exits, so a steer arrives "+
			"after the agent is already gone. Costs a TUI startup and a silence timeout per turn — a live "+
			"turn has no exit to end it, so it ends on quiet")
}

// humanSeat resolves who takes the human seat: whoever the caller authenticated,
// falling back to the OS user.
//
// The fallback is not a default identity so much as the only one a terminal can
// offer — on loopback the caller has already proved the single thing loopback
// proves, that it is the machine owner. It is deliberately NOT applied when a
// name was supplied: silently substituting the OS user for an authenticated
// account would file the room under the wrong person and hand them the
// organizer privilege that goes with it.
func (sf *sessionFlags) humanSeat() string {
	if h := strings.TrimSpace(sf.human); h != "" {
		return h
	}
	return humanName()
}

func (sf *sessionFlags) newState() (*State, error) {
	for _, f := range sf.context {
		if _, err := os.Stat(f); err != nil {
			return nil, fmt.Errorf("meet: --context %s: %w", f, err)
		}
	}
	// A board keeps no minutes and spawns no secretary — the default --secretary
	// "claude" is a meeting default, not a board one. Clear it before the roster
	// is canonicalized/routability-checked so a board never seats or arms one.
	if sf.board {
		sf.secretary = ""
	}
	// Seating happens before Validate, so a band-built roster is held to the
	// same rules as one someone typed out.
	if err := sf.seatByBand(); err != nil {
		return nil, err
	}
	// And every seat is resolved to its canonical name before Validate, so the
	// duplicate check sees through aliases: --participant Sable --participant
	// claude-fable5 is ONE agent seated twice, and seating it twice would
	// dilute the vote while looking like diversity.
	sf.canonicalizeRoster()
	// A registered PERSON, or in a shared board a colleague's seat on another
	// host (`<name>@<host>`), is an ATTENDEE — attributed and addressable,
	// never scheduled — exactly what `meet invite` seats it as. Creation used
	// to refuse both ("not a registered agent") while the --shared help said
	// to invite colleagues by address (sprint 220, story ad5b0de9).
	attendees := sf.splitAttendees()
	// And every seat must be one this host can actually drive. `meet invite`
	// has always checked; creation did not, so any name at all could be seated
	// and then record a failed turn every round for a participant that was
	// never real.
	if err := sf.routableRoster(); err != nil {
		return nil, err
	}
	cwd, _ := os.Getwd()
	st := &State{
		ID: newID(sf.topic, nowFn()), Room: assignRoom(), Topic: sf.topic, Agenda: sf.agenda,
		Participants: sf.participants, Secretary: sf.secretary, Chair: sf.chair,
		Observers:    attendees,
		Human:        sf.humanSeat(),
		Initiator:    sf.initiator,
		DecisionMode: sf.decisionMode, MinTurnChars: sf.minTurnChars, Context: sf.context,
		Steerable: sf.steerable, Board: sf.board,
		MaxTurns: sf.maxTurns, MaxStalls: sf.maxStalls,
		Status: "open", Cwd: cwd, Out: sf.out,
		TurnTimeout: sf.turnTimeout, Created: nowFn(),
	}
	if err := st.Validate(); err != nil {
		return nil, err
	}
	return st, nil
}

// deliberate runs the discussion under whichever turn model the roster implies,
// so `start --non-interactive` and `consult` share one path. There is no mode
// flag: an agent chair runs the ledger loop, no chair runs a round-robin.
func deliberate(ctx context.Context, st *State, w io.Writer, rounds int, question string, verbose bool) error {
	if st.chaired() {
		if rounds > 1 {
			// --rounds is a round-robin control. Under a chair the ledger loop
			// decides how many turns to run, so honouring it is impossible —
			// say so rather than silently running a shorter meeting than asked.
			fmt.Fprintf(w, "meet: ⚠ --rounds %d ignored — %s is facilitating, and the facilitator decides "+
				"turn count (bounded by --max-turns %d). Drop --owner for round-robin rounds.\n",
				rounds, st.chair(), st.MaxTurns)
		}
		res, err := runChaired(ctx, st, nil)
		if err != nil {
			return err
		}
		if verbose {
			fmt.Fprintf(w, "facilitated: %d turns, %d stalls, %d re-plans, %d degraded selections — stopped by %s\n",
				res.Turns, res.Stalls, res.Replans, res.Degraded, res.StoppedBy)
		}
		if res.StoppedBy == "stalled" {
			fmt.Fprintln(w, "⚠ the meeting stalled — participants stopped adding anything new")
		}
		return nil
	}
	for i := 0; i < rounds; i++ {
		q := question
		if q == "" && i < len(st.Agenda) {
			q = st.Agenda[i]
		}
		evs, err := runRound(ctx, st, q, nil)
		if err != nil {
			return err
		}
		for _, e := range evs {
			if verbose {
				fmt.Fprintf(w, "%s> %s\n", e.Speaker, oneLine(e.Text))
			}
		}
	}
	return nil
}

func newOpenCmd() *cobra.Command {
	var sf sessionFlags
	var rounds int
	var dry, nonInteractive, yes bool
	var fromMB string
	cmd := &cobra.Command{
		Use:   "open [<room>|<id>] [--topic TEXT --participant AGENT ...]",
		Short: "open a meeting (enters the REPL unless --non-interactive)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if len(args) == 1 {
				if strings.TrimSpace(sf.topic) != "" || len(sf.participants) > 0 {
					return fmt.Errorf("meet: reopen a room by reference or create one with --topic/--participant, not both")
				}
				st, err := Open(args[0], humanName())
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "opened %s in room %d\n", st.ID, st.Room)
				return nil
			}
			// These flags only govern an unattended deliberation. They used to be
			// silently ignored in the default interactive mode, so a command that
			// explicitly said "one round, two turns, twenty minutes per turn" could
			// sit in the REPL forever. Require the mode bit rather than guessing that
			// a bounded-looking invocation meant to be interactive.
			if !nonInteractive && !dry {
				if flags := changedUnattendedOpenFlags(cmd); len(flags) > 0 {
					return fmt.Errorf("meet open: %s only controls unattended deliberation; pass --non-interactive or omit the bounded flags to enter the REPL", strings.Join(flags, ", "))
				}
			}
			if err := guardDepth(); err != nil {
				return err
			}
			seqs, err := parseSeqList(fromMB)
			if err != nil {
				return err
			}
			if len(seqs) > 0 && !sf.board {
				return fmt.Errorf("meet: --from-mb seeds a board; pass --board")
			}
			if strings.TrimSpace(sf.initiator) == "" {
				sf.initiator = humanName() // `start` always names its initiator
			}
			if !sf.board && strings.TrimSpace(sf.chair) == "" {
				return fmt.Errorf("meet: an open meeting requires an owner (facilitator) — pass --owner NAME from `bashy agent list`")
			}
			shared := sf.shared
			if shared && !sf.board {
				return fmt.Errorf("meet: --shared rooms are boards (nothing schedules a turn across hosts); pass --board")
			}
			st, err := sf.newState()
			if err != nil {
				return err
			}
			if shared {
				if SharedSessionID == nil {
					return fmt.Errorf("meet: --shared needs the relay bashy wires (not available here)")
				}
				sid, serr := SharedSessionID()
				if serr != nil {
					return fmt.Errorf("meet: --shared: %w", serr)
				}
				st.Shared, st.Session = true, sid
			}
			w := cmd.OutOrStdout()
			sf.printRoster(w)
			printPreview(w, st)
			if dry {
				fmt.Fprintln(w, "  (dry-run: no agents launched)")
				return nil
			}
			if err := st.save(); err != nil {
				return err
			}
			markDepth()
			for _, a := range st.Agenda {
				_, _ = record(st, "agenda", procedural(st), string(RoleChair), a)
			}
			if len(seqs) > 0 {
				posted, err := SeedBoardFromMB(st, seqs)
				if err != nil {
					return err
				}
				fmt.Fprintf(w, "seeded board %s from mb %s\n", st.ID, joinSeqs(seqs))
				if posted {
					fmt.Fprintln(w, "posted a pointer back to mb — the room is the thread")
				} else {
					// The pointer is best-effort, but saying it happened when it
					// did not is the failure this whole surface exists to remove:
					// anyone still on mb would never learn the thread had moved.
					fmt.Fprintln(w, "no pointer posted: no message-board seam is wired — tell the thread yourself")
				}
			}
			if !nonInteractive {
				return repl(cmd, st)
			}
			if err := deliberate(cmd.Context(), st, w, rounds, "", true); err != nil {
				return err
			}
			// An unattended run cannot prompt a human; an agent initiator is still
			// asked, because that is the whole point of agent-initiated meetings.
			autoYes := yes || st.initiatorKind() == "human"
			path, err := closeMeeting(cmd.Context(), st, closeOptions{
				Synthesize: true, Confirm: true, Yes: autoYes,
				In: cmd.InOrStdin(), Out: w,
			}, nil)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "wrote %s\n", redactHome(path))
			return nil
		},
	}
	sf.bind(cmd)
	f := cmd.Flags()
	f.IntVar(&rounds, "rounds", 1, "rounds to run in --non-interactive mode")
	f.BoolVar(&dry, "dry-run", false, "print the resolved session and exit")
	f.BoolVar(&nonInteractive, "non-interactive", false, "run rounds then close, no REPL")
	f.BoolVar(&yes, "yes", false, "close without asking the initiator to confirm")
	f.BoolVar(&sf.board, "board", false,
		"open a BOARD: participants read and post on their own turns. No facilitator runs the "+
			"floor and no secretary is spawned; post with bashy meet tell ROOM --as NAME MESSAGE")
	f.BoolVar(&sf.shared, "shared", false,
		"share the board across hosts: every post rides this repo's cloudbox session and colleagues on "+
			"other hosts (same account or another) read and post in a mirror of the room under the same id. "+
			"Invite them as <name>@<host>; people from the people catalog by name. Requires --board")
	f.StringVar(&fromMB, "from-mb", "",
		"seed the board from these message-board posts (comma-separated seqs, e.g. 3,7,12), "+
			"attributed to their original authors, and post a pointer back to mb. Requires --board")
	return cmd
}

// changedUnattendedOpenFlags names explicit lifecycle controls that have no
// meaning in the interactive REPL. Defaults do not count: an ordinary
// `meet open` remains interactive, while spelling a bound cannot be ignored.
func changedUnattendedOpenFlags(cmd *cobra.Command) []string {
	var out []string
	for _, name := range []string{"rounds", "yes"} {
		if cmd.Flags().Changed(name) {
			out = append(out, "--"+name)
		}
	}
	return out
}

// parseSeqList parses the comma-separated message-board sequence list of
// --from-mb into positive int64s. Empty is no seeding; a non-numeric or
// non-positive entry is a usage error rather than a silently dropped post.
func parseSeqList(s string) ([]int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	var out []int64
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "#"))
		if part == "" {
			continue
		}
		n, err := strconv.ParseInt(part, 10, 64)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("meet: --from-mb: %q is not a message-board sequence", part)
		}
		out = append(out, n)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("meet: --from-mb: no sequences given")
	}
	return out, nil
}

// Consult outcomes. Disagreement is a FIRST-CLASS result, not an error: the one
// thing a calling agent must never do is read `summary` from a panel that did not
// converge and act on it as an answer.
const (
	verdictAgree    = "agree"    // decisive, complete panel — safe to act on
	verdictSplit    = "split"    // the panel genuinely disagreed — you decide
	verdictEscalate = "escalate" // the panel could not answer (seats failed, or nothing decided)
)

// Exit codes, mirroring the convention agent-callable gates converge on:
// 0 = act on it · 1 = a decision exists but blocking issues were raised ·
// 2 = do not proceed on this alone.
const (
	exitAgree    = 0
	exitBlocked  = 1
	exitEscalate = 2
)

// Verdict is the machine-readable result of a one-shot `meet consult`. It is the
// return value of `meet` used as a tool: a calling agent reads this, not prose.
type Verdict struct {
	Schema        string      `json:"schema"`
	ID            string      `json:"id"`
	Topic         string      `json:"topic"`
	Question      string      `json:"question,omitempty"`
	Participants  []string    `json:"participants"`
	Rounds        int         `json:"rounds"`
	Verdict       string      `json:"verdict"`    // agree | split | escalate
	Confidence    float64     `json:"confidence"` // 0..1, from panel coverage and vote share
	ExitCode      int         `json:"exit_code"`
	Summary       string      `json:"summary,omitempty"`
	Decisions     []Decision  `json:"decisions,omitempty"`
	Actions       []string    `json:"actions,omitempty"`
	Risks         []string    `json:"risks,omitempty"` // the blocking issues
	OpenQuestions []string    `json:"open_questions,omitempty"`
	Corrections   []string    `json:"corrections,omitempty"`
	Poll          *PollResult `json:"poll,omitempty"`
	Coverage      []Coverage  `json:"coverage"`
	Minutes       string      `json:"minutes"`
}

// decide computes the verdict, a confidence, and an exit code from what actually
// happened — never from a model's self-reported confidence, which the literature
// finds badly miscalibrated. Confidence here is a coverage-and-vote-share
// statistic: what fraction of the seats we asked actually answered, and how
// lopsidedly.
func (v *Verdict) decide() {
	seats := len(v.Coverage)
	answered := 0
	for _, c := range v.Coverage {
		if c.Contributed() {
			answered++
		}
	}
	coverageRatio := 1.0
	if seats > 0 {
		coverageRatio = float64(answered) / float64(seats)
	}

	// A poll, when present, is the sharpest signal we have.
	agreement := 0.5
	decisive := false
	if v.Poll != nil {
		if win, ok := v.Poll.Winner(); ok {
			decisive = true
			if seats > 0 {
				agreement = float64(v.Poll.Tally[win]) / float64(seats)
			}
		} else {
			agreement = 0.0
		}
	} else if len(v.Decisions) > 0 {
		decisive = true
		agreement = 1.0
	} else {
		agreement = 0.0
	}

	v.Confidence = coverageRatio * agreement

	switch {
	case answered < seats || answered == 0:
		// Half a panel is a sample, not a consensus, however loudly it agreed.
		v.Verdict, v.ExitCode = verdictEscalate, exitEscalate
	case len(v.Decisions) == 0:
		// The panel answered but settled nothing — there is no result to split over.
		v.Verdict, v.ExitCode = verdictEscalate, exitEscalate
	case !decisive:
		v.Verdict, v.ExitCode = verdictSplit, exitEscalate
	case len(v.Risks) > 0:
		v.Verdict, v.ExitCode = verdictAgree, exitBlocked
	default:
		v.Verdict, v.ExitCode = verdictAgree, exitAgree
	}
}

// newConsultCmd is `meet` as a tool call: one command, no REPL, no confirmation
// round-trip (the caller IS the initiator and receives the verdict synchronously),
// and a JSON verdict on stdout. An agent mid-task runs this to get a cross-vendor
// second opinion and then continues.
func newConsultCmd() *cobra.Command {
	var sf sessionFlags
	var question, deadline string
	var choices []string
	var rounds int
	var jsonOut, failOnDissent bool
	cmd := &cobra.Command{
		Use:   "consult --topic TEXT [--question TEXT] [--participant AGENT ...]",
		Short: "one-shot panel: convene, deliberate, synthesize, return a verdict (agent-callable)",
		Long: "Convene a panel, run the rounds, poll if --choice is given, synthesize, file the\n" +
			"minutes, and print a verdict. Blocks until done and never enters a REPL, so an\n" +
			"agentic tool can call it as a tool and read the result.\n\n" +
			"A participant cannot call this from inside a meeting (unbounded recursion).",
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := guardDepth(); err != nil {
				return err
			}
			// The caller is an agent we cannot name unless it says so. Leave the
			// initiator empty rather than inventing an attendee; consult never
			// confirms, because the caller receives the verdict synchronously.
			if q := strings.TrimSpace(question); q != "" && len(sf.agenda) == 0 {
				sf.agenda = []string{q}
			}
			st, err := sf.newState()
			if err != nil {
				return err
			}
			// consult's stdout is the verdict a caller parses; the roster note
			// is commentary and belongs on stderr.
			sf.printRoster(cmd.ErrOrStderr())
			if len(st.Participants) == 0 {
				return fmt.Errorf("meet: consult needs at least one --participant or a --min-band")
			}
			if err := st.save(); err != nil {
				return err
			}
			markDepth()
			w := cmd.OutOrStdout()

			// A blocking call needs a ceiling on the WHOLE consult, not just on
			// each turn: N participants × R rounds × --turn-timeout is otherwise a
			// multi-hour hang inside somebody's tool call.
			ctx := cmd.Context()
			if d, err := time.ParseDuration(strings.TrimSpace(deadline)); err == nil && d > 0 {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, d)
				defer cancel()
			} else if strings.TrimSpace(deadline) != "" {
				return fmt.Errorf("meet: bad --deadline %q: %w", deadline, err)
			}

			for _, a := range st.Agenda {
				_, _ = record(st, "agenda", procedural(st), string(RoleChair), a)
			}
			if err := deliberate(ctx, st, w, rounds, question, false); err != nil {
				return err
			}

			v := &Verdict{
				Schema: schemaVersion, ID: st.ID, Topic: st.Topic, Question: question,
				Participants: st.Participants,
			}
			if len(choices) > 0 {
				poll, err := runPoll(ctx, st, question, choices, st.Participants, nil)
				if err != nil {
					return err
				}
				for i := range poll.Votes {
					poll.Votes[i].Text = redactHome(poll.Votes[i].Text)
					poll.Votes[i].File = redactHome(poll.Votes[i].File)
				}
				v.Poll = poll
			}

			// The caller is the initiator and gets the verdict synchronously, so
			// there is nobody else to confirm the conclusion to.
			path, err := closeMeeting(ctx, st, closeOptions{
				Synthesize: true, Confirm: false, Out: io.Discard,
			}, nil)
			if err != nil {
				return err
			}
			events, _ := readTranscript(st.ID)
			if syn := loadSynthesis(st.ID); syn != nil {
				v.Summary, v.Decisions, v.Actions = syn.Summary, syn.Decisions, syn.Actions
				v.Risks, v.OpenQuestions, v.Corrections = syn.Risks, syn.OpenQuestions, syn.Corrections
			}
			v.Rounds, v.Coverage, v.Minutes = st.Round, coverage(st, events), redactHome(path)
			v.decide()

			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				if err := enc.Encode(v); err != nil {
					return err
				}
			} else {
				writeVerdict(w, v)
			}
			if failOnDissent && v.ExitCode != exitAgree {
				return fmt.Errorf("meet: verdict=%s (exit_code=%d) — the panel did not agree", v.Verdict, v.ExitCode)
			}
			return nil
		},
	}
	sf.bind(cmd)
	f := cmd.Flags()
	f.StringVar(&question, "question", "", "the question put to the panel")
	f.StringArrayVar(&choices, "choice", nil, "make it a poll: permitted answer (repeatable; default free-form)")
	f.IntVar(&rounds, "rounds", 1, "deliberation rounds before the poll/synthesis")
	f.StringVar(&deadline, "deadline", "10m", "hard ceiling on the whole consult (a blocking tool call must not hang)")
	f.BoolVar(&jsonOut, "json", false, "emit the verdict as JSON (the agent-callable shape)")
	f.BoolVar(&failOnDissent, "fail-on-dissent", false, "exit non-zero unless the verdict is `agree` with no blocking issues")
	return cmd
}

func writeVerdict(w io.Writer, v *Verdict) {
	fmt.Fprintf(w, "meeting %s — %s\n\n", v.ID, v.Topic)
	if v.Summary != "" {
		fmt.Fprintf(w, "%s\n\n", v.Summary)
	}
	if v.Poll != nil {
		if win, ok := v.Poll.Winner(); ok {
			fmt.Fprintf(w, "poll: %s\n", win)
		} else {
			fmt.Fprintf(w, "poll: no clear result\n")
		}
		for _, vote := range v.Poll.Votes {
			answer := vote.Choice
			if answer == "" {
				answer = statusOf(vote)
			}
			fmt.Fprintf(w, "  %-12s %s\n", vote.Speaker, answer)
		}
		fmt.Fprintln(w)
	}
	for _, d := range v.Decisions {
		tag := ""
		if d.Inferred {
			tag = " (inferred)"
		}
		fmt.Fprintf(w, "decision: %s%s\n", d.Text, tag)
	}
	for _, a := range v.Actions {
		fmt.Fprintf(w, "action:   %s\n", a)
	}
	for _, r := range v.Risks {
		fmt.Fprintf(w, "risk:     %s\n", r)
	}
	for _, q := range v.OpenQuestions {
		fmt.Fprintf(w, "open:     %s\n", q)
	}
	fmt.Fprintf(w, "\nverdict: %s (confidence %.2f, exit %d)\n", v.Verdict, v.Confidence, v.ExitCode)
	switch v.Verdict {
	case verdictSplit:
		fmt.Fprintln(w, "⚠ the panel genuinely disagreed — this is input, not an answer")
	case verdictEscalate:
		fmt.Fprintln(w, "⚠ the panel could not answer (seats failed, or nothing was decided) — do not act on this alone")
	}
	fmt.Fprintf(w, "\nminutes: %s\n", v.Minutes)
}

// repl is the interactive meeting loop.
func repl(cmd *cobra.Command, st *State) error {
	if st == nil {
		return fmt.Errorf("meet: cannot enter interactive mode without a room")
	}
	if st.Status != "open" {
		return fmt.Errorf("meet: %s is %s; terminal rooms cannot enter interactive mode", st.ID, st.Status)
	}
	w := cmd.OutOrStdout()
	sc := bufio.NewScanner(cmd.InOrStdin())
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	secretary := "none (this room keeps no minutes)"
	if st.recorded() {
		secretary = st.secretary() + "(notes-only)"
	}
	fmt.Fprintf(w, "\nmeet %s · secretary=%s · participants: %s\n",
		st.ID, secretary, strings.Join(st.Participants, ", "))
	fmt.Fprintln(w, "commands: <text> | @name <text> | /round | /facilitator | /poll <q> | /ask <q> |")
	fmt.Fprintln(w, "          /invite <agent> | /kick <agent> |")
	fmt.Fprintln(w, "          /decision <t> | /action owner: task | /agenda <t> | /show | /converge | /close")
	fmt.Fprint(w, "you> ")
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			fmt.Fprint(w, "you> ")
			continue
		}
		switch {
		case line == "/converge":
			syn, err := converge(cmd.Context(), st, nil)
			if err != nil {
				fmt.Fprintf(w, "⏺ secretary pass failed: %v\n", err)
				break
			}
			fmt.Fprintf(w, "⏺ converged: %d decisions, %d actions, %d risks, %d open questions\n",
				len(syn.Decisions), len(syn.Actions), len(syn.Risks), len(syn.OpenQuestions))
			if syn.Summary != "" {
				fmt.Fprintf(w, "  summary: %s\n", oneLine(syn.Summary))
			}
		case line == "/show":
			events, _ := readTranscript(st.ID)
			writeShow(w, st, events, loadSynthesis(st.ID))
		case line == "/close":
			path, err := closeMeeting(cmd.Context(), st, closeOptions{
				Synthesize: true, Confirm: true, In: cmd.InOrStdin(), Out: w,
			}, nil)
			if errors.Is(err, ErrDeclined) {
				fmt.Fprintln(w, "⏺ meeting continues.")
				break
			}
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "wrote %s\n", redactHome(path))
			return nil
		case line == "/round":
			evs, err := runRound(cmd.Context(), st, currentAgenda(st), nil)
			if err != nil {
				fmt.Fprintf(w, "⏺ %v\n", err)
				break
			}
			for _, e := range evs {
				fmt.Fprintf(w, "%s> %s\n", e.Speaker, oneLine(e.Text))
			}
		case line == "/facilitator":
			if !st.chaired() {
				fmt.Fprintln(w, "⏺ no --owner (facilitator) for this meeting; use /round, or restart with --owner <agent>")
				break
			}
			res, err := runChaired(cmd.Context(), st, nil)
			if err != nil {
				fmt.Fprintf(w, "⏺ facilitation failed: %v\n", err)
				break
			}
			fmt.Fprintf(w, "⏺ facilitated %d turns (%d stalls, %d re-plans, %d degraded) — stopped by %s\n",
				res.Turns, res.Stalls, res.Replans, res.Degraded, res.StoppedBy)
		case strings.HasPrefix(line, "/poll "):
			q := strings.TrimSpace(line[len("/poll "):])
			res, err := runPoll(cmd.Context(), st, q, nil, nil, nil)
			if err != nil {
				fmt.Fprintf(w, "⏺ poll failed: %v\n", err)
				break
			}
			for _, v := range res.Votes {
				answer := v.Choice
				if answer == "" {
					answer = statusOf(v)
				}
				fmt.Fprintf(w, "%s> %s — %s\n", v.Speaker, answer, oneLine(v.Text))
			}
			if win, ok := res.Winner(); ok {
				fmt.Fprintf(w, "⏺ poll result: %s\n", win)
			} else {
				fmt.Fprintln(w, "⏺ poll result: no clear result")
			}
		case strings.HasPrefix(line, "/ask "):
			q := strings.TrimSpace(line[len("/ask "):])
			evs, err := runAsk(cmd.Context(), st, q, true, nil, nil)
			if err != nil {
				fmt.Fprintf(w, "⏺ ask failed: %v\n", err)
				break
			}
			for _, e := range evs {
				fmt.Fprintf(w, "%s> %s\n", e.Speaker, oneLine(e.Text))
			}
		case strings.HasPrefix(line, "/decision "):
			t := strings.TrimSpace(line[len("/decision "):])
			_, _ = record(st, "decision", st.Human, "", t)
			fmt.Fprintf(w, "⏺ recorded DECISION: %s\n", t)
		case strings.HasPrefix(line, "/action "):
			t := strings.TrimSpace(line[len("/action "):])
			_, _ = record(st, "action", st.Human, "", t)
			fmt.Fprintf(w, "⏺ recorded ACTION: %s\n", t)
		case strings.HasPrefix(line, "/invite "):
			// The actor is whoever is at this keyboard — the room's human. If
			// somebody else convened the room, requireOrganizer says so.
			name := strings.TrimSpace(line[len("/invite "):])
			if err := inviteTo(st, st.Human, name); err != nil {
				fmt.Fprintf(w, "⏺ %v\n", err)
				break
			}
			fmt.Fprintf(w, "⏺ %s joined — participants: %s\n", seatLabel(canonAgent(name)), strings.Join(st.Participants, ", "))
		case strings.HasPrefix(line, "/kick "):
			name := strings.TrimSpace(line[len("/kick "):])
			if err := kickFrom(st, st.Human, name); err != nil {
				fmt.Fprintf(w, "⏺ %v\n", err)
				break
			}
			fmt.Fprintf(w, "⏺ %s left — participants: %s\n", seatLabel(canonAgent(name)), strings.Join(st.Participants, ", "))
		case strings.HasPrefix(line, "/agenda "):
			t := strings.TrimSpace(line[len("/agenda "):])
			st.Agenda = append(st.Agenda, t)
			_ = st.save()
			_, _ = record(st, "agenda", procedural(st), string(RoleChair), t)
		case strings.HasPrefix(line, "@"):
			name, text, _ := strings.Cut(strings.TrimPrefix(line, "@"), " ")
			ev, _ := runTurn(cmd.Context(), st, name, text, nil)
			fmt.Fprintf(w, "%s> %s\n", ev.Speaker, oneLine(ev.Text))
		default:
			_, _ = record(st, "human", st.Human, "human", line)
		}
		fmt.Fprint(w, "you> ")
	}
	return sc.Err()
}

func newTellCmd() *cobra.Command {
	var as, to string
	cmd := &cobra.Command{
		Use:   "tell <room>|<id> <text...>",
		Short: "append a human contribution to a session",
		Long: "Append a human contribution. One manually authored tell is limited to 1024 UTF-8 bytes and is never truncated or auto-split. " +
			"Prefer a stable repo-relative commit/issue/room/artifact reference; without one, manually send numbered <=1024-byte parts with one correlation token and END, then report missing parts.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			text := strings.Join(args[1:], " ")
			if strings.TrimSpace(text) == "" {
				b, err := io.ReadAll(cmd.InOrStdin())
				if err != nil {
					return err
				}
				text = string(b)
			}
			if err := bus.ValidateCoordinationBody(text); err != nil {
				return fmt.Errorf("meet tell: %w", err)
			}
			ev, err := PostAs(args[0], as, to, text)
			if err != nil {
				if strings.Contains(err.Error(), "failed:") {
					fmt.Fprintf(cmd.ErrOrStderr(), "failed: %s\n", strings.TrimSpace(strings.TrimPrefix(to, "@")))
				}
				return err
			}
			writeTellReceipts(cmd.ErrOrStderr(), args[0], ev)
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "participant name to post as (required on a board)")
	cmd.Flags().StringVar(&to, "to", "",
		"participant name to address, or `all` to address every participant "+
			"(omit to post room history that is addressed to nobody)")
	return cmd
}

func newReadCmd() *cobra.Command {
	var as string
	var wait time.Duration
	var peek, jsonOut bool
	var limit int
	cmd := &cobra.Command{
		Use:   "read <room>|<id> --as NAME [--wait DUR] [--peek] [--limit N] [--json]",
		Short: "read unread board messages for a participant",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			reader := canonAgent(strings.TrimSpace(strings.TrimPrefix(as, "@")))
			if reader == "" {
				return fmt.Errorf("meet: --as NAME is required")
			}
			if wait < 0 {
				return fmt.Errorf("meet: --wait cannot be negative")
			}
			if limit < 0 {
				return fmt.Errorf("meet: --limit cannot be negative")
			}
			st, err := roomOf(args[0])
			if err != nil {
				return err
			}
			if !st.board() {
				return fmt.Errorf("meet: room %s is not a board", st.ID)
			}
			// READING IS OPEN; only posting needs a seat. Three reasons, and the
			// third is decisive:
			//
			// (1) It is what an mb pointer promises. A board that seeds from a
			//     thread posts "read: bashy meet read <id> --as <you>" back to
			//     mb, and everyone who reads that is BY DEFINITION not seated
			//     yet. Gating the read makes the invitation instruct people to
			//     run a command that refuses them.
			// (2) `meet observe` already renders the same transcript to anyone,
			//     unrestricted and exit 0. So the gate bought no confidentiality
			//     whatsoever -- it only broke the documented flow.
			// (3) Membership stays organizer-push regardless: reading takes no
			//     seat, appears on no roster, and grants no right to post.
			if wait > 0 {
				if st.Status == "closed" {
					return fmt.Errorf("meet: --wait is refused on closed board %s", st.ID)
				}
				if err := WaitForRoom(cmd.Context(), st.ID, reader, limit, wait); err != nil {
					return err
				}
			}
			directed, other, older, through, err := UnreadThrough(st.ID, reader, limit)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, e := range directed {
				writeEvent(w, e, jsonOut)
			}
			for _, e := range other {
				writeEvent(w, e, jsonOut)
			}
			if older > 0 && !jsonOut {
				fmt.Fprintf(cmd.ErrOrStderr(), "%d older broadcast event(s) hidden by --limit\n", older)
			}
			if !peek {
				if err := MarkSeenThrough(st.ID, reader, through); err != nil {
					return err
				}
			}
			if len(directed)+len(other) == 0 {
				seen := SeenSeq(st.ID, reader)
				if wait > 0 {
					fmt.Fprintf(cmd.ErrOrStderr(), "EMPTY (timeout; seen through %d)\n", seen)
				} else {
					fmt.Fprintf(cmd.ErrOrStderr(), "EMPTY (seen through %d)\n", seen)
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "participant name to read as")
	cmd.Flags().DurationVar(&wait, "wait", 0, "wait up to DUR for unread board messages")
	cmd.Flags().BoolVar(&peek, "peek", false, "do not advance the read cursor")
	cmd.Flags().IntVarP(&limit, "limit", "n", DefaultRoomLimit, "maximum broadcast events to show")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit canonical event JSON")
	return cmd
}

// writeTellReceipts tells the SENDER who the message went to and how far it
// got. The resolution half is not decoration: a room's default addressee is
// stored as a seat label, so without rendering it the sender believes they
// addressed somebody and the recipient was never named — which is the Sprint 98
// failure running in the other direction.
func writeTellReceipts(w io.Writer, ref string, ev Event) {
	st, err := roomOf(ref)
	if err != nil {
		return
	}
	recipients := []string{}
	if ev.To != "" {
		recipients = append(recipients, ev.To)
	} else {
		for _, p := range st.Participants {
			if !strings.EqualFold(p, ev.Speaker) {
				recipients = append(recipients, p)
			}
		}
	}
	if len(recipients) == 0 {
		fmt.Fprintln(w, "queued: room")
		return
	}
	events, err := readRoomTranscript(st.ID)
	if err != nil {
		return
	}
	seq := int64(len(events))
	for _, r := range recipients {
		holder, live := bus.RoleHolderFor(r)
		if !live && !isRoleAddress(r) {
			fmt.Fprintf(w, "%s: %s\n", boardDeliveryState(st.ID, r, seq), r)
			continue
		}
		if !live {
			// The seat is real and vacant. Durable, addressed, undeliverable
			// right now — and it resolves itself the moment somebody takes the
			// lease, which is the entire point of addressing a seat.
			fmt.Fprintf(w, "→ %s (no current holder — resolves when the seat is taken)\n", r)
			fmt.Fprintf(w, "queued: %s\n", r)
			continue
		}
		fmt.Fprintf(w, "→ %s (currently %s)\n", r, holder)
		fmt.Fprintf(w, "%s: %s\n", boardDeliveryState(st.ID, holder, seq), holder)
	}
}

// isRoleAddress reports a seat-shaped address ("conductor:99", "steward").
//
// It is deliberately shape-based rather than a registry lookup: an address the
// host cannot resolve must still RENDER as the seat it names. The alternative
// prints a bare label as if it were an agent name, which is how mail becomes
// unattributable — the same argument bus.RoleLabelFor makes for the reverse
// direction.
func isRoleAddress(to string) bool {
	to = strings.TrimSpace(to)
	if to == "" {
		return false
	}
	if strings.EqualFold(to, "steward") {
		return true
	}
	kind, ref, ok := strings.Cut(to, ":")
	return ok && strings.TrimSpace(kind) != "" && strings.TrimSpace(ref) != ""
}

func boardDeliveryState(id, reader string, seq int64) string {
	p, err := seenPath(id, reader)
	if err != nil {
		return "failed"
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "unverified"
		}
		return "failed"
	}
	at, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return "failed"
	}
	if at >= seq {
		return "read"
	}
	return "queued"
}

func newRoundCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "round <room>|<id>",
		Short: "run one moderated round across participants",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			markDepth()
			evs, err := Round(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			for _, e := range evs {
				fmt.Fprintf(cmd.OutOrStdout(), "%s> %s\n", e.Speaker, oneLine(e.Text))
			}
			return nil
		},
	}
}

// newPollCmd is the request-for-comment style: a fixed answer set, every
// participant must answer, the tally is recorded.
func newPollCmd() *cobra.Command {
	var question string
	var choices, participants []string
	cmd := &cobra.Command{
		Use:   "poll <id> --question TEXT [--choice yes --choice no]",
		Short: "put a fixed-choice question to the participants and tally the answers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := loadState(args[0])
			if err != nil {
				return err
			}
			markDepth()
			res, err := runPoll(cmd.Context(), st, question, choices, participants, nil)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, v := range res.Votes {
				answer := v.Choice
				if answer == "" {
					answer = statusOf(v)
				}
				fmt.Fprintf(w, "%-12s %-10s %s\n", v.Speaker, answer, oneLine(v.Text))
			}
			if win, ok := res.Winner(); ok {
				fmt.Fprintf(w, "\nresult: %s\n", win)
			} else {
				fmt.Fprintln(w, "\nresult: no clear result (tie, or too many non-answers)")
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&question, "question", "", "the poll question (required)")
	f.StringArrayVar(&choices, "choice", nil, "permitted answer (repeatable; default: yes, no)")
	f.StringArrayVar(&participants, "participant", nil, "poll only these participants (default: all)")
	return cmd
}

// newAskCmd is the open-question style: answering is optional and silence is a
// recorded abstention rather than a failure.
func newAskCmd() *cobra.Command {
	var question string
	var participants []string
	var required bool
	cmd := &cobra.Command{
		Use:   "ask <id> --question TEXT",
		Short: "put an open question to the participants (answering is optional)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := loadState(args[0])
			if err != nil {
				return err
			}
			markDepth()
			evs, err := runAsk(cmd.Context(), st, question, !required, participants, nil)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			for _, e := range evs {
				fmt.Fprintf(w, "── %s (%s)\n%s\n\n", e.Speaker, statusOf(e), strings.TrimSpace(e.Text))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&question, "question", "", "the open question (required)")
	f.StringArrayVar(&participants, "participant", nil, "ask only these participants (default: all)")
	f.BoolVar(&required, "required", false, "an empty answer is a failure, not an abstention")
	return cmd
}

// newInviteCmd and newKickCmd change the roster of a RUNNING room. Both take a
// room reference rather than an id, because the human doing it is looking at
// `bashy meet list` and will type the room number.
func newInviteCmd() *cobra.Command {
	var as, notify string
	var inv OpenInvite
	cmd := &cobra.Command{
		Use:   "invite <ref> [<agent>]",
		Short: "seat an agent in a running room, or open seating to an audience (organizer only)",
		Long: "Add an agent to a room that is already open. It speaks from the next round on.\n\n" +
			"Only the organizer — whoever convened the room — may change its roster.\n" +
			"Inviting somebody already seated is a no-op, not a second seat.\n\n" +
			"On a BOARD, give a selector instead of an agent to open seating to an audience:\n" +
			"  bashy meet invite <ref> --any            # anyone may self-seat on first post\n" +
			"  bashy meet invite <ref> --band 4         # any L4 may self-seat\n" +
			"  bashy meet invite <ref> --tool codex     # any codex agent may self-seat\n" +
			"A matching agent self-seats on its first post; nobody is pushed in. The selector\n" +
			"vocabulary is `mb send`'s own (--band/--tool/--provider/--family/--version/--any).",
		Args: cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			actor := strings.TrimSpace(as)
			if actor == "" {
				actor = humanName()
			}
			// One ref, no agent, and a declared audience is the OPEN-INVITE path:
			// the organizer delegates seating rather than pushing one agent in.
			if len(args) == 1 || !inv.empty() {
				if len(args) == 2 {
					return fmt.Errorf("meet: give an agent to push in, OR a selector to open seating — not both")
				}
				return runOpenInvite(cmd, args[0], actor, inv)
			}
			if notify != "mb" && notify != "none" {
				return fmt.Errorf("meet: --notify must be mb or none, got %q", notify)
			}
			if err := Invite(args[0], actor, args[1]); err != nil {
				return err
			}
			st, err := loadMeeting(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s is in %s — participants: %s\n",
				seatLabel(canonAgent(args[1])), st.ID, strings.Join(st.Participants, ", "))
			inv := invitationFor(st, canonAgent(args[1]))
			// Always render this literal command. It is both the fallback when no
			// notifier is configured and the only stable instruction: st.Room is a
			// reusable convenience number, while inv.ID survives the room lifetime.
			fmt.Fprintf(cmd.OutOrStdout(), "join: %s\n", inv.Join)
			switch notify {
			case "none":
				fmt.Fprintln(cmd.OutOrStdout(), "notification disabled")
			case "mb":
				if Notify == nil {
					fmt.Fprintln(cmd.OutOrStdout(), "notification unavailable: no notifier is configured")
					return nil
				}
				delivered, reason, err := Notify(canonAgent(args[1]), inv)
				if err != nil {
					return fmt.Errorf("meet: notify %s: %w", seatLabel(canonAgent(args[1])), err)
				}
				if delivered {
					if reason != "" {
						fmt.Fprintf(cmd.OutOrStdout(), "notified %s: %s\n", seatLabel(canonAgent(args[1])), reason)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "notified %s\n", seatLabel(canonAgent(args[1])))
					}
				} else if reason != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "notification not delivered: %s\n", reason)
				} else {
					fmt.Fprintln(cmd.OutOrStdout(), "notification not delivered")
				}
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "act as this member (default: the human); must be the room's organizer")
	cmd.Flags().StringVar(&notify, "notify", "mb", "notification transport: mb | none")
	f := cmd.Flags()
	f.BoolVar(&inv.Any, "any", false, "open seating to ANYONE (a board): any registered agent may self-seat")
	f.IntVar(&inv.Band, "band", 0, "open seating to agents at this capability band (a board)")
	f.StringVar(&inv.Tool, "tool", "", "open seating to agents on this tool (a board)")
	f.StringVar(&inv.Provider, "provider", "", "open seating to agents on this provider (a board)")
	f.StringVar(&inv.Family, "family", "", "open seating to agents in this model family (a board)")
	f.StringVar(&inv.Version, "version", "", "open seating to agents at this model version (a board)")
	return cmd
}

// runOpenInvite records the open invite on the board and posts a group invite to
// mb. Recording and announcing are two acts: SetOpenTo is authoritative (an
// agent may self-seat even if the announcement never lands), and the mb post is
// how agents that are not watching the room learn the door is open. The receipt
// never claims the announcement was delivered when no seam is wired.
func runOpenInvite(cmd *cobra.Command, ref, actor string, inv OpenInvite) error {
	st, err := SetOpenTo(ref, actor, inv)
	if err != nil {
		return err
	}
	w := cmd.OutOrStdout()
	fmt.Fprintf(w, "%s is open to %s — a matching agent self-seats on its first post\n", st.ID, inv.describe())
	fmt.Fprintf(w, "join: bashy meet tell %s --as <you> \"...\"\n", st.roomRef())
	if PostMB == nil {
		fmt.Fprintln(w, "notification unavailable: no message-board seam is wired")
		return nil
	}
	body := fmt.Sprintf("open board %s — %q. %s may join by posting: bashy meet tell %s --as <you> \"...\"",
		st.durableRef(), st.Topic, inv.describe(), st.ID)
	audience := inv
	if _, err := PostMB(MBPost{From: actor, Topic: st.Topic, Body: body}, &audience); err != nil {
		return fmt.Errorf("meet: post mb group invite: %w", err)
	}
	fmt.Fprintf(w, "posted a group invite to %s on mb\n", inv.describe())
	return nil
}

func newKickCmd() *cobra.Command {
	var as string
	cmd := &cobra.Command{
		Use:   "kick <ref> <agent>",
		Short: "remove an agent from a running room (organizer only)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			actor := strings.TrimSpace(as)
			if actor == "" {
				actor = humanName()
			}
			if err := Kick(args[0], actor, args[1]); err != nil {
				return err
			}
			st, err := loadMeeting(args[0])
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s left %s — participants: %s\n",
				seatLabel(canonAgent(args[1])), st.ID, strings.Join(st.Participants, ", "))
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "act as this member (default: the human); must be the room's organizer")
	return cmd
}

func newConvergeCmd() *cobra.Command {
	var mode, secretary string
	cmd := &cobra.Command{
		Use:   "converge <room>|<id>",
		Short: "secretary pass: extract decisions, actions, risks, open questions, corrections",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// The overrides are applied and PERSISTED first, then the shared verb
			// runs off what is on disk — so `converge` and the HTTP route execute
			// the identical pass rather than one of them carrying a private
			// in-memory tweak the other cannot see.
			st, err := roomOf(args[0])
			if err != nil {
				return err
			}
			if err := overrideSecretary(st, secretary); err != nil {
				return err
			}
			if mode != "" {
				st.DecisionMode = mode
				if err := st.save(); err != nil {
					return err
				}
			}
			markDepth()
			syn, err := Converge(cmd.Context(), st.ID)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			fmt.Fprintf(w, "converged (%s mode): %d decisions, %d actions, %d risks, %d open questions, %d corrections\n",
				syn.Mode, len(syn.Decisions), len(syn.Actions), len(syn.Risks), len(syn.OpenQuestions), len(syn.Corrections))
			if syn.Summary != "" {
				fmt.Fprintf(w, "summary: %s\n", oneLine(syn.Summary))
			}
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&mode, "decision-mode", "", "override the session's decision mode: infer | explicit")
	f.StringVar(&secretary, "secretary", "", "record with this agent instead of the session's secretary (recovery when the original could not launch)")
	return cmd
}

func newCloseCmd() *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "close <room>|<id>",
		Short: "secretary pass, confirm with the initiator, then write and file the minutes",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			markDepth()
			// closeRoom is the body api.Close shares with this command. The actor
			// is empty here — this is the ATTENDED path, where the terminal prompt
			// is itself the privilege check: confirmConclusion asks the room's
			// initiator, not whoever typed the command.
			path, err := closeRoom(cmd.Context(), args[0], "", closeOptions{
				Synthesize: true, Confirm: true, Yes: yes,
				In: cmd.InOrStdin(), Out: cmd.OutOrStdout(),
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", redactHome(path))
			return nil
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "close without asking the initiator to confirm")
	return cmd
}

// newAbandonCmd reaps a room instead of concluding it. `close` is built for a
// live meeting reaching its end — it runs the secretary (converge SPAWNS an
// agent, and --yes does not skip it), asks the initiator to confirm, then files
// minutes. None of that is right for a room dead for six weeks: there is nothing
// to synthesize, nobody to confirm to, and no repo that wants its minutes.
//
// abandon is the janitorial exit. It marks the room ABANDONED (so it never reads
// like a concluded one afterwards), releases its room number, and archives the
// transcript beside the room's other artifacts — exactly as a reopen would, so
// the transcript SURVIVES; this is not a delete. It spawns nothing, synthesizes
// nothing, and files nothing, which is also why it removes the reason to reach
// for `close --yes` on a dead room: --yes suppresses the confirmation prompt but
// not the expensive secretary pass, so it was never the right tool for reaping.
func newAbandonCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "abandon <room>|<id>",
		Short: "reap a dead room: mark it abandoned, release its number, archive the transcript (spawns nothing)",
		Long: "Close a room that will never conclude. Unlike `close`, abandon spawns no\n" +
			"secretary, synthesizes nothing, and files no minutes: it marks the room\n" +
			"ABANDONED, releases its room number, and archives the transcript beside the\n" +
			"room's other artifacts. The transcript survives — this is not a delete.\n\n" +
			"Use it to reap a stale room instead of `close --yes`, which still runs the\n" +
			"expensive secretary pass it was never meant to.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveMeeting(args[0])
			if err != nil {
				return err
			}
			// Take the run lease so a room mid-turn cannot be reaped out from under
			// an active session; release it as soon as we are done.
			lease, err := acquireRunLease(id)
			if err != nil {
				return err
			}
			defer lease.Release()
			st, err := loadState(id)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if st.Status == "abandoned" {
				fmt.Fprintf(w, "%s is already abandoned\n", st.ID)
				return nil
			}
			room := st.Room
			// Record WHY the room ended before archiving, so the archived transcript
			// carries the reason and the room never reads as concluded. A plain
			// marker append — no agent is launched, nothing is extracted.
			_, _ = record(st, "abandoned", st.Human, "", "room abandoned; not concluded")
			// Move the transcript and its siblings under archive/<ts>, the same path
			// a reopen archives through — the transcript is preserved, not deleted.
			if err := archiveSessionArtifacts(st); err != nil {
				return err
			}
			st.Status = "abandoned"
			st.Room = 0
			if err := st.save(); err != nil {
				return err
			}
			if room > 0 {
				fmt.Fprintf(w, "abandoned %s (released room %d)\n", st.ID, room)
			} else {
				fmt.Fprintf(w, "abandoned %s\n", st.ID)
			}
			fmt.Fprintln(w, "  no synthesis, no minutes — the transcript is archived in the store")
			return nil
		},
	}
	return cmd
}

// newAmendCmd re-runs the secretary over the existing transcript and rewrites the
// minutes. The fix for a weak secretary pass: the transcript is the durable
// artifact, the minutes are a projection of it, and a projection can be redone.
func newAmendCmd() *cobra.Command {
	var mode, secretary string
	var resynthesize bool
	cmd := &cobra.Command{
		Use:   "amend <id>",
		Short: "regenerate the minutes from the transcript (optionally re-running the secretary)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := loadState(args[0])
			if err != nil {
				return err
			}
			if err := overrideSecretary(st, secretary); err != nil {
				return err
			}
			if secretary != "" {
				resynthesize = true // naming a new secretary is a request to re-run it
			}
			if mode != "" {
				st.DecisionMode = mode
				resynthesize = true
			}
			w := cmd.OutOrStdout()
			if resynthesize {
				markDepth()
				if _, err := converge(cmd.Context(), st, nil); err != nil {
					return err
				}
				fmt.Fprintf(w, "re-ran the secretary (%s mode)\n", st.decisionMode())
			}
			_ = st.save()
			path, err := fileMinutes(st)
			if err != nil {
				return err
			}
			fmt.Fprintf(w, "rewrote %s\n", redactHome(path))
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&mode, "decision-mode", "", "re-run the secretary with this mode: infer | explicit")
	f.StringVar(&secretary, "secretary", "", "re-run with this agent instead of the session's secretary (implies --resynthesize)")
	f.BoolVar(&resynthesize, "resynthesize", false, "re-run the secretary before rewriting the minutes")
	return cmd
}

func newApplyCmd() *cobra.Command {
	var to string
	var write bool
	cmd := &cobra.Command{
		Use:   "apply <id> [--to PATH --write]",
		Short: "render the agreed action items as a block; --write appends them to a document",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := loadState(args[0])
			if err != nil {
				return err
			}
			events, _ := readTranscript(st.ID)
			block, err := applyActions(st, events, loadSynthesis(st.ID), to, write)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if write {
				fmt.Fprintf(w, "appended %d action item(s) to %s\n", len(actionsOf(events, loadSynthesis(st.ID))), to)
				return nil
			}
			fmt.Fprint(w, block)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&to, "to", "", "target document")
	f.BoolVar(&write, "write", false, "append the block to --to (default: print it)")
	return cmd
}

func newShowCmd() *cobra.Command {
	var jsonOut, links bool
	cmd := &cobra.Command{
		Use:   "show <room>|<id>",
		Short: "show a meeting's roster, per-participant coverage, and artifacts",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveMeeting(args[0])
			if err != nil {
				return err
			}
			args = []string{id}
			st, err := loadState(args[0])
			if err != nil {
				return err
			}
			events, _ := readTranscript(st.ID)
			w := cmd.OutOrStdout()
			linkRefs := meetingLinks(events)
			if jsonOut {
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(map[string]any{
					"state": st, "coverage": coverage(st, events), "synthesis": loadSynthesis(st.ID), "links": linkRefs,
				})
			}
			writeShow(w, st, events, loadSynthesis(st.ID))
			if links {
				printMeetingLinks(w, linkRefs)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the session, coverage, and synthesis as JSON")
	cmd.Flags().BoolVar(&links, "links", false, "resolve transcript links at read time against this repo's kb and todo stores")
	return cmd
}

type meetLinkRef struct {
	Ref    string `json:"ref"`
	Title  string `json:"title,omitempty"`
	Status string `json:"status,omitempty"`
}

func meetingLinks(events []Event) []meetLinkRef {
	nodes := repoLinkNodes()
	seen := map[string]bool{}
	var out []meetLinkRef
	for _, e := range events {
		for _, l := range kb.ParseLinks(e.Text) {
			ref := l.Ref()
			if seen[ref] {
				continue
			}
			seen[ref] = true
			if n, ok := kb.ResolveLink(l, nodes); ok {
				out = append(out, meetLinkRef{Ref: n.Ref(), Title: n.Title, Status: "resolved"})
				continue
			}
			out = append(out, meetLinkRef{Ref: ref, Status: l.Status()})
		}
	}
	return out
}

func repoLinkNodes() []kb.LinkNode {
	root, ok := scope.FindGitRoot()
	if !ok {
		return nil
	}
	var nodes []kb.LinkNode
	if pages, err := kb.Open(filepath.Join(root, kb.RepoSub)).List(); err == nil {
		nodes = append(nodes, kb.KBNodes(pages)...)
	}
	if todos, err := kb.TodoNodesFromDir(filepath.Join(root, "docs", "todo")); err == nil {
		nodes = append(nodes, todos...)
	}
	return nodes
}

func printMeetingLinks(w io.Writer, links []meetLinkRef) {
	fmt.Fprintf(w, "\nlinks (%d)\n", len(links))
	for _, l := range links {
		fmt.Fprintf(w, "  -> %s  %s  %s\n", l.Ref, emptyDash(l.Title), l.Status)
	}
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "-"
	}
	return s
}

func newContributionsCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:     "contributions <id> [participant]",
		Aliases: []string{"contrib"},
		Short:   "print every contribution in full, optionally filtered to one participant",
		Args:    cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := loadState(args[0])
			if err != nil {
				return err
			}
			who := ""
			if len(args) == 2 {
				who = args[1]
			}
			events, _ := readTranscript(st.ID)
			w := cmd.OutOrStdout()
			if jsonOut {
				var sel []Event
				for _, e := range events {
					if e.Kind != "turn" && e.Kind != "vote" && e.Kind != "human" {
						continue
					}
					if who != "" && !strings.EqualFold(e.Speaker, who) {
						continue
					}
					e.Text = redactHome(e.Text)
					e.File = redactHome(e.File)
					sel = append(sel, e)
				}
				enc := json.NewEncoder(w)
				enc.SetIndent("", "  ")
				return enc.Encode(sel)
			}
			writeContributions(w, st, events, who)
			return nil
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the contributions as JSON")
	return cmd
}

func newListCmd() *cobra.Command {
	var as string
	cmd := &cobra.Command{
		Use:   "list",
		Short: "list saved meetings",
		Long: "List saved meetings.\n\n" +
			"ROOM is the short number you attach by: `bashy meet observe 2`. NAME is\n" +
			"a stable configured address such as `bashy meet observe steward`. ROOM is\n" +
			"assigned from the lowest free number among the OPEN meetings and reused\n" +
			"once a meeting closes, exactly like a shell's job numbers.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			sessions, err := openRooms()
			if err != nil {
				return err
			}
			reader := canonAgent(strings.TrimSpace(strings.TrimPrefix(as, "@")))
			if reader != "" {
				return writeGroupedRoomList(cmd, sessions, reader)
			}
			w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(w, "ROOM\tNAME\tID\tSTATUS\tPARTICIPANTS\tTOPIC")
			for _, s := range sessions {
				name := s.Name
				if name == "" {
					name = "-"
				}
				fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
					roomLabel(s), name, s.ID, s.Status, strings.Join(s.Participants, ","), s.Topic)
			}
			if err := w.Flush(); err != nil {
				return err
			}
			if len(sessions) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no meetings on this host")
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "show grouped rooms and unread counts for NAME (does not mark messages read)")
	return cmd
}

type roomListGroup struct {
	heading string
	rooms   []*State
}

func writeGroupedRoomList(cmd *cobra.Command, sessions []*State, reader string) error {
	groups := []roomListGroup{
		{heading: "STANDING CHANNELS"},
		{heading: "AD-HOC ROOMS"},
		{heading: "DIRECT MESSAGES"},
	}
	for _, st := range sessions {
		switch {
		case st.Permanent && strings.HasPrefix(st.Name, "dm-"):
			groups[2].rooms = append(groups[2].rooms, st)
		case st.Permanent:
			groups[0].rooms = append(groups[0].rooms, st)
		default:
			groups[1].rooms = append(groups[1].rooms, st)
		}
	}

	w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
	for i, group := range groups {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintln(w, group.heading)
		fmt.Fprintln(w, "ROOM\tNAME\tSTATUS\tUNREAD\tPARTICIPANTS\tTOPIC")
		for _, st := range group.rooms {
			name := st.Name
			if name == "" {
				name = "-"
			}
			unread := "-"
			if st.board() && participantSeat(st, reader) {
				directed, other, _, err := Unread(st.ID, reader, 0)
				if err != nil {
					return err
				}
				unread = strconv.Itoa(len(directed) + len(other))
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\n",
				roomLabel(st), name, st.Status, unread, strings.Join(st.attendees(), ","), st.Topic)
		}
		if len(group.rooms) == 0 {
			fmt.Fprintln(w, "-\t-\t-\t-\t-\t-")
		}
	}
	return w.Flush()
}

var dmPeerLive = func(agent string) (bool, error) {
	_, found, err := room.Find(agent)
	return found, err
}

func newDMCmd() *cobra.Command {
	var as string
	cmd := &cobra.Command{
		Use:   "dm <agent>",
		Short: "open or reuse a direct conversation using derived peer presence",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			peer := canonAgent(strings.TrimSpace(strings.TrimPrefix(args[0], "@")))
			if err := routableSeat(peer); err != nil {
				return err
			}
			caller := canonAgent(strings.TrimSpace(strings.TrimPrefix(as, "@")))
			if caller == "" {
				caller = canonAgent(strings.TrimSpace(os.Getenv("BASHY_AGENT_ID")))
			}
			if caller == "" {
				return fmt.Errorf("meet: dm needs the calling agent identity in --as NAME or BASHY_AGENT_ID")
			}
			if err := routableSeat(caller); err != nil {
				return fmt.Errorf("meet: DM caller: %w", err)
			}
			if strings.EqualFold(caller, peer) {
				return fmt.Errorf("meet: a direct message needs two distinct seats")
			}

			live, err := dmPeerLive(peer)
			if err != nil {
				return fmt.Errorf("meet: derive presence for %s: %w", peer, err)
			}
			if !live {
				dm, err := ensureRelayDM(peer, caller)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Meet DM with %s · cold · Chat spawns the counterpart when you send a message\n", dm.Agent)
				return nil
			}

			name, err := directMessageRoomName(caller, peer)
			if err != nil {
				return err
			}
			st, err := EnsurePermanentRoom(name, CreateOptions{
				Topic:        "Direct message: " + caller + " ↔ " + peer,
				Participants: []string{caller, peer}, Initiator: caller,
				Board: true, NoSecretary: true, Out: OutStore,
			})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "board DM @%s · room %s · %s is live; messages wait for its next `meet read`\n",
				st.Name, roomLabel(st), peer)
			return nil
		},
	}
	cmd.Flags().StringVar(&as, "as", "", "agent identity opening the DM (default: BASHY_AGENT_ID)")
	return cmd
}

func newResumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume <room>|<id>",
		Short: "reopen a saved meeting in the REPL",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, err := resolveMeeting(args[0])
			if err != nil {
				return err
			}
			st, err := loadState(id)
			if err != nil {
				return err
			}
			markDepth()
			return repl(cmd, st)
		},
	}
}
