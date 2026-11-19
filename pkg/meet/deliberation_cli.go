package meet

import (
	"encoding/json"
	"github.com/spf13/cobra"
	"time"
)

func newDeliberationCmd() *cobra.Command {
	var p DeliberationPlan
	var actor string
	cmd := &cobra.Command{Use: "deliberate ROOM", Short: "Bounded independent proposals, critique and targeted evidence consultation", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if actor == "" {
			actor = humanName()
		}
		r, err := Deliberate(cmd.Context(), args[0], actor, p, nil)
		if e := json.NewEncoder(cmd.OutOrStdout()).Encode(r); e != nil {
			return e
		}
		return err
	}}
	f := cmd.Flags()
	f.StringVar(&actor, "as", "", "Organizer identity")
	f.StringVar(&p.Question, "question", "", "Question to deliberate")
	f.StringSliceVar(&p.Proposers, "proposers", nil, "Independent proposer seats (at least two)")
	f.StringSliceVar(&p.Experts, "experts", nil, "Eligible targeted evidence experts")
	f.StringVar(&p.Critic, "critic", "", "Designated critic seat")
	f.StringVar(&p.Facilitator, "facilitator", "", "Designated facilitator seat")
	f.IntVar(&p.MaxTurns, "max-turns", 8, "Hard invocation budget including facilitation")
	f.DurationVar(&p.Duration, "duration", 5*time.Minute, "Hard total wall-clock budget")
	f.DurationVar(&p.TurnTimeout, "turn-timeout", time.Minute, "Hard per-turn deadline")
	return cmd
}
