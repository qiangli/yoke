package fleet

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strings"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/spf13/cobra"
)

// NewSuiteClient uses the same account owner credential resolution as member.
func NewSuiteClient(master, token string) (*boundary.Client, error) {
	selected := token != "" || os.Getenv("BASHY_FLEET_TOKEN") != "" || os.Getenv("BASHY_API_KEY") != ""
	master, token = ResolveMaster(master, token)
	if !selected {
		if ownerToken := masterToken(master); ownerToken != "" {
			token = ownerToken
		}
	}
	if token == "" {
		return nil, fmt.Errorf("suite: no Cloudbox owner token")
	}
	return &boundary.Client{BaseURL: master, Token: token, HTTP: httpClient()}, nil
}

// NewSuiteCmd provides the account-level suite and grant verbs.
func NewSuiteCmd() *cobra.Command {
	var master, token string
	var dryRun, noInput bool
	var acct accountFlag
	root := &cobra.Command{Use: "suite", Short: "Cloudbox app suites", SilenceUsage: true, SilenceErrors: true}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&master, "master", "", "Cloudbox master URL")
	root.PersistentFlags().StringVar(&token, "token", "", "owner Bearer token")
	root.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "print write request without sending it")
	root.PersistentFlags().BoolVar(&noInput, "no-input", false, "never prompt")
	_ = noInput
	root.PersistentFlags().Bool("json", true, "machine-readable output")
	acct.bindPersistent(root)
	client := func(cmd *cobra.Command) (*boundary.Client, error) {
		u, t, e := cloudOverrides(cmd.Context(), &acct, master, token)
		if e != nil {
			return nil, e
		}
		return NewSuiteClient(u, t)
	}
	call := func(cmd *cobra.Command, method, path string, body any) error {
		if dryRun && method != http.MethodGet {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"dry_run": true, "method": method, "path": path, "body": body})
		}
		c, e := client(cmd)
		if e != nil {
			return e
		}
		raw, e := c.Do(cmd.Context(), method, path, body)
		if e != nil {
			return e
		}
		if len(raw) == 0 {
			// 204 is the server's answer to every successful write without
			// a body (add, remove, rm): report what was done, not "removed".
			done := "ok"
			if method == http.MethodDelete {
				done = "removed"
			}
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{done: true})
		}
		_, e = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return e
	}
	root.AddCommand(&cobra.Command{Use: "ls", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return call(cmd, http.MethodGet, "/api/v1/suites", nil) }})
	root.AddCommand(&cobra.Command{Use: "show NAME", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		return call(cmd, http.MethodGet, "/api/v1/suites/"+url.PathEscape(a[0]), nil)
	}})
	root.AddCommand(&cobra.Command{Use: "set NAME APP|HOST/APP...", Short: "replace a suite's apps (APP = wherever you serve it; HOST/APP = one host)", Args: cobra.MinimumNArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		apps, e := suiteAppArgs(a[1:])
		if e != nil {
			return e
		}
		return call(cmd, http.MethodPut, "/api/v1/suites/"+url.PathEscape(a[0]), map[string]any{"apps": apps})
	}})
	for _, spec := range []struct{ name, method string }{{"add", http.MethodPost}, {"remove", http.MethodDelete}} {
		sp := spec
		root.AddCommand(&cobra.Command{Use: sp.name + " NAME APP|HOST/APP", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
			app, e := suiteAppArgs(a[1:])
			if e != nil {
				return e
			}
			base := "/api/v1/suites/" + url.PathEscape(a[0]) + "/apps"
			if sp.method == http.MethodDelete {
				return call(cmd, sp.method, base+"/"+url.PathEscape(app[0].Host)+"/"+url.PathEscape(app[0].App), nil)
			}
			return call(cmd, sp.method, base, app[0])
		}})
	}
	root.AddCommand(&cobra.Command{Use: "rm NAME", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		return call(cmd, http.MethodDelete, "/api/v1/suites/"+url.PathEscape(a[0]), nil)
	}})
	root.AddCommand(&cobra.Command{Use: "shares NAME", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		return call(cmd, http.MethodGet, "/api/v1/suites/"+url.PathEscape(a[0])+"/shares", nil)
	}})
	var circles, groups, emails []string
	share := &cobra.Command{Use: "share NAME", Short: "add one audience rule to a suite", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		return call(cmd, http.MethodPost, "/api/v1/suites/"+url.PathEscape(a[0])+"/shares", newGrantRule(circles, groups, emails))
	}}
	share.Flags().StringSliceVar(&circles, "circle", nil, "member circle (repeatable)")
	share.Flags().StringSliceVar(&groups, "group", nil, "owner group (repeatable)")
	share.Flags().StringSliceVar(&emails, "email", nil, "member email (repeatable)")
	root.AddCommand(share)
	root.AddCommand(&cobra.Command{Use: "unshare NAME SHARE_ID", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		return call(cmd, http.MethodDelete, "/api/v1/suites/"+url.PathEscape(a[0])+"/shares/"+url.PathEscape(a[1]), nil)
	}})
	return root
}

type suiteAppArg struct {
	Host string `json:"host"`
	App  string `json:"app"`
}

// suiteAnyHost marks a logical suite entry: the app is granted wherever its
// owner serves it; the serving node binds it to its hosts.
const suiteAnyHost = "*"

// suiteAppArgs parses APP (logical: any host the owner serves it from) or
// HOST/APP (one host).
func suiteAppArgs(args []string) ([]suiteAppArg, error) {
	out := make([]suiteAppArg, 0, len(args))
	for _, arg := range args {
		host, name, ok := strings.Cut(arg, "/")
		if !ok {
			host, name = suiteAnyHost, arg
		}
		if host == "" || name == "" || strings.Contains(name, "/") {
			return nil, fmt.Errorf("suite: app must be APP or HOST/APP: %q", arg)
		}
		out = append(out, suiteAppArg{host, name})
	}
	return out, nil
}
