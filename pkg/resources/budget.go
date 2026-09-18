package resources

import (
	"bytes"
	"fmt"
	"strconv"
	"text/tabwriter"
	"time"

	"github.com/qiangli/yoke/pkg/llmbudget"
)

// BudgetReport is the `resources budget` payload: what the local LLM meter has
// spent and what is left, per model. It rides the same bashy-resources-v1
// envelope as the fleet and system readings and is distinguished by Kind, so
// the Periscope web UI and the ycode TUI parse one schema, not two.
type BudgetReport struct {
	SchemaVersion string                   `json:"schema_version"`
	Kind          string                   `json:"kind"`
	GeneratedAt   time.Time                `json:"generated_at"`
	Models        []llmbudget.BudgetStatus `json:"models"`
	// LimitsKnown counts the entries with a known ceiling. When it is less
	// than len(Models) some readings are UNKNOWN — neither exhausted nor
	// unlimited — and a renderer must say so rather than pick a default.
	LimitsKnown int `json:"limits_known"`
}

// CollectBudget reads the local meter. It is READ-ONLY: no counters move, no
// state is saved, no gate decision is taken.
func CollectBudget(at time.Time, models []string) *BudgetReport {
	if at.IsZero() {
		at = time.Now().UTC()
	}
	var rows []llmbudget.BudgetStatus
	if len(models) == 0 {
		rows = llmbudget.StatusAll()
	} else {
		for _, m := range models {
			rows = append(rows, llmbudget.Status(m))
		}
	}
	known := 0
	for _, r := range rows {
		if r.LimitKnown {
			known++
		}
	}
	return &BudgetReport{
		SchemaVersion: SchemaVersion,
		Kind:          "budget",
		GeneratedAt:   at,
		Models:        rows,
		LimitsKnown:   known,
	}
}

// FormatBudgetTable renders the human view. Unknown ceilings print "unknown",
// never "0" and never "∞".
func FormatBudgetTable(br *BudgetReport) string {
	var out bytes.Buffer
	w := tabwriter.NewWriter(&out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(w, "MODEL\tLANE\tBASIS\tUNIT\tSPENT\tLIMIT\tREMAINING")
	for _, m := range br.Models {
		limit, remaining := "unknown", "unknown"
		if m.LimitKnown {
			limit = strconv.FormatInt(*m.Limit, 10)
			remaining = strconv.FormatInt(*m.Remaining, 10)
		}
		lane := string(m.Lane)
		if lane == "" {
			lane = "-"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\t%s\n",
			m.Model, lane, m.Basis, m.Unit, m.Spent, limit, remaining)
	}
	if len(br.Models) == 0 {
		fmt.Fprintln(w, "(no recorded LLM usage on this host)")
	}
	_ = w.Flush()
	if br.LimitsKnown < len(br.Models) {
		fmt.Fprintf(&out, "\n%d of %d models have no known ceiling: unknown is neither exhausted nor unlimited.\n",
			len(br.Models)-br.LimitsKnown, len(br.Models))
	}
	return out.String()
}
