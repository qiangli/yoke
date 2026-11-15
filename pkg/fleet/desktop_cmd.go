package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/qiangli/yoke/pkg/boundary"
	"github.com/spf13/cobra"
)

// NewDesktopCmd adds only the hosted desktop operations. Account selection,
// credentials and HTTP transport are the same ones used by member and suite.
func NewDesktopCmd() *cobra.Command {
	var master, token string
	var acct accountFlag
	var expected int64
	var dry bool
	root := &cobra.Command{Use: "desktop", Short: "Inspect and shape your Cloudbox desktop", SilenceUsage: true, SilenceErrors: true}
	root.PersistentFlags().StringVar(&master, "master", "", "Cloudbox URL")
	root.PersistentFlags().StringVar(&token, "token", "", "Bearer token (prefer the saved account credential)")
	root.PersistentFlags().Bool("json", true, "machine-readable output")
	root.PersistentFlags().Int64Var(&expected, "revision", -1, "expected revision; default reads the current revision once")
	acct.bindPersistent(root)
	client := func(cmd *cobra.Command) (*boundary.Client, error) {
		u, t, e := cloudOverrides(cmd.Context(), &acct, master, token)
		if e != nil {
			return nil, e
		}
		return NewMemberClient(u, t)
	}
	for _, name := range []string{"schema", "get", "catalog", "history", "allowance", "pages"} {
		op := name
		root.AddCommand(&cobra.Command{Use: op, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
			c, e := client(cmd)
			if e != nil {
				return e
			}
			raw, e := c.Do(cmd.Context(), http.MethodGet, "/api/v1/desktop/"+op, nil)
			if e != nil {
				return e
			}
			_, e = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return e
		}})
	}
	root.AddCommand(&cobra.Command{Use: "result ID", Short: "Read a prior page command outcome without replaying it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client(cmd)
		if err != nil {
			return err
		}
		raw, err := c.Do(cmd.Context(), http.MethodGet, "/api/v1/desktop/result?id="+url.QueryEscape(args[0]), nil)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}})
	write := func(cmd *cobra.Command, op string, body map[string]any, toggle bool) error {
		c, e := client(cmd)
		if e != nil {
			return e
		}
		revision := expected
		if revision < 0 || toggle {
			raw, e := c.Do(cmd.Context(), http.MethodGet, "/api/v1/desktop/get", nil)
			if e != nil {
				return e
			}
			var s struct {
				Revision int64          `json:"revision"`
				Resolved map[string]any `json:"resolved"`
			}
			if e = json.Unmarshal(raw, &s); e != nil {
				return e
			}
			if revision < 0 {
				revision = s.Revision
			}
			if toggle {
				on, _ := s.Resolved["shapeOnly"].(bool)
				body["patch"] = map[string]any{"shapeOnly": !on}
			}
		}
		if op == "wish" {
			c.HTTP = &http.Client{Timeout: 50 * time.Second}
		}
		body["expected_revision"] = revision
		raw, e := c.Do(cmd.Context(), http.MethodPost, "/api/v1/desktop/"+op, body)
		if e != nil {
			return e
		}
		_, e = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return e
	}
	set := &cobra.Command{Use: "set PROPERTY VALUE", Short: "Set a declared property (JSON values or plain strings)", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		var value any
		if json.Unmarshal([]byte(a[1]), &value) != nil {
			value = a[1]
		}
		return write(cmd, "set", map[string]any{"patch": map[string]any{a[0]: value}, "dry_run": dry}, false)
	}}
	set.Flags().BoolVar(&dry, "dry-run", false, "validate without saving a revision")
	root.AddCommand(set)
	root.AddCommand(&cobra.Command{Use: "wish WORDS...", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		return write(cmd, "wish", map[string]any{"wish": strings.Join(a, " ")}, false)
	}})
	root.AddCommand(&cobra.Command{Use: "undo", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return write(cmd, "undo", map[string]any{}, false) }})
	root.AddCommand(&cobra.Command{Use: "shape-only [on|off|toggle]", Short: "Toggle the single-Shape-button presentation", Args: cobra.MaximumNArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		mode := "toggle"
		if len(a) > 0 {
			mode = a[0]
		}
		if mode != "on" && mode != "off" && mode != "toggle" {
			return fmt.Errorf("expected on, off or toggle")
		}
		return write(cmd, "set", map[string]any{"patch": map[string]any{"shapeOnly": mode == "on"}}, mode == "toggle")
	}})
	root.AddCommand(&cobra.Command{Use: "invoke PAGE ACTION [JSON]", Short: "Invoke a mounted page handler and wait for completion", Args: cobra.RangeArgs(2, 3), RunE: func(cmd *cobra.Command, args []string) error {
		c, err := client(cmd)
		if err != nil {
			return err
		}
		var input any
		if len(args) == 3 {
			if err = json.Unmarshal([]byte(args[2]), &input); err != nil {
				return fmt.Errorf("invalid action JSON: %w", err)
			}
		}
		raw, err := c.Do(cmd.Context(), http.MethodPost, "/api/v1/desktop/invoke", map[string]any{"page_id": args[0], "action": args[1], "input": input})
		if err != nil {
			return err
		}
		var result struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err = json.Unmarshal(raw, &result); err != nil {
			return err
		}
		deadline := time.NewTimer(35 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(250 * time.Millisecond)
		defer tick.Stop()
		for {
			select {
			case <-cmd.Context().Done():
				return cmd.Context().Err()
			case <-deadline.C:
				return fmt.Errorf("command %s outcome unknown; not retried", result.ID)
			case <-tick.C:
				raw, err = c.Do(cmd.Context(), http.MethodGet, "/api/v1/desktop/result?id="+url.QueryEscape(result.ID), nil)
				if err != nil {
					return err
				}
				if err = json.Unmarshal(raw, &result); err != nil {
					return err
				}
				if result.Status == "completed" {
					_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
					return err
				}
				if result.Status == "failed" || result.Status == "expired" {
					return fmt.Errorf("command %s %s: %s; not retried", result.ID, result.Status, result.Error)
				}
			}
		}
	}})

	return root
}

func DesktopExitCode(err error) int {
	if err == nil {
		return 0
	}
	var apiErr *boundary.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 403 || apiErr.Status == 409 || apiErr.Status == 422 || apiErr.Status == 429) {
		return 3
	}
	return 1
}
