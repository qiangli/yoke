package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

// NewShareCmd constructs the `share` verb tree for a host front door.
func NewShareCmd() *cobra.Command {
	root := &cobra.Command{Use: "share", Short: "Cloudbox shares", SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.AddCommand(NewShareWhoCmd())
	return root
}

// NewShareWhoCmd constructs the `share who HOST/APP` command.
func NewShareWhoCmd() *cobra.Command {
	var master, token string
	var asJSON, noInput bool
	var acct accountFlag
	cmd := &cobra.Command{
		Use:           "who HOST/APP",
		Short:         "list members admitted to a host app",
		Args:          cobra.ExactArgs(1),
		SilenceUsage:  true,
		SilenceErrors: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			host, app, ok := strings.Cut(args[0], "/")
			if !ok || host == "" || app == "" || strings.Contains(app, "/") {
				return fmt.Errorf("share who: must be HOST/APP: %q", args[0])
			}
			u, t, err := cloudOverrides(cmd.Context(), &acct, master, token)
			if err != nil {
				return err
			}
			c, err := NewMemberClient(u, t)
			if err != nil {
				return err
			}
			path := "/api/v1/hosts/" + url.PathEscape(host) + "/apps/" + url.PathEscape(app) + "/who"
			raw, err := c.Do(cmd.Context(), http.MethodGet, path, nil)
			if err != nil {
				return err
			}
			if asJSON {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
				return err
			}
			var env struct {
				Host   string   `json:"host"`
				App    string   `json:"app"`
				Owner  string   `json:"owner"`
				Emails []string `json:"emails"`
			}
			if err := json.Unmarshal(raw, &env); err != nil {
				return err
			}
			for _, email := range env.Emails {
				fmt.Fprintln(cmd.OutOrStdout(), email)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&master, "master", "", "Cloudbox master URL")
	cmd.Flags().StringVar(&token, "token", "", "owner Bearer token")
	cmd.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
	cmd.Flags().BoolVar(&noInput, "no-input", false, "never prompt")
	_ = noInput
	acct.bindPersistent(cmd)
	return cmd
}
