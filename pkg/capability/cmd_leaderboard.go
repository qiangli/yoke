package capability

// `bashy leaderboard` — the fleet's own run evidence, rendered.
//
// A top-level verb rather than `capability leaderboard`, because it answers a
// different question from the matrix. The matrix is a ROUTING input ("who
// should take this"); the leaderboard is an ACCOUNT ("what has actually
// happened, and how sure are we"). Collapsing them would invite reading a
// routing estimate as a measurement, which is the exact confusion the tiers
// exist to prevent.
//
// Every emitter carries the provenance banner. A table of agent names and
// percentages, detached from the sentence that says these are one host's runs
// on its own repos under its own gates, is a benchmark claim — and it is not
// one we can support.

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// Provenance is stamped on every rendering. It is not boilerplate: without it
// the artifact reads as a comparable score, and the numbers cannot carry that.
const Provenance = "host-local fleet evidence — one host, its repos, its gates. NOT a public benchmark."

// NewLeaderboardCmd builds the `leaderboard` verb.
func NewLeaderboardCmd() *cobra.Command {
	var (
		asJSON, asMD bool
		minSamples   int
		since        time.Duration
		role         string
	)
	cmd := &cobra.Command{
		Use:   "leaderboard",
		Short: "rank this host's agents on the runs they actually completed",
		Long: `leaderboard ranks agents on evidence from runs that actually happened here.

It is NOT a benchmark. These are one host's runs, on its own repos, under its own
gates — useful because they are the only numbers produced by the work being done,
and not comparable with anyone else's.

Three tiers, and only the first is an ordering:

  RANKED    enough gated runs to order, by the WILSON 95% LOWER BOUND on gate
            pass rate. A point estimate would make one lucky run the fleet
            leader; Wilson pulls a small sample toward zero by exactly how
            little is known, so 1/1 scores 0.21 and 90/100 scores 0.83.

  OBSERVED  evidence exists but is too thin to order. n is shown; the print
            order is alphabetical so it cannot be read as a ranking.

  PRIOR     a seeded research estimate and nothing else. Printed so the fleet is
            fully enumerated — an agent missing from a leaderboard is
            indistinguishable from one that failed.

A run whose gate never ran counts in NEITHER numerator nor denominator. Absence
of evidence is not a failure, and ranking an agent down for a harness crash is
how a leaderboard stops describing agents.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			recs, err := ReadLedger()
			if err != nil {
				return fmt.Errorf("leaderboard: reading the run ledger: %w", err)
			}
			recs = Since(recs, since, time.Now())

			// A matrix that will not load is not fatal: the ledger is the
			// ranking source, and the matrix only enumerates the rest of the
			// fleet. Degrade to what can be shown.
			m, _ := Load()

			board := Compute(recs, ComputeOptions{MinSamples: minSamples, Role: role, Matrix: m})
			out := cmd.OutOrStdout()
			switch {
			case asJSON:
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				return enc.Encode(board)
			case asMD:
				return renderMarkdown(out, board, since)
			default:
				return renderTable(out, board)
			}
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable board (bashy-leaderboard-v1)")
	cmd.Flags().BoolVar(&asMD, "md", false, "markdown, for the published doc")
	cmd.Flags().IntVar(&minSamples, "min-samples", DefaultMinSamples, "gated runs required before an agent is ranked")
	cmd.Flags().DurationVar(&since, "since", 0, "ignore records older than this (0 = all history)")
	cmd.Flags().StringVar(&role, "role", "", "restrict to one seat (steward, conductor, coder, tester, agent-user)")
	return cmd
}

func tiers(board Board) (ranked, observed, prior []Standing) {
	for _, s := range board.Standings {
		switch s.Tier {
		case TierRanked:
			ranked = append(ranked, s)
		case TierObserved:
			observed = append(observed, s)
		default:
			prior = append(prior, s)
		}
	}
	return
}

func renderTable(w io.Writer, board Board) error {
	ranked, observed, prior := tiers(board)
	fmt.Fprintf(w, "bashy leaderboard — %s\n", Provenance)
	fmt.Fprintf(w, "%d ledger records · %d agents · ranked at n>=%d\n\n", board.Records, len(board.Standings), board.MinSamples)

	if len(ranked) == 0 {
		fmt.Fprintf(w, "RANKED: none — no agent has reached %d gated runs in the ledger.\n\n", board.MinSamples)
	} else {
		fmt.Fprintln(w, "RANKED (by Wilson 95% lower bound on gate pass rate)")
		fmt.Fprintf(w, "  %-28s %8s %7s %6s %8s\n", "agent", "wilson", "rate", "n", "repeat")
		for _, s := range ranked {
			fmt.Fprintf(w, "  %-28s %8.3f %6.0f%% %6d %8s\n",
				s.Agent, s.WilsonLB, s.PassRate*100, s.GatedRuns, repeatCell(s))
		}
		fmt.Fprintln(w)
	}

	if len(observed) > 0 {
		fmt.Fprintln(w, "OBSERVED, NOT RANKED (too little evidence to order; alphabetical)")
		for _, s := range observed {
			fmt.Fprintf(w, "  %-28s %s\n", s.Agent, observedNote(s))
		}
		fmt.Fprintln(w)
	}
	if len(prior) > 0 {
		fmt.Fprintln(w, "PRIOR — NOT EVIDENCE (seeded estimate; this host has run nothing)")
		for _, s := range prior {
			fmt.Fprintf(w, "  %-28s prior quality %.2f\n", s.Agent, s.PriorQuality)
		}
		fmt.Fprintln(w)
	}
	if board.PreLedger > 0 {
		fmt.Fprintf(w, "NOTE: %d agent(s) carry matrix evidence from runs that predate the run ledger.\n"+
			"      An EMA cannot be un-averaged into a pass count, so they are observed and\n"+
			"      never ranked. Their absence from the ranking is a RECORDING gap, not a\n"+
			"      performance one.\n", board.PreLedger)
	}
	return nil
}

func renderMarkdown(w io.Writer, board Board, since time.Duration) error {
	ranked, observed, prior := tiers(board)
	window := "all history"
	if since > 0 {
		window = "last " + since.String()
	}

	fmt.Fprintln(w, "<!-- GENERATED by `bashy leaderboard --md`. Do not edit by hand. -->")
	fmt.Fprintln(w, "# bashy agent leaderboard")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "**%s**\n\n", Provenance)
	fmt.Fprintf(w, "Evidence window: %s · %d ledger records · %d agents · ranked at n≥%d.\n\n",
		window, board.Records, len(board.Standings), board.MinSamples)
	fmt.Fprintln(w, "Ordering is the **Wilson 95% lower bound** on gate pass rate, not the rate itself:")
	fmt.Fprintln(w, "a point estimate makes one lucky run the fleet leader. A run whose gate never ran")
	fmt.Fprintln(w, "counts in neither numerator nor denominator — absence of evidence is not a failure.")
	fmt.Fprintln(w)

	fmt.Fprintln(w, "## Ranked")
	fmt.Fprintln(w)
	if len(ranked) == 0 {
		fmt.Fprintf(w, "*None.* No agent has reached %d gated runs in the run ledger.\n\n", board.MinSamples)
	} else {
		fmt.Fprintln(w, "| # | agent | wilson LB | pass rate | n | repeat | cost idx |")
		fmt.Fprintln(w, "|---|---|---|---|---|---|---|")
		for i, s := range ranked {
			fmt.Fprintf(w, "| %d | `%s` | **%.3f** | %.0f%% | %d | %s | %s |\n",
				i+1, s.Agent, s.WilsonLB, s.PassRate*100, s.GatedRuns, repeatCell(s), costCell(s))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## Observed, not ranked")
	fmt.Fprintln(w)
	if len(observed) == 0 {
		fmt.Fprintln(w, "*None.*")
		fmt.Fprintln(w)
	} else {
		fmt.Fprintln(w, "Evidence exists but is too thin to order. Listed alphabetically — the order")
		fmt.Fprintln(w, "carries no meaning.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| agent | evidence |")
		fmt.Fprintln(w, "|---|---|")
		for _, s := range observed {
			fmt.Fprintf(w, "| `%s` | %s |\n", s.Agent, observedNote(s))
		}
		fmt.Fprintln(w)
	}

	fmt.Fprintln(w, "## Prior — not evidence")
	fmt.Fprintln(w)
	if len(prior) == 0 {
		fmt.Fprintln(w, "*None.*")
	} else {
		fmt.Fprintln(w, "Seeded research estimates. This host has run these agents through no gate,")
		fmt.Fprintln(w, "so nothing here is a measurement. They are listed because an agent missing")
		fmt.Fprintln(w, "from a leaderboard is indistinguishable from one that failed.")
		fmt.Fprintln(w)
		fmt.Fprintln(w, "| agent | prior quality |")
		fmt.Fprintln(w, "|---|---|")
		for _, s := range prior {
			fmt.Fprintf(w, "| `%s` | %.2f |\n", s.Agent, s.PriorQuality)
		}
	}
	fmt.Fprintln(w)

	if board.PreLedger > 0 {
		fmt.Fprintln(w, "## Why the ranked table is thin")
		fmt.Fprintln(w)
		fmt.Fprintf(w, "%d agent(s) carry capability-matrix evidence from runs that happened **before the\n", board.PreLedger)
		fmt.Fprintln(w, "run ledger existed**. The matrix stores an exponential moving average, and an EMA")
		fmt.Fprintln(w, "cannot be un-averaged into a pass count — so those runs cannot produce a")
		fmt.Fprintln(w, "confidence interval and the agents stay in *observed*. Their absence from the")
		fmt.Fprintln(w, "ranking is a **recording gap, not a performance one**, and it closes on its own as")
		fmt.Fprintln(w, "new gated runs land.")
		fmt.Fprintln(w)
	}
	return nil
}

// repeatCell renders loop discipline, and renders it as unknown when no
// events-mode record exists. Printing 0.0 would read as "no repetition", which
// is the opposite of what an absent measurement means.
func repeatCell(s Standing) string {
	if s.RepeatSamples == 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f×", s.RepeatRatio)
}

func costCell(s Standing) string {
	if s.CostIndex <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", s.CostIndex)
}

// observedNote says WHY a row is unranked, because "observed" alone leaves a
// reader guessing between "barely ran" and "ran a lot, before we recorded it".
func observedNote(s Standing) string {
	var parts []string
	if s.GatedRuns > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d gated runs passed", s.Passes, s.GatedRuns))
	}
	if s.MatrixSamples > 0 {
		parts = append(parts, fmt.Sprintf("%d pre-ledger matrix samples (EMA, no pass count recoverable)", s.MatrixSamples))
	}
	if s.Reviewed > 0 {
		parts = append(parts, fmt.Sprintf("%d/%d reviews survived", s.Survived, s.Reviewed))
	}
	if len(parts) == 0 {
		return "no gated evidence"
	}
	return strings.Join(parts, "; ")
}
