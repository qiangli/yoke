package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/spf13/cobra"
)

func (c PairedHostsClient) reachability(ctx context.Context, host string, patch map[string]any) ([]byte, error) {
	path := "/api/v1/hosts/" + url.PathEscape(host) + "/reachability"
	method := http.MethodGet
	var body io.Reader
	if patch != nil {
		method = http.MethodPatch
		raw, err := json.Marshal(patch)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.Token)
	req.Header.Set("Accept", "application/json")
	if patch != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, provisionAPIError(method+" "+path, resp.StatusCode, raw)
	}
	return raw, nil
}

func newReachabilityCmd(clientFor func(context.Context) (PairedHostsClient, error), asJSON *bool) *cobra.Command {
	root := &cobra.Command{Use: "reachability", Short: "Read host reachability and manage owner alerts"}
	add := func(use, short, field string, count int, value func(string) (any, error)) {
		root.AddCommand(&cobra.Command{
			Use: use, Short: short, Args: cobra.ExactArgs(count),
			RunE: func(cmd *cobra.Command, args []string) error {
				var patch map[string]any
				if field != "" {
					v, err := value(args[1])
					if err != nil {
						return err
					}
					patch = map[string]any{field: v}
				}
				client, err := clientFor(cmd.Context())
				if err != nil {
					return err
				}
				host := args[0]
				if host == "@all" {
					host = "~all"
				}
				raw, err := client.reachability(cmd.Context(), host, patch)
				if err != nil {
					return err
				}
				return printReachability(cmd.OutOrStdout(), raw, *asJSON)
			},
		})
	}
	add("status HOST|@all", "Show the current state and alert", "", 1, nil)
	add("ack HOST ALERT_ID", "Acknowledge the specified active alert", "ack_alert_id", 2, func(s string) (any, error) { return s, nil })
	add("mute HOST DURATION", "Mute notices for a duration (0 clears mute)", "mute_seconds", 2, reachabilitySeconds)
	add("threshold HOST|@all DURATION|default|off|inherit", "Enable or disable alerts and set their threshold", "threshold_seconds", 2, func(s string) (any, error) {
		switch s {
		case "default":
			return int64(900), nil
		case "off":
			return int64(0), nil
		case "inherit":
			return int64(-1), nil
		default:
			return reachabilitySeconds(s)
		}
	})
	return root
}

func reachabilitySeconds(s string) (any, error) {
	if s == "0" {
		return int64(0), nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < time.Second || d > 30*24*time.Hour || d%time.Second != 0 {
		return nil, fmt.Errorf("use a whole-second duration between 1s and 720h")
	}
	return int64(d / time.Second), nil
}

func printReachability(out io.Writer, raw []byte, asJSON bool) error {
	var status struct {
		Host         string `json:"host"`
		State        string `json:"state"`
		Reason       string `json:"reason"`
		LastSeen     string `json:"last_seen_at"`
		MuteUntil    string `json:"mute_until"`
		Threshold    int64  `json:"threshold_seconds"`
		Monitoring   bool   `json:"monitoring"`
		AlertID      string `json:"alert_id"`
		Acknowledged bool   `json:"acknowledged"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return err
	}
	if status.Host == "" || status.State == "" {
		return fmt.Errorf("invalid reachability response")
	}
	if asJSON {
		_, err := fmt.Fprintln(out, string(bytes.TrimSpace(raw)))
		return err
	}
	if status.LastSeen == "" {
		status.LastSeen = "unknown"
	}
	_, err := fmt.Fprintf(out, "%s\t%s\t%s\nlast seen: %s\nmonitoring: %t (threshold %s)\n", status.Host,
		status.State, status.Reason, status.LastSeen, status.Monitoring, time.Duration(status.Threshold)*time.Second)
	if err != nil {
		return err
	}
	if status.AlertID != "" {
		if _, err := fmt.Fprintf(out, "alert: %s (acknowledged: %t)\n", status.AlertID, status.Acknowledged); err != nil {
			return err
		}
	}
	if status.MuteUntil != "" {
		_, err = fmt.Fprintf(out, "muted until: %s\n", status.MuteUntil)
	}
	return err
}
