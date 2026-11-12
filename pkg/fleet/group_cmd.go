package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/spf13/cobra"
)

// NewGroupClient resolves the account-level master and owner credential used
// for group operations.
func NewGroupClient(master, token string) (*boundary.Client, error) {
	selected := token != "" || os.Getenv("BASHY_FLEET_TOKEN") != "" || os.Getenv("BASHY_API_KEY") != ""
	master, token = ResolveMaster(master, token)
	if !selected {
		if ownerToken := masterToken(master); ownerToken != "" {
			token = ownerToken
		}
	}
	if token == "" {
		return nil, fmt.Errorf("group: no Cloudbox owner token")
	}
	return &boundary.Client{BaseURL: master, Token: token, HTTP: httpClient()}, nil
}

// NewGroupCmd constructs the account group verbs for a host front door.
func NewGroupCmd() *cobra.Command {
	var master, token string
	var asJSON, dryRun, noInput bool
	var acct accountFlag
	root := &cobra.Command{Use: "group", Short: "Cloudbox groups", SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&master, "master", "", "Cloudbox master URL")
	root.PersistentFlags().StringVar(&token, "token", "", "owner Bearer token")
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "machine-readable output")
	root.PersistentFlags().BoolVar(&noInput, "no-input", false, "never prompt")
	_ = noInput
	acct.bindPersistent(root)
	client := func(ctx context.Context) (*boundary.Client, error) {
		u, t, err := cloudOverrides(ctx, &acct, master, token)
		if err != nil {
			return nil, err
		}
		return NewGroupClient(u, t)
	}
	output := func(cmd *cobra.Command, value any, plain string) error {
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(value)
		}
		_, err := fmt.Fprintln(cmd.OutOrStdout(), plain)
		return err
	}
	write := func(cmd *cobra.Command, c *boundary.Client, method, path string, body any) error {
		if dryRun {
			return output(cmd, map[string]any{"dry_run": true, "method": method, "path": path, "body": body}, method+" "+path)
		}
		raw, err := c.Do(cmd.Context(), method, path, body)
		if err != nil {
			return err
		}
		if asJSON {
			if len(raw) == 0 {
				return output(cmd, map[string]any{"removed": true}, "")
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return err
		}
		if method == http.MethodDelete {
			return output(cmd, nil, "removed")
		}
		return output(cmd, nil, string(raw))
	}

	ls := &cobra.Command{Use: "ls [NAME]", Short: "list groups or group members", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		path := "/api/v1/groups"
		if len(args) == 1 {
			path += "?name=" + url.QueryEscape(args[0])
		}
		raw, err := c.Do(cmd.Context(), http.MethodGet, path, nil)
		if err != nil {
			return err
		}
		if asJSON {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return err
		}
		var env struct {
			Groups []struct {
				Name    string   `json:"name"`
				Members []string `json:"members"`
			} `json:"groups"`
		}
		if err = json.Unmarshal(raw, &env); err != nil {
			return err
		}
		for _, g := range env.Groups {
			if len(g.Members) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), g.Name)
			} else {
				for _, m := range g.Members {
					fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\n", g.Name, m)
				}
			}
		}
		return nil
	}}
	root.AddCommand(ls)

	add := &cobra.Command{Use: "add NAME EMAIL...", Short: "add members to a group", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		name, err := groupName(args[0])
		if err != nil {
			return err
		}
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		path := "/api/v1/groups/" + url.PathEscape(name) + "/members"
		body := map[string]any{"emails": args[1:]}
		return write(cmd, c, http.MethodPost, path, body)
	}}
	root.AddCommand(add)

	rm := &cobra.Command{Use: "rm NAME EMAIL...", Short: "remove members from a group", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		name := args[0]
		emails := args[1:]
		if dryRun {
			for _, email := range emails {
				p := "/api/v1/groups/" + url.PathEscape(name) + "/members/" + url.PathEscape(email)
				if err := output(cmd, map[string]any{"dry_run": true, "method": http.MethodDelete, "path": p, "body": nil}, http.MethodDelete+" "+p); err != nil {
					return err
				}
			}
			return nil
		}
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		for _, email := range emails {
			p := "/api/v1/groups/" + url.PathEscape(name) + "/members/" + url.PathEscape(email)
			if _, err := c.Do(cmd.Context(), http.MethodDelete, p, nil); err != nil {
				return err
			}
		}
		if asJSON {
			return output(cmd, map[string]any{"removed": true}, "")
		}
		return output(cmd, nil, "removed")
	}}
	root.AddCommand(rm)

	del := &cobra.Command{Use: "delete NAME", Short: "delete a group", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		return write(cmd, c, http.MethodDelete, "/api/v1/groups/"+url.PathEscape(args[0]), nil)
	}}
	root.AddCommand(del)

	for _, c := range root.Commands() {
		if c.Name() == "add" || c.Name() == "rm" || c.Name() == "delete" {
			c.Flags().BoolVar(&dryRun, "dry-run", false, "print request without sending")
		}
	}
	return root
}

func groupName(s string) (string, error) {
	const rule = "a lowercase slug [a-z0-9-] of 1-32 chars"
	if s == "public" || s == "owner" {
		return "", fmt.Errorf("group: name %q is reserved; pick %s", s, rule)
	}
	if len(s) < 1 || len(s) > 32 {
		return "", fmt.Errorf("group: invalid name %q (%d chars; want %s)", s, len(s), rule)
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return "", fmt.Errorf("group: invalid name %q (use %s)", s, rule)
		}
	}
	return s, nil
}
