package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/qiangli/yoke/pkg/boundary"
)

// SuperadminSessionEnv names the environment variable holding the cloudbox
// step-up superadmin session (from <master>/superadmin/login). The master's
// /api/superadmin routes refuse API tokens, so this verb never falls back to
// an owner or paired token; there is no flag either, so the session never
// shows in a process listing. Keep it in the vault:
//
//	bashy secret exec CLOUDBOX_SUPERADMIN_SESSION=CLOUDBOX_SUPERADMIN_SESSION -- bashy superadmin hosts ls
const SuperadminSessionEnv = "CLOUDBOX_SUPERADMIN_SESSION"

// NewSuperadminCmd is the operator repair surface of the cloudbox master:
// cross-owner host listing and removal.
func NewSuperadminCmd() *cobra.Command {
	var master string
	var asJSON bool
	root := &cobra.Command{Use: "superadmin", Short: "Cloudbox operator repair: list and remove any owner's hosts (step-up session only)", SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&master, "master", "", "Cloudbox master URL")
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "machine-readable output")

	client := func() (*boundary.Client, error) {
		session := strings.TrimSpace(os.Getenv(SuperadminSessionEnv))
		base, _ := resolveCloud(master, "-", PortalBase())
		if session == "" {
			return nil, fmt.Errorf("superadmin: no step-up session: sign in at %s/superadmin/login and pass the session as %s (API tokens are refused)", base, SuperadminSessionEnv)
		}
		return &boundary.Client{BaseURL: base, Token: session, HTTP: httpClient()}, nil
	}

	hosts := &cobra.Command{Use: "hosts", Short: "every owner's hosts"}
	var owner string
	ls := &cobra.Command{Use: "ls", Short: "list hosts across owners", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client()
		if err != nil {
			return err
		}
		raw, err := c.Do(cmd.Context(), http.MethodGet, "/api/superadmin/hosts", nil)
		if err != nil {
			return err
		}
		var rows []struct {
			Name       string `json:"name"`
			Owner      string `json:"owner"`
			PairMode   string `json:"pair_mode"`
			LastSeenAt string `json:"last_seen_at"`
		}
		if err := json.Unmarshal(raw, &rows); err != nil {
			return fmt.Errorf("superadmin: hosts: %w", err)
		}
		out := rows[:0]
		for _, r := range rows {
			if owner == "" || strings.EqualFold(r.Owner, owner) {
				out = append(out, r)
			}
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"hosts": out})
		}
		for _, r := range out {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\n", r.Owner, r.Name, r.PairMode, r.LastSeenAt)
		}
		return nil
	}}
	ls.Flags().StringVar(&owner, "owner", "", "only this owner's hosts")

	rm := &cobra.Command{Use: "rm OWNER/HOST", Short: "remove another owner's host with its shares (audited)", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		o, h, ok := strings.Cut(a[0], "/")
		if !ok || strings.TrimSpace(o) == "" || strings.TrimSpace(h) == "" || strings.Contains(h, "/") {
			return fmt.Errorf("superadmin: want OWNER/HOST, got %q", a[0])
		}
		c, err := client()
		if err != nil {
			return err
		}
		path := "/api/superadmin/hosts/" + url.PathEscape(o) + "/" + url.PathEscape(h) + "?confirm=" + url.QueryEscape(h)
		if _, err := c.Do(cmd.Context(), http.MethodDelete, path, nil); err != nil {
			return err
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"removed": true, "owner": o, "host": h})
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "removed %s/%s\n", o, h)
		return err
	}}
	hosts.AddCommand(ls, rm)
	root.AddCommand(hosts)
	return root
}
