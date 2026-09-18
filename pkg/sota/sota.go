// Package sota is `bashy sota` — the research capability (P1). It grounds a
// synthesis agent in REAL sources from `bashy search` and instructs it to cite
// ONLY those URLs, so citations are real by construction rather than by trust —
// the anti-hallucination discipline built in, not bolted on. When no search
// backend is configured it can HITCHHIKE on the agent's own web-search tool
// (expensive but reliable), the honest fallback.
package sota

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/chat"
	"github.com/qiangli/yoke/pkg/search"
	"github.com/qiangli/yoke/pkg/telemetry"
)

// NewSotaCmd returns `bashy sota QUESTION` — research the current state of the
// art and return a cited, date-grounded report.
func NewSotaCmd() *cobra.Command {
	var (
		agent        string
		maxSrc       int
		hitchhike    bool
		gate         bool
		maxAge       time.Duration
		noCache      bool
		noVerify     bool
		verifySample int
		asJSON       bool
		timeout      time.Duration
	)
	cmd := &cobra.Command{
		Use:   "sota QUESTION...",
		Short: "research the current state of the art on a topic (cited, date-grounded report)",
		Long: "sota grounds a synthesis agent in REAL web-search results (`bashy search`) and asks\n" +
			"it to cite ONLY those URLs — so citations are real by construction. With --hitchhike\n" +
			"(or no search backend) the agent uses its OWN web-search tool instead (reliable, costlier).",
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(c *cobra.Command, args []string) error {
			q := strings.Join(args, " ")
			ctx := c.Context()
			store := kbStore()
			slug := slugify(q)

			// STEWARD GATE: a design/direction decision must be SOTA-grounded. The
			// gate is a CHECK (exit 0/1), not a ritual — it passes iff fresh research
			// already exists, and points at how to get it otherwise.
			if gate {
				if p, ok := cachedFresh(store, slug, maxAge); ok {
					fmt.Fprintf(os.Stderr, "sota gate: OK — fresh research exists for %q (updated %s)\n", q, firstNonEmpty(p.Updated, p.Created))
					return nil
				}
				return fmt.Errorf("sota gate: NO fresh research for %q — run `bashy sota %q` before committing this design", q, q)
			}

			// CACHE REUSE: research once, many read; do not re-spend on a fresh topic.
			if !noCache {
				if p, ok := cachedFresh(store, slug, maxAge); ok {
					telemetry.Provenance(ctx, "sota.cache_hit", 1, "kb")
					fmt.Fprintf(os.Stderr, "sota: cached (updated %s; --no-cache to refresh)\n", firstNonEmpty(p.Updated, p.Created))
					fmt.Println(p.Body)
					return nil
				}
			}

			var sources []search.Result
			var backend string
			if !hitchhike {
				var err error
				sources, backend, err = search.Web(ctx, q, search.Options{MaxResults: maxSrc})
				if err != nil {
					fmt.Fprintf(os.Stderr, "sota: no search backend (%v) — hitchhiking on the agent's web search\n", err)
					hitchhike = true
				}
			}
			if agent == "" {
				agent = "claude" // capable synthesizer, and has a web_search tool for hitchhike
			}

			prompt := buildPrompt(q, sources, hitchhike, time.Now().UTC())
			telemetry.Provenance(ctx, "sota.sources", int64(len(sources)), firstNonEmpty(backend, "agent"))

			// ReadOnly: research WRITES NOTHING — no file authority, which also clears
			// the uncontained-host launch guard by construction.
			res, err := chat.Invoke(ctx, chat.Options{
				Agent:       agent,
				Instruction: prompt,
				ReadOnly:    true,
				Timeout:     timeout,
			}, nil)
			if err != nil {
				return fmt.Errorf("sota: %w", err)
			}
			report := res.Output

			// CITATION VERIFY: fetch-check a sample of cited URLs actually resolve.
			// A citation that does not resolve is a plausible-but-unreal source — the
			// exact thing a research tool must never hide.
			if !noVerify {
				vs, okN, total, dead := verifyCitations(ctx, report, verifySample)
				report += vs
				telemetry.Provenance(ctx, "sota.citations_ok", int64(okN), "verify")
				if len(dead) > 0 {
					telemetry.BoundHit(ctx, "sota.dead_citations", int64(total), int64(len(dead)), q)
				}
			}

			// Cache as a kb page (reused + discoverable via `bashy search --kb`).
			if !noCache {
				if err := cacheReport(store, slug, q, report); err == nil && store != nil {
					telemetry.Provenance(ctx, "sota.cache_write", 1, "kb")
				}
			}

			if asJSON {
				out := struct {
					SchemaVersion string          `json:"schema_version"`
					Question      string          `json:"question"`
					Agent         string          `json:"agent"`
					Grounded      bool            `json:"grounded"`
					Slug          string          `json:"slug"`
					Sources       []search.Result `json:"sources,omitempty"`
					Report        string          `json:"report"`
				}{"bashy-sota-v1", q, agent, !hitchhike, slug, sources, report}
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			fmt.Println(report)
			return nil
		},
	}
	f := cmd.Flags()
	f.StringVar(&agent, "agent", "", "synthesis/research agent (default: claude)")
	f.IntVar(&maxSrc, "max", 8, "web sources to ground the report on")
	f.BoolVar(&hitchhike, "hitchhike", false, "skip `bashy search`; let the agent use its OWN web-search tool")
	f.BoolVar(&gate, "gate", false, "STEWARD GATE: exit 0 iff fresh research exists for the topic, else exit 1 (does not research)")
	f.DurationVar(&maxAge, "max-age", 7*24*time.Hour, "cached research older than this is stale (reuse + gate)")
	f.BoolVar(&noCache, "no-cache", false, "ignore the kb cache: always re-research")
	f.BoolVar(&noVerify, "no-verify", false, "skip the citation-resolves check")
	f.IntVar(&verifySample, "verify-sample", 12, "how many cited URLs to fetch-check")
	f.BoolVar(&asJSON, "json", false, "print a bashy-sota-v1 JSON envelope")
	f.DurationVar(&timeout, "timeout", 0, "agent timeout, e.g. 10m")
	return cmd
}

// buildPrompt is the sota-research procedure as a prompt: grounded in real
// sources, or instructing the agent to search itself.
func buildPrompt(q string, sources []search.Result, hitchhike bool, now time.Time) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Today's date is %s. Research the CURRENT state of the art on:\n\n  %s\n\n", now.Format("2006-01-02"), q)

	if hitchhike {
		b.WriteString("USE YOUR WEB SEARCH TOOL to find current, authoritative sources (prefer recent ones — your training may be stale). ")
		b.WriteString("Cite ONLY real URLs you actually retrieved; NEVER invent a URL.\n\n")
	} else {
		b.WriteString("Here are current web-search results (retrieved just now). Base the report on THESE, and cite ONLY these URLs — do not invent any:\n\n")
		for i, s := range sources {
			fmt.Fprintf(&b, "  [%d] %s\n      %s\n", i+1, s.Title, s.URL)
			if sn := strings.TrimSpace(s.Snippet); sn != "" {
				fmt.Fprintf(&b, "      %s\n", sn)
			}
		}
		b.WriteString("\n")
	}

	b.WriteString("Write a tight report:\n")
	b.WriteString("  1. A 2–3 sentence SUMMARY of where the field is now.\n")
	b.WriteString("  2. KEY FINDINGS as bullets, each ending with a [URL] citation.\n")
	b.WriteString("  3. Name the CORPUS (e.g. \"based on the N sources above\") and note any freshness caveats.\n")
	b.WriteString("Be specific and current. If the sources disagree or are thin, SAY SO — do not paper over gaps.")
	return b.String()
}

func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
