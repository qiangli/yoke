package fleet

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/spf13/cobra"
)

// NewMemberClient resolves the account-level master and owner credential used
// for membership checks. A paired host token is identity-only, so the saved
// owner credential wins when no token was selected explicitly.
func NewMemberClient(master, token string) (*boundary.Client, error) {
	selected := token != "" || os.Getenv("BASHY_FLEET_TOKEN") != "" || os.Getenv("BASHY_API_KEY") != ""
	master, token = ResolveMaster(master, token)
	if !selected {
		if ownerToken := masterToken(master); ownerToken != "" {
			token = ownerToken
		}
	}
	if token == "" {
		return nil, fmt.Errorf("member: no Cloudbox owner token")
	}
	return &boundary.Client{BaseURL: master, Token: token, HTTP: httpClient()}, nil
}

// NewMemberCmd constructs the account member verb for a host front door.
func NewMemberCmd() *cobra.Command {
	var master, token string
	var asJSON, dryRun, noInput bool
	var acct accountFlag
	root := &cobra.Command{Use: "member", Short: "Cloudbox members", SilenceUsage: true, SilenceErrors: true}
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
		return NewMemberClient(u, t)
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
		var env map[string]json.RawMessage
		if err = json.Unmarshal(raw, &env); err != nil {
			return err
		}
		if m := env["member"]; len(m) > 0 {
			var row boundary.Member
			if json.Unmarshal(m, &row) == nil {
				return output(cmd, nil, fmt.Sprintf("%s\t%s\t%s\t%s", row.ID, row.Email, row.Circle, row.Status))
			}
		}
		return output(cmd, nil, string(raw))
	}
	resolve := func(ctx context.Context, c *boundary.Client, key string) (string, error) {
		if strings.HasPrefix(key, "mem_") {
			return key, nil
		}
		rows, stale, err := c.List(ctx)
		if err != nil {
			return "", err
		}
		if stale {
			return "", fmt.Errorf("member: cannot resolve email from offline list for a write")
		}
		for _, r := range rows {
			if strings.EqualFold(r.Email, key) {
				return r.ID, nil
			}
		}
		return "", fmt.Errorf("member: %q not found", key)
	}
	var lsCircle string
	ls := &cobra.Command{Use: "ls", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		rows, stale, err := c.List(cmd.Context())
		if err != nil {
			return err
		}
		filtered := []boundary.Member{}
		for _, r := range rows {
			if lsCircle == "" || r.Circle == lsCircle {
				filtered = append(filtered, r)
			}
		}
		if asJSON {
			return output(cmd, map[string]any{"members": filtered, "stale": stale}, "")
		}
		for _, r := range filtered {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%s\t%t\t%s\n", r.ID, r.Email, r.Circle, r.Status, r.Mutual, r.Name)
		}
		if stale {
			fmt.Fprintln(cmd.ErrOrStderr(), "member: showing last known list (offline)")
		}
		return nil
	}}
	ls.Flags().StringVar(&lsCircle, "circle", "", "filter by circle")
	root.AddCommand(ls)
	var inviteCircle string
	invite := &cobra.Command{Use: "invite EMAIL", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		circle, err := memberCircle(inviteCircle, false)
		if err != nil {
			return err
		}
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		return write(cmd, c, http.MethodPost, "/api/v1/members", map[string]string{"email": args[0], "circle": circle})
	}}
	invite.Flags().StringVar(&inviteCircle, "circle", "guest", "member circle")
	root.AddCommand(invite)
	var acceptCircle string
	accept := &cobra.Command{Use: "accept ID", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		circle, err := memberCircle(acceptCircle, false)
		if err != nil {
			return err
		}
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		return write(cmd, c, http.MethodPost, boundary.InvitationPath(args[0]), map[string]string{"circle": circle})
	}}
	accept.Flags().StringVar(&acceptCircle, "circle", "guest", "your circle for the inviter")
	root.AddCommand(accept)
	var setCircle, setName string
	set := &cobra.Command{Use: "set EMAIL", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		body := map[string]string{}
		if setCircle != "" || !cmd.Flags().Changed("name") {
			circle, err := memberCircle(setCircle, true)
			if err != nil {
				return err
			}
			body["circle"] = circle
		}
		if cmd.Flags().Changed("name") {
			if setName = strings.TrimSpace(setName); setName == "" {
				return fmt.Errorf("member: --name must not be blank")
			}
			body["name"] = setName
		}
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		id, err := resolve(cmd.Context(), c, args[0])
		if err != nil {
			return err
		}
		return write(cmd, c, http.MethodPatch, boundary.MemberPath(id), body)
	}}
	set.Flags().StringVar(&setCircle, "circle", "", "new member circle")
	set.Flags().StringVar(&setName, "name", "", "display name for the member")
	root.AddCommand(set)
	root.AddCommand(&cobra.Command{Use: "rm EMAIL", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		id, err := resolve(cmd.Context(), c, args[0])
		if err != nil {
			return err
		}
		return write(cmd, c, http.MethodDelete, boundary.MemberPath(id), nil)
	}})
	root.AddCommand(&cobra.Command{Use: "of", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		raw, err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/members/of", nil)
		if err != nil {
			return err
		}
		if asJSON {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return err
		}
		var env struct {
			Of []boundary.MemberOf `json:"of"`
		}
		if err = json.Unmarshal(raw, &env); err != nil {
			return err
		}
		for _, r := range env.Of {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\t%t\n", r.ID, r.OwnerEmail, r.Status, r.Mutual)
		}
		return nil
	}})
	root.AddCommand(&cobra.Command{Use: "invitations", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		raw, err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/member/invitations", nil)
		if err != nil {
			return err
		}
		if asJSON {
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return err
		}
		var env struct {
			Invitations []boundary.Invitation `json:"invitations"`
		}
		if err = json.Unmarshal(raw, &env); err != nil {
			return err
		}
		for _, r := range env.Invitations {
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", r.ID, r.FromEmail, r.Status)
		}
		return nil
	}})
	root.AddCommand(&cobra.Command{Use: "check EMAIL", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client(cmd.Context())
		if err != nil {
			return err
		}
		rows, stale, err := c.List(cmd.Context())
		if err != nil {
			return err
		}
		result := map[string]any{"email": args[0], "allowed": false, "reason": "unknown", "stale": stale}
		for _, r := range rows {
			if strings.EqualFold(r.Email, args[0]) {
				result["circle"] = r.Circle
				result["status"] = r.Status
				result["mutual"] = r.Mutual
				switch {
				case stale:
					result["reason"] = "offline"
				case r.Status != "active":
					result["reason"] = "inactive"
				case !r.Mutual:
					result["reason"] = "not_mutual"
				default:
					result["allowed"] = true
					result["reason"] = "active_mutual"
				}
				break
			}
		}
		if asJSON {
			return output(cmd, result, "")
		}
		return output(cmd, nil, fmt.Sprintf("%s: %s (circle %v)", args[0], result["reason"], result["circle"]))
	}})
	for _, c := range root.Commands() {
		if c.Name() == "invite" || c.Name() == "accept" || c.Name() == "set" || c.Name() == "rm" {
			c.Flags().BoolVar(&dryRun, "dry-run", false, "print request without sending")
		}
	}
	return root
}

// memberCircle checks the client side of the circle rule (the master
// re-checks): a lowercase slug [a-z0-9-] of 1-32 chars; public and owner are
// reserved. family, friend, team and guest are only the defaults.
func memberCircle(s string, required bool) (string, error) {
	if s == "" && !required {
		return "guest", nil
	}
	const rule = "a lowercase slug [a-z0-9-] of 1-32 chars, e.g. family, friend, team, guest or your own name"
	if s == "public" || s == "owner" {
		return "", fmt.Errorf("member: circle %q is reserved; pick %s", s, rule)
	}
	if len(s) < 1 || len(s) > 32 {
		return "", fmt.Errorf("member: invalid circle %q (%d chars; want %s)", s, len(s), rule)
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return "", fmt.Errorf("member: invalid circle %q (use %s)", s, rule)
		}
	}
	return s, nil
}
