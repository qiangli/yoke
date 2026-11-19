package meet

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

func newRouteCmd() *cobra.Command {
	var req ResponsibilityRequest
	var digest []string
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "route <question>",
		Short: "ask one accountable seat and send sourced digests to observers",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req.Question = args[0]
			for _, raw := range digest {
				parts := strings.SplitN(raw, "|", 3)
				if len(parts) != 3 {
					return fmt.Errorf("meet route: --digest requires authority|source|text")
				}
				req.Digest = append(req.Digest, DigestItem{Authority: parts[0], Source: parts[1], Text: parts[2]})
			}
			receipt, err := RouteResponsibility(req)
			if jsonOut {
				if encodeErr := json.NewEncoder(cmd.OutOrStdout()).Encode(receipt); encodeErr != nil {
					return encodeErr
				}
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "question → %s: %s (%s)\n", receipt.Question.To, receipt.Question.State, receipt.Question.Channel)
				for _, d := range receipt.Observers {
					fmt.Fprintf(cmd.OutOrStdout(), "digest → %s: %s (%s)\n", d.To, d.State, d.Channel)
				}
				if receipt.Escalation != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "escalation → %s: %s (%s)\n", receipt.Escalation.To, receipt.Escalation.State, receipt.Escalation.Channel)
				}
				if receipt.Fallback != "" {
					fmt.Fprintln(cmd.OutOrStdout(), "fallback: "+receipt.Fallback)
				}
			}
			return err
		},
	}
	cmd.Flags().StringVar(&req.Room, "room", "", "deliver through this Meet room; default is direct Bus mail")
	cmd.Flags().StringVar(&req.OperationID, "id", "", "stable operation ID; reuse it when retrying")
	cmd.Flags().StringVar(&req.From, "as", "", "sender seat")
	cmd.Flags().StringVar(&req.Accountable, "accountable", "", "one accountable agent or role")
	cmd.Flags().StringSliceVar(&req.Observers, "observer", nil, "named observer (repeatable)")
	cmd.Flags().StringArrayVar(&digest, "digest", nil, "authority|source|text (repeatable)")
	cmd.Flags().StringVar(&req.EscalateTo, "escalate-to", "", "distinct fallback seat")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print structured delivery and escalation receipt")
	return cmd
}
