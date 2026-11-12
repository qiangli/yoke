package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/spf13/cobra"
)

func hostSharePath(arg string) (string, error) {
	host, app, ok := strings.Cut(arg, "/")
	if !ok || host == "" || app == "" || strings.Contains(app, "/") {
		return "", fmt.Errorf("share: expected HOST/APP: %q", arg)
	}
	return "/api/v1/hosts/" + url.PathEscape(host) + "/shares", nil
}

// newHostGrantCmds add and inspect owner grants on a host app.
func newHostGrantCmds() []*cobra.Command {
	var master, token string
	var acct accountFlag
	var asJSON, dryRun bool
	bind := func(c *cobra.Command) {
		c.Flags().StringVar(&master, "master", "", "Cloudbox master URL")
		c.Flags().StringVar(&token, "token", "", "owner Bearer token")
		c.Flags().BoolVar(&asJSON, "json", false, "machine-readable output")
		c.Flags().BoolVar(&dryRun, "dry-run", false, "print write request without sending it")
		acct.bind(c)
	}
	call := func(cmd *cobra.Command, method, path string, body any) error {
		if dryRun && method != http.MethodGet {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"dry_run": true, "method": method, "path": path, "body": body})
		}
		u, t, e := cloudOverrides(cmd.Context(), &acct, master, token)
		if e != nil {
			return e
		}
		c, e := NewMemberClient(u, t)
		if e != nil {
			return e
		}
		raw, e := c.Do(cmd.Context(), method, path, body)
		if e != nil {
			return e
		}
		if asJSON {
			if len(raw) == 0 {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"removed": true})
			}
			_, e = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return e
		}
		if method == http.MethodDelete {
			_, e = fmt.Fprintln(cmd.OutOrStdout(), "removed")
			return e
		}
		var env struct {
			Shares []struct {
				ID  string `json:"id"`
				App string `json:"app"`
				grantRule
			} `json:"shares"`
			Share struct {
				ID  string `json:"id"`
				App string `json:"app"`
				grantRule
			} `json:"share"`
		}
		if e = json.Unmarshal(raw, &env); e != nil {
			return e
		}
		rows := env.Shares
		if method == http.MethodPost {
			rows = append(rows, env.Share)
		}
		for _, s := range rows {
			s.grantRule.normalize()
			b, _ := json.Marshal(s.grantRule)
			fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", s.ID, s.App, b)
		}
		return nil
	}
	var circles, groups, emails []string
	add := &cobra.Command{Use: "add HOST/APP", Short: "add one host app audience rule", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		p, e := hostSharePath(a[0])
		if e != nil {
			return e
		}
		_, app, _ := strings.Cut(a[0], "/")
		rule := newGrantRule(circles, groups, emails)
		return call(cmd, http.MethodPost, p, map[string]any{"app": app, "circles": rule.Circles, "groups": rule.Groups, "emails": rule.Emails})
	}}
	bind(add)
	add.Flags().StringSliceVar(&circles, "circle", nil, "member circle (repeatable)")
	add.Flags().StringSliceVar(&groups, "group", nil, "owner group (repeatable)")
	add.Flags().StringSliceVar(&emails, "email", nil, "member email (repeatable)")
	ls := &cobra.Command{Use: "ls HOST", Short: "list audience rules on a host", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		if a[0] == "" || strings.Contains(a[0], "/") {
			return fmt.Errorf("share ls: expected HOST: %q", a[0])
		}
		return call(cmd, http.MethodGet, "/api/v1/hosts/"+url.PathEscape(a[0])+"/shares", nil)
	}}
	bind(ls)
	rm := &cobra.Command{Use: "rm HOST/APP SHARE_ID", Short: "remove a host app audience rule", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		p, e := hostSharePath(a[0])
		if e != nil {
			return e
		}
		return call(cmd, http.MethodDelete, p+"/"+url.PathEscape(a[1]), nil)
	}}
	bind(rm)
	return []*cobra.Command{add, ls, rm}
}
