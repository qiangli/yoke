package broker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/broker/door"
)

// UsageSchemaVersion identifies the JSON envelope emitted by llm usage.
const UsageSchemaVersion = "bashy-llm-usage-v1"

type usageReport struct {
	Schema     string           `json:"schema"`
	Since      time.Time        `json:"since"`
	Principals []PrincipalUsage `json:"principals"`
}

var usageNow = time.Now

var usageAuditPath = func() (string, error) {
	dir, err := door.StateDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "audit.jsonl"), nil
}

func newUsageCmd() *cobra.Command {
	var peer, sinceText string
	var asJSON bool
	cmd := &cobra.Command{
		Use:          "usage",
		Short:        "roll up requests, tokens and cost by door principal",
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			window, err := parseUsageWindow(sinceText)
			if err != nil {
				return err
			}
			principal := ""
			if peer != "" {
				if !ValidPeerName(peer) {
					return fmt.Errorf("llm usage: invalid peer name %q", peer)
				}
				principal = PeerPrincipalPrefix + peer
			}
			path, err := usageAuditPath()
			if err != nil {
				return err
			}
			f, err := os.Open(path)
			if errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("llm usage: no door audit log at %s", path)
			}
			if err != nil {
				return fmt.Errorf("llm usage: open audit log: %w", err)
			}
			defer f.Close()
			since := time.Time{}
			if window > 0 {
				since = usageNow().UTC().Add(-window)
			}
			rows, err := RollupUsage(f, UsageRollupOptions{Since: since, Principal: principal})
			if err != nil {
				return err
			}
			if asJSON {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(usageReport{Schema: UsageSchemaVersion, Since: since, Principals: rows})
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 0, 0, 2, ' ', 0)
			fmt.Fprintln(tw, "PRINCIPAL\tREQUESTS\tPROMPT_TOKENS\tCOMPLETION_TOKENS\tCACHED_TOKENS\tCOST_USD\tUNPRICED")
			for _, row := range rows {
				fmt.Fprintf(tw, "%s\t%d\t%d\t%d\t%d\t%.6f\t%t\n", row.Principal, row.Requests, row.PromptTokens, row.CompletionTokens, row.CachedTokens, row.CostUSD, row.Unpriced)
			}
			return tw.Flush()
		},
	}
	f := cmd.Flags()
	f.StringVar(&peer, "peer", "", "report only peer NAME (principal peer:NAME)")
	f.StringVar(&sinceText, "since", "7d", "lookback window (for example 7d or 24h; 0 = all history)")
	f.BoolVar(&asJSON, "json", false, "emit the stable usage report envelope")
	return cmd
}

func parseUsageWindow(s string) (time.Duration, error) {
	s = strings.TrimSpace(s)
	if s == "0" {
		return 0, nil
	}
	if strings.HasSuffix(s, "d") {
		days, err := strconv.ParseFloat(strings.TrimSuffix(s, "d"), 64)
		if err != nil || days <= 0 {
			return 0, fmt.Errorf("llm usage: invalid --since %q", s)
		}
		return time.Duration(days * float64(24*time.Hour)), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("llm usage: invalid --since %q", s)
	}
	return d, nil
}
