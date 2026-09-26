package stats

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

// NewCmd is `stats`: summary, paired and passk over results JSONL read from
// files or stdin.
func NewCmd() *cobra.Command {
	var f Fields
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "stats",
		Short: "Resolve rates with clustered CIs, paired differences, pass^k and cost per solve over results JSONL",
		Long: `stats is a unix filter over results JSONL: one record per attempt, field
names given by flags. Repeated attempts of an instance are clustered by
instance, so the standard error counts instances, not attempts.

  stats summary  [FILE...]              per-arm resolve rate, 95% CI, cost per solve
  stats paired   --a ARM --b ARM [FILE...]   B − A over shared instances, paired CI
  stats passk    --k K [FILE...]        pass^k (all k pass) and pass@k per arm

Reads stdin when no FILE is given.`,
	}
	pf := cmd.PersistentFlags()
	pf.StringVar(&f.Instance, "instance", "instance_id", "field naming the instance (the cluster)")
	pf.StringVar(&f.Arm, "arm", "", "field naming the arm (agent/config); empty = one arm")
	pf.StringVar(&f.Outcome, "outcome", "resolved", "field holding pass/fail (bool, 0/1, or pass/fail words)")
	pf.StringVar(&f.Cost, "cost", "", "field holding the attempt's cost (optional)")
	pf.BoolVar(&asJSON, "json", false, "emit JSON instead of a table")

	read := func(c *cobra.Command, files []string) ([]Attempt, error) {
		var readers []io.Reader
		if len(files) == 0 {
			readers = append(readers, c.InOrStdin())
		}
		for _, name := range files {
			fh, err := os.Open(name)
			if err != nil {
				return nil, err
			}
			defer fh.Close()
			readers = append(readers, fh)
		}
		attempts, err := Read(io.MultiReader(readers...), f)
		if err != nil {
			return nil, err
		}
		if len(attempts) == 0 {
			return nil, errors.New("stats: no records read")
		}
		return attempts, nil
	}
	emit := func(c *cobra.Command, v any, table func(w *tabwriter.Writer)) error {
		if asJSON {
			enc := json.NewEncoder(c.OutOrStdout())
			enc.SetIndent("", "  ")
			return enc.Encode(v)
		}
		w := tabwriter.NewWriter(c.OutOrStdout(), 0, 0, 2, ' ', 0)
		table(w)
		return w.Flush()
	}

	cmd.AddCommand(&cobra.Command{
		Use:   "summary [FILE...]",
		Short: "Per-arm resolve rate with a 95% CI clustered by instance, and cost per solve",
		RunE: func(c *cobra.Command, files []string) error {
			attempts, err := read(c, files)
			if err != nil {
				return err
			}
			rows := Summarize(attempts)
			return emit(c, map[string]any{"schema_version": "bashy-stats-summary-v1", "arms": rows}, func(w *tabwriter.Writer) {
				fmt.Fprintln(w, "ARM\tINSTANCES\tATTEMPTS\tPASSES\tRATE\t95% CI\tCOST/SOLVE")
				for _, r := range rows {
					cost := "-"
					if r.CostPerSolve != nil {
						cost = fmt.Sprintf("%.4g", *r.CostPerSolve)
					}
					fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%.3f\t[%.3f, %.3f]\t%s\n", r.Arm, r.Instances, r.Attempts, r.Passes, r.Rate.Mean, r.Rate.Low, r.Rate.High, cost)
				}
			})
		},
	})

	var a, b string
	paired := &cobra.Command{
		Use:   "paired --a ARM --b ARM [FILE...]",
		Short: "B − A over the instances both arms attempted, with a paired 95% CI",
		RunE: func(c *cobra.Command, files []string) error {
			if f.Arm == "" {
				return errors.New("stats paired: --arm names the field that distinguishes the arms")
			}
			if a == "" || b == "" {
				return errors.New("stats paired: --a and --b name the two arms")
			}
			attempts, err := read(c, files)
			if err != nil {
				return err
			}
			p, err := ComparePaired(attempts, a, b)
			if err != nil {
				return err
			}
			return emit(c, map[string]any{"schema_version": "bashy-stats-paired-v1", "paired": p}, func(w *tabwriter.Writer) {
				fmt.Fprintln(w, "B − A\tSHARED\tRATE A\tRATE B\tDIFF\t95% CI\tB BETTER/WORSE\tUNPAIRED SE")
				fmt.Fprintf(w, "%s − %s\t%d\t%.3f\t%.3f\t%+.3f\t[%+.3f, %+.3f]\t%d/%d\t%.3f\n", p.B, p.A, p.Shared, p.RateA, p.RateB, p.Diff.Mean, p.Diff.Low, p.Diff.High, p.Wins, p.Losses, p.UnpairedSE)
			})
		},
	}
	paired.Flags().StringVar(&a, "a", "", "baseline arm")
	paired.Flags().StringVar(&b, "b", "", "compared arm (the difference is B − A)")
	cmd.AddCommand(paired)

	var k int
	passk := &cobra.Command{
		Use:   "passk --k K [FILE...]",
		Short: "pass^k (all k attempts pass) and pass@k (at least one) per arm",
		RunE: func(c *cobra.Command, files []string) error {
			attempts, err := read(c, files)
			if err != nil {
				return err
			}
			rows, err := ComputePassK(attempts, k)
			if err != nil {
				return err
			}
			return emit(c, map[string]any{"schema_version": "bashy-stats-passk-v1", "arms": rows}, func(w *tabwriter.Writer) {
				fmt.Fprintln(w, "ARM\tK\tINSTANCES\tEXCLUDED\tPASS^K\tPASS@K")
				for _, r := range rows {
					fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%.3f\t%.3f\n", r.Arm, r.K, r.Instances, r.Excluded, r.PassHatK, r.PassAtK)
				}
			})
		},
	}
	passk.Flags().IntVar(&k, "k", 1, "attempts per trial")
	cmd.AddCommand(passk)
	return cmd
}
