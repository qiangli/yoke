package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"
)

// NewCoachCmd runs one agent on one task under the LLM-free auto-coach: it
// watches the tool.call stream and, when the agent starts looping, ESCs it out
// and tells it to deliver. The coach is a report channel — it can interrupt and
// speak, never write. See dhnt live-agent-coaching design (P0).
func NewCoachCmd() *cobra.Command {
	var (
		agent       string
		message     string
		cwd         string
		repeat      int
		ratio       float64
		minCalls    int
		cooldown    int
		maxSteers   int
		steer       string
		noInt       bool
		quiet       time.Duration
		timeout     time.Duration
		logPath     string
		readOnly    bool
		allowUnsafe bool
		asJSON      bool
	)
	cmd := &cobra.Command{
		Use:   "coach --agent AGENT -m TASK",
		Short: "run an agent under an LLM-free auto-coach that steers it off doomed loops",
		Long: "Coach starts a steerable session, watches for a doomed loop, and when the agent " +
			"loops (re-issuing the same call without progress) it presses ESC to break the loop " +
			"and tells it to deliver. LLM-free; a report channel, never an author.\n\n" +
			"It watches through one of two signals: the STRUCTURED event channel when the tool " +
			"reports one (precise tool.call tracking — e.g. ycode), or the GENERIC pty-scrape " +
			"loop signal otherwise (output flowing but distinct content not growing — works for " +
			"any tool, including event-less ones like agy and any third-party CLI). Both signals " +
			"share one trip implementation.\n\n" +
			"To coach a session that is ALREADY RUNNING (instead of launching one), use " +
			"`coach attach <session>` — see `coach attach --help`.",
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			if agent == "" || message == "" {
				return fmt.Errorf("coach: --agent and -m/--message are required")
			}
			pol := DefaultCoachPolicy()
			if repeat > 0 {
				pol.RepeatThreshold = repeat
			}
			if ratio > 0 {
				pol.RatioThreshold = ratio
			}
			if minCalls > 0 {
				pol.MinCalls = minCalls
			}
			if cooldown >= 0 {
				pol.Cooldown = cooldown
			}
			if maxSteers > 0 {
				pol.MaxSteers = maxSteers
			}
			if steer != "" {
				pol.Steer = steer
			}
			pol.Interrupt = !noInt
			pol.LogPath = logPath

			// A cascade agent (X4) is not launched directly: run its cheap BASE
			// and arm the EXPLICIT escalation ladder (e.g. glm-5.2 → terra → sol)
			// so premium help is called in only when the base has demonstrably
			// stalled. A plain agent uses the auto-picked band-graduated coach.
			launchAgent := agent
			escalator := BandGraduatedEscalator
			if base, ladder, ok := cascadeLadder(agent); ok {
				launchAgent = base
				escalator = LadderEscalator(ladder)
				fmt.Fprintf(os.Stderr, "coach: cascade %q → base %q, escalation ladder %v\n", agent, base, ladder)
			}

			// Enforce --timeout as a hard context deadline. Without it, WaitIdle
			// blocks forever on a turn.end that a stuck agent never sends — the coach
			// exists precisely for agents that loop, so an unbounded wait is the one
			// thing it must not do.
			base := cmd.Context()
			var ctx context.Context
			var cancel context.CancelFunc
			if timeout > 0 {
				ctx, cancel = context.WithTimeout(base, timeout)
			} else {
				ctx, cancel = context.WithCancel(base)
			}
			defer cancel()

			sess, err := Start(ctx, launchAgent, SessionOptions{
				Prompt:      message,
				Cwd:         cwd,
				Timeout:     timeout,
				Stream:      os.Stderr, // the agent's own output tees to stderr
				ReadOnly:    readOnly,
				AllowUnsafe: allowUnsafe && !readOnly,
			})
			if err != nil {
				return fmt.Errorf("coach: start %s: %w", launchAgent, err)
			}
			defer sess.Close()

			if sess.EventsPath() == "" {
				fmt.Fprintf(os.Stderr, "coach: %s has no event channel — using the GENERIC pty-scrape loop signal (imprecise; a first-party tool like ycode gives precise tool.call tracking)\n", agent)
			}

			// Agent exit cancels the context, so a converged run returns at once
			// instead of waiting out the whole timeout.
			go func() { _, _ = sess.Wait(); cancel() }()

			coach := sess.StartCoach(ctx, pol)
			coach.SetEscalation(ctx, launchAgent, escalator)

			// Drive the single task to completion: wait for the turn to end (the
			// tool reports it), then read the answer.
			werr := sess.WaitIdle(ctx, quiet)
			answer := sess.Turn()
			// The turn is over — tear down NOW. The coach's watch goroutine loops
			// until ctx.Done(), and a steerable TUI agent never exits on its own
			// (the early cancel-on-agent-exit never fires), so without this
			// coach.Wait() would block until the whole --timeout even though
			// WaitIdle returned on turn.end. Cancelling ends the watcher and kills
			// the lingering agent at once.
			cancel()
			coach.Wait()
			rep := coach.Report()

			// A hit deadline / cancel is the EXPECTED end of a coached run — the
			// budget ran out, or (until ycode emits turn.end in the steerable path)
			// there was no turn boundary to return on. It is not a command failure,
			// so it must not print usage or exit non-zero.
			if werr == context.DeadlineExceeded || werr == context.Canceled {
				werr = nil
			}

			if asJSON {
				out := struct {
					Agent  string      `json:"agent"`
					Answer string      `json:"answer"`
					Coach  CoachReport `json:"coach"`
				}{Agent: agent, Answer: answer, Coach: rep}
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
				return werr
			}

			fmt.Fprintf(os.Stderr, "\n── coach report (%s) ──\n", agent)
			fmt.Fprintf(os.Stderr, "tool calls: %d total / %d distinct (repeat %.2f)\n", rep.Total, rep.Distinct, rep.Repeat)
			fmt.Fprintf(os.Stderr, "steers: %d\n", len(rep.Steers))
			for i, s := range rep.Steers {
				fmt.Fprintf(os.Stderr, "  %d. [%s] at repeat=%.2f (call seen %d×): %q\n", i+1, s.Reason, s.Repeat, s.Count, s.Steer)
			}
			if answer != "" {
				fmt.Println(answer)
			}
			return werr
		},
	}
	f := cmd.Flags()
	f.StringVar(&agent, "agent", "", "agent to coach (a fleet binding/nick, e.g. ycode-glm-5.2)")
	f.StringVarP(&message, "message", "m", "", "the task prompt")
	f.StringVar(&cwd, "cwd", "", "working directory for the agent")
	f.IntVar(&repeat, "repeat", 0, "trip when one identical call is issued this many times (default 3)")
	f.Float64Var(&ratio, "ratio", 0, "trip when total/distinct tool calls reach this (default 3.0)")
	f.IntVar(&minCalls, "min-calls", 0, "suppress any steer before this many calls (default 3)")
	f.IntVar(&cooldown, "cooldown", -1, "distinct calls required between steers (default 2)")
	f.IntVar(&maxSteers, "max-steers", 0, "hard cap on interventions (default 3)")
	f.StringVar(&steer, "steer", "", "override the steer line")
	f.BoolVar(&noInt, "no-interrupt", false, "do not ESC before speaking (probe: a plain Say does not land mid-loop)")
	f.DurationVar(&quiet, "quiet", 25*time.Second, "idle window that ends the turn when the tool has no event channel")
	f.DurationVar(&timeout, "timeout", 0, "overall session timeout, e.g. 15m")
	f.StringVar(&logPath, "log", "", "append one JSON line per steer here (the training record)")
	f.BoolVar(&readOnly, "read-only", false, "strip write authority (a talk-only session)")
	f.BoolVar(&allowUnsafe, "yolo", false, "disable the agent's approval gate and accept the uncontained-host risk")
	f.BoolVar(&asJSON, "json", false, "print a JSON result with the coach report")

	// attach: coach a session that is ALREADY RUNNING, rather than launching one.
	cmd.AddCommand(newCoachAttachCmd())
	return cmd
}
