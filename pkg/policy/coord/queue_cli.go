package coord

import (
	"encoding/json"
	"fmt"
	"github.com/spf13/cobra"
	"text/tabwriter"
)

// NewQueueCommand exposes the same ledger through claim and resource reporting.
func NewQueueCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "queue", Short: "show host-local FIFO wait tickets, holders and wait reasons", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		rows, err := Queue(DefaultDir())
		if err != nil {
			return err
		}
		j, _ := cmd.Flags().GetBool("json")
		if j {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(rows)
		}
		w := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "TICKET\tTARGET\tWAITER\tPOSITION\tREASON\tINTENT")
		for _, r := range rows {
			fmt.Fprintf(w, "%s\t%s\t%s\t%d\t%s\t%s\n", r.ID, r.Ref, r.Holder.Name, r.Position, r.Reason, r.Intent)
		}
		return w.Flush()
	}}
	cmd.Flags().Bool("json", false, "emit queue snapshot including blocking claims and fencing epochs")
	cmd.AddCommand(&cobra.Command{Use: "cancel TICKET", Short: "cancel your pending ticket without releasing any hold", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error { return CancelWait(DefaultDir(), args[0], Self()) }})
	return cmd
}
