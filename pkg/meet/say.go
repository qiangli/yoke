package meet

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/agentctl"
	"github.com/qiangli/yoke/pkg/fleet"
)

// A turn is a whole model call. Until now the only thing you could do with one
// that had gone wrong — a participant answering a question nobody asked, or
// restating its last turn at length — was wait it out, or kill it and lose
// everything it had already said.
//
// `meet say` is the third option: put a line in front of the agent WHILE it is
// still writing. It arrives as keystrokes on the agent's terminal, which is what
// the PTY and its control socket are for, and it is how a chair keeps a
// discussion on the agenda without ending anyone's turn.
//
// It only ever addresses an agent that currently HAS THE FLOOR. Steering one
// that has already finished would be a line typed into a closed room: accepted,
// acknowledged, and read by nobody.

func newSayCmd() *cobra.Command {
	var to string
	cmd := &cobra.Command{
		Use:   "say [<room>|<id>] <text>",
		Short: "steer the agent that is currently speaking (mid-turn)",
		Long: "Steer the agent that is currently speaking, without ending its turn.\n\n" +
			"The text reaches the agent as keystrokes on its terminal, so it lands\n" +
			"mid-answer: a facilitator can tell a participant that has wandered off the\n" +
			"agenda to come back to it, and keep everything it has already said.\n\n" +
			"Only an agent that currently holds the floor can be steered. There is\n" +
			"no queue: a line for an agent that has already finished would be typed\n" +
			"into an empty room.",
		Example: "  bashy meet say 2 \"stay on the gate question — you are re-litigating the schema\"\n" +
			"  bashy meet say 2 --to Sable \"give a number, not a range\"",
		Args:          cobra.RangeArgs(1, 2),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			var ref, text string
			if len(args) == 2 {
				ref, text = args[0], args[1]
			} else {
				text = args[0]
			}
			if strings.TrimSpace(text) == "" {
				return fmt.Errorf("meet: nothing to say")
			}

			var id string
			var err error
			if ref != "" {
				if id, err = resolveMeeting(ref); err != nil {
					return err
				}
			} else if id, err = latestOpenMeeting(); err != nil {
				return err
			}

			// A meeting whose turns are headless one-shots has nothing to steer. Say
			// so, rather than writing into a socket that will accept the bytes and
			// drop them: `say` used to print "→ Sable (round 2): stay on the gate
			// question" and deliver precisely nothing. A control channel that reports
			// success and delivers nothing is worse than one that refuses.
			if st, err := loadState(id); err == nil {
				if st.board() {
					return st.boardRefusal("steer")
				}
				if !st.Steerable {
					return fmt.Errorf("meet: this meeting's turns are headless one-shots — the agent runs its "+
						"prompt and exits, so there is nobody to interrupt.\n"+
						"Start a meeting with --steerable to hold each speaker open for its turn:\n"+
						"  bashy meet open --steerable --topic %q …", st.Topic)
				}
			}

			floor, err := currentSpeaker(id)
			if err != nil {
				return err
			}
			if to != "" && canonAgent(to) != floor.Speaker {
				return fmt.Errorf("meet: %s does not have the floor right now (%s is speaking) — "+
					"a steer only reaches an agent mid-turn", to, seatLabel(floor.Speaker))
			}
			if floor.CtlSock == "" {
				return fmt.Errorf("meet: %s has no control channel — it was launched without a PTY, "+
					"so there is no way to reach it mid-turn", seatLabel(floor.Speaker))
			}

			// Warn, but still send. A tool that does not DECLARE supports_say may
			// still read its terminal; refusing outright would be us guessing on the
			// registry's behalf. Saying so lets the operator interpret the silence.
			if !steerable(floor.Speaker) {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: %s does not declare supports_say — the line is delivered, but the tool may ignore it\n",
					seatLabel(floor.Speaker))
			}

			if err := agentctl.Say(floor.CtlSock, text); err != nil {
				return fmt.Errorf("meet: could not reach %s: %w", seatLabel(floor.Speaker), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "→ %s (round %d): %s\n",
				seatLabel(floor.Speaker), floor.Round, text)
			return nil
		},
	}
	cmd.Flags().StringVar(&to, "to", "", "refuse unless this seat is the one currently speaking")
	return cmd
}

// currentSpeaker returns the turn that currently holds the floor.
//
// The live channel already answers this: a `speaking` with no matching `spoke`
// IS the agent mid-turn. Reading it, rather than keeping a separate "who is
// talking" file, means the answer cannot drift from what an observer is watching.
func currentSpeaker(id string) (LiveEvent, error) {
	dir, err := storeDir(id)
	if err != nil {
		return LiveEvent{}, err
	}
	events, err := readLive(&lineTail{path: filepath.Join(dir, "live.jsonl")})
	if err != nil {
		return LiveEvent{}, err
	}
	var floor LiveEvent
	var held bool
	for _, e := range events {
		switch e.Kind {
		case liveSpeaking:
			floor, held = e, true
		case liveSpoke:
			if e.Speaker == floor.Speaker && e.Round == floor.Round {
				held = false
			}
		}
	}
	if !held {
		return LiveEvent{}, fmt.Errorf("meet: nobody is speaking right now — " +
			"a steer only reaches an agent mid-turn")
	}
	return floor, nil
}

// steerable reports whether the seat's tool declares that it reads its terminal
// mid-run. The registry is the source of truth; meet keeps no list of its own.
func steerable(seat string) bool {
	a, ok := fleet.New().Agent(seat)
	if !ok {
		return false
	}
	p, ok := agentctl.ProfileFor(a.Tool)
	return ok && p.Steerable
}
