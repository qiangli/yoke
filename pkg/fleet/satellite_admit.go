package fleet

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/spf13/cobra"
)

type admission struct {
	Satellite string      `json:"satellite"`
	Rules     []grantRule `json:"rules"`
}

// NewSatelliteAdmitCmd manages the satellite owner's independent admission rules.
func NewSatelliteAdmitCmd() *cobra.Command {
	var master, token string
	var asJSON, dryRun bool
	var acct accountFlag
	root := &cobra.Command{Use: "admit", Short: "manage satellite admission rules", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&master, "master", "", "Cloudbox master URL")
	root.PersistentFlags().StringVar(&token, "token", "", "owner Bearer token")
	root.PersistentFlags().BoolVar(&asJSON, "json", false, "machine-readable output")
	root.PersistentFlags().BoolVar(&dryRun, "dry-run", false, "print write request without sending it")
	acct.bindPersistent(root)
	client := func(cmd *cobra.Command) (*boundary.Client, error) {
		u, t, e := cloudOverrides(cmd.Context(), &acct, master, token)
		if e != nil {
			return nil, e
		}
		return NewMemberClient(u, t)
	}
	path := func(s string) string { return "/api/v1/satellites/" + url.PathEscape(s) + "/admit" }
	get := func(cmd *cobra.Command, s string) (admission, error) {
		var v admission
		c, e := client(cmd)
		if e != nil {
			return v, e
		}
		raw, e := c.Do(cmd.Context(), http.MethodGet, path(s), nil)
		if e != nil {
			return v, e
		}
		e = json.Unmarshal(raw, &v)
		if v.Rules == nil {
			v.Rules = []grantRule{}
		}
		for i := range v.Rules {
			v.Rules[i].normalize()
		}
		return v, e
	}
	put := func(cmd *cobra.Command, s string, rules []grantRule) error {
		if rules == nil {
			rules = []grantRule{}
		}
		body := map[string]any{"rules": rules}
		if dryRun {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"dry_run": true, "method": "PUT", "path": path(s), "body": body})
		}
		c, e := client(cmd)
		if e != nil {
			return e
		}
		raw, e := c.Do(cmd.Context(), http.MethodPut, path(s), body)
		if e != nil {
			return e
		}
		if asJSON {
			_, e = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return e
		}
		return printAdmission(cmd, raw)
	}
	ls := &cobra.Command{Use: "ls SAT", Short: "list admission rules", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		v, e := get(cmd, a[0])
		if e != nil {
			return e
		}
		if asJSON {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(v)
		}
		return printAdmissionValue(cmd, v)
	}}
	var circles, groups, emails []string
	add := &cobra.Command{Use: "add SAT", Short: "append one admission rule", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		v, e := get(cmd, a[0])
		if e != nil {
			return e
		}
		v.Rules = append(v.Rules, newGrantRule(circles, groups, emails))
		return put(cmd, a[0], v.Rules)
	}}
	add.Flags().StringSliceVar(&circles, "circle", nil, "member circle (repeatable)")
	add.Flags().StringSliceVar(&groups, "group", nil, "owner group (repeatable)")
	add.Flags().StringSliceVar(&emails, "email", nil, "member email (repeatable)")
	rm := &cobra.Command{Use: "rm SAT INDEX", Short: "remove rule at zero-based INDEX", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		v, e := get(cmd, a[0])
		if e != nil {
			return e
		}
		i, e := strconv.Atoi(a[1])
		if e != nil || i < 0 || i >= len(v.Rules) {
			return fmt.Errorf("admit: rule index %q out of range", a[1])
		}
		v.Rules = append(v.Rules[:i], v.Rules[i+1:]...)
		return put(cmd, a[0], v.Rules)
	}}
	var jsonRules string
	set := &cobra.Command{Use: "set SAT --json-rules FILE|-", Short: "replace all admission rules from a JSON array", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		if jsonRules == "" {
			return fmt.Errorf("admit set: --json-rules is required")
		}
		var reader io.Reader = os.Stdin
		if jsonRules != "-" {
			f, e := os.Open(jsonRules)
			if e != nil {
				return e
			}
			defer f.Close()
			reader = f
		}
		var rules []grantRule
		if e := json.NewDecoder(reader).Decode(&rules); e != nil {
			return e
		}
		if rules == nil {
			return fmt.Errorf("admit set: expected a JSON array")
		}
		for i := range rules {
			rules[i].normalize()
		}
		return put(cmd, a[0], rules)
	}}
	set.Flags().StringVar(&jsonRules, "json-rules", "", "file containing a JSON rule array, or - for stdin")
	root.AddCommand(ls, add, rm, set)
	return root
}

func printAdmission(cmd *cobra.Command, raw []byte) error {
	var v admission
	if e := json.Unmarshal(raw, &v); e != nil {
		return e
	}
	return printAdmissionValue(cmd, v)
}
func printAdmissionValue(cmd *cobra.Command, v admission) error {
	for i, r := range v.Rules {
		r.normalize()
		b, _ := json.Marshal(r)
		if _, e := fmt.Fprintf(cmd.OutOrStdout(), "%d\t%s\n", i, b); e != nil {
			return e
		}
	}
	return nil
}
