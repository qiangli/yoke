package search

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/telemetry"
)

// NewSearchCmd is `bashy search` — the find-things primitive. P0a is web search
// through the provider ladder; `--local` (grep/find/ast/kb/graph) is P0b.
func NewSearchCmd() *cobra.Command {
	var (
		asJSON  bool
		local   bool
		content bool
		files   bool
		kb      bool
		dir     string
		max     int
		backend string
	)
	cmd := &cobra.Command{
		Use:   "search QUERY...",
		Short: "web search (query → cited results) — the find-things primitive",
		Long: "Search the web through a provider ladder (auto by available key, or --backend):\n" +
			"  1. tavily  (TAVILY_API_KEY)\n" +
			"  2. brave   (BRAVE_API_KEY)\n" +
			"  3. serper  (SERPER_API_KEY)\n" +
			"Keys come from the environment (project them with `eval \"$(bashy secret env)\"`).\n" +
			"Results are cited (url + retrieved-at) so a caller can verify they resolve.",
		Args:          cobra.MinimumNArgs(1),
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(c *cobra.Command, args []string) error {
			query := strings.Join(args, " ")
			ctx := c.Context()

			// --- local (P0b): content/files scan + kb facts ---
			if local || files || kb || content {
				domain := ""
				switch {
				case files:
					domain = "files"
				case kb:
					domain = "kb"
				case content:
					domain = "content"
				}
				lmax := max
				if lmax <= 8 {
					lmax = 40 // local wants more than the web default
				}
				res, err := Local(query, LocalOptions{Dir: dir, MaxResults: lmax, Domain: domain})
				if err != nil {
					return err
				}
				dom := domain
				if dom == "" {
					dom = "content+kb"
				}
				telemetry.Provenance(ctx, "search.local_results", int64(len(res)), dom)
				if asJSON {
					out := struct {
						SchemaVersion string        `json:"schema_version"`
						Query         string        `json:"query"`
						Domain        string        `json:"domain"`
						Count         int           `json:"count"`
						Results       []LocalResult `json:"results"`
					}{"bashy-search-v1", query, dom, len(res), res}
					b, _ := json.MarshalIndent(out, "", "  ")
					fmt.Println(string(b))
					return nil
				}
				for _, r := range res {
					switch r.Kind {
					case "content":
						fmt.Printf("%s:%d: %s\n", r.Path, r.Line, truncate(r.Text, 160))
					case "kb":
						fmt.Printf("kb: %s\n", r.Path)
					default:
						fmt.Println(r.Path)
					}
				}
				fmt.Fprintf(os.Stderr, "(%d local results · %s)\n", len(res), dom)
				return nil
			}

			// --- web (P0a): provider ladder ---
			results, used, err := Web(ctx, query, Options{MaxResults: max, Backend: backend})
			if err != nil {
				return err
			}
			telemetry.Provenance(ctx, "search.results", int64(len(results)), used)

			if asJSON {
				out := struct {
					SchemaVersion string   `json:"schema_version"`
					Query         string   `json:"query"`
					Backend       string   `json:"backend"`
					Count         int      `json:"count"`
					Results       []Result `json:"results"`
				}{"bashy-search-v1", query, used, len(results), results}
				b, _ := json.MarshalIndent(out, "", "  ")
				fmt.Println(string(b))
				return nil
			}
			for i, r := range results {
				fmt.Printf("%d. %s\n   %s\n", i+1, r.Title, r.URL)
				if s := strings.TrimSpace(r.Snippet); s != "" {
					fmt.Printf("   %s\n", truncate(s, 200))
				}
			}
			fmt.Fprintf(os.Stderr, "(%d results via %s)\n", len(results), used)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&local, "local", false, "local search: file content + kb facts (default domain content+kb)")
	f.BoolVar(&content, "content", false, "local: file-content scan only (implies --local)")
	f.BoolVar(&files, "files", false, "local: filename scan only (implies --local)")
	f.BoolVar(&kb, "kb", false, "local: kb facts only (implies --local)")
	f.StringVar(&dir, "dir", "", "local: root to scan (default: cwd)")
	f.BoolVar(&asJSON, "json", false, "print a bashy-search-v1 JSON envelope")
	f.IntVar(&max, "max", 8, "maximum results (local defaults to 40)")
	f.StringVar(&backend, "backend", "", "web: force a backend: tavily | brave | serper (default: auto)")
	return cmd
}

func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n] + "…"
	}
	return s
}
