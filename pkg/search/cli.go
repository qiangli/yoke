package search

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/telemetry"
)

// NewSearchCmd is `bashy search` — web search and local search in one command.
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
		index   bool
		status  bool
	)
	cmd := &cobra.Command{
		Use:   "search QUERY...",
		Short: "Search the web, local files, file contents, or knowledge",
		Long: "Search the web through a provider ladder (auto by available key, or --backend):\n" +
			"  1. tavily  (TAVILY_API_KEY)\n" +
			"  2. brave   (BRAVE_API_KEY)\n" +
			"  3. serper  (SERPER_API_KEY)\n" +
			"Keys come from the environment (project them with `eval \"$(bashy secret env)\"`).\n" +
			"Results are cited (url + retrieved-at) so a caller can verify they resolve.\n\n" +
			"Use --files for filename search. Without an index, it scans --dir (default: cwd).\n" +
			"Build or refresh a persistent filename index with --files --index; then queries use it without a crawl.\n" +
			"Inspect count, disk size, and age with --files --index-status. File contents are never indexed.",
		Args:          cobra.ArbitraryArgs,
		SilenceUsage:  true,
		SilenceErrors: false,
		RunE: func(c *cobra.Command, args []string) error {
			if index || status {
				if !files || len(args) != 0 || index && status {
					return fmt.Errorf("search: --index and --index-status require --files and no query")
				}
				var info FileIndexStatus
				var err error
				if index {
					info, err = BuildFileIndex(dir)
				} else {
					info, err = FileIndexInfo(dir)
				}
				if err != nil {
					return err
				}
				if asJSON {
					return json.NewEncoder(c.OutOrStdout()).Encode(info)
				}
				fmt.Fprintf(c.OutOrStdout(), "%d files, %d skipped, %d bytes, indexed %s\n", info.Files, info.Skipped, info.Bytes, info.UpdatedAt.Format(time.RFC3339))
				return nil
			}
			if len(args) == 0 {
				return fmt.Errorf("search: a query is required")
			}
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
				if !c.Flags().Changed("max") {
					lmax = 40 // local wants more than the web default
				}
				if lmax <= 0 {
					return fmt.Errorf("search: --max must be positive")
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
					fmt.Fprintln(c.OutOrStdout(), string(b))
					return nil
				}
				for _, r := range res {
					switch r.Kind {
					case "content":
						fmt.Fprintf(c.OutOrStdout(), "%s:%d: %s\n", r.Path, r.Line, truncate(r.Text, 160))
					case "kb":
						fmt.Fprintf(c.OutOrStdout(), "kb: %s\n", r.Path)
					default:
						fmt.Fprintln(c.OutOrStdout(), r.Path)
					}
				}
				fmt.Fprintf(c.ErrOrStderr(), "(%d local results · %s)\n", len(res), dom)
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
				fmt.Fprintln(c.OutOrStdout(), string(b))
				return nil
			}
			for i, r := range results {
				fmt.Fprintf(c.OutOrStdout(), "%d. %s\n   %s\n", i+1, r.Title, r.URL)
				if s := strings.TrimSpace(r.Snippet); s != "" {
					fmt.Fprintf(c.OutOrStdout(), "   %s\n", truncate(s, 200))
				}
			}
			fmt.Fprintf(c.ErrOrStderr(), "(%d results via %s)\n", len(results), used)
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&local, "local", false, "local search: file content + kb facts (default domain content+kb)")
	f.BoolVar(&content, "content", false, "local: file-content scan only (implies --local)")
	f.BoolVar(&files, "files", false, "local: filename search (uses an index when available)")
	f.BoolVar(&index, "index", false, "with --files: build or refresh the filename index for --dir")
	f.BoolVar(&status, "index-status", false, "with --files: show index count, size, and last refresh")
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
