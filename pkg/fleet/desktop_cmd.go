package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
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
	reads := []struct{ op, short string }{
		{"schema", "List the declared properties, their types and allowed values"},
		{"get", "Show your resolved desktop and where each value came from"},
		{"catalog", "List the tiles you can order, hide or relabel"},
		{"history", "List your numbered revisions, newest first, with wish text"},
		{"allowance", "Show today's wish allowance, or why wishes are unavailable"},
		{"pages", "List your open desktop pages and their named actions"},
	}
	for _, r := range reads {
		op := r.op
		root.AddCommand(&cobra.Command{Use: op, Short: r.short, Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
	root.AddCommand(&cobra.Command{Use: "wish WORDS...", Short: "Ask for a change in words; it applies only if it validates against the schema", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		return write(cmd, "wish", map[string]any{"wish": strings.Join(a, " ")}, false)
	}})
	var reason string
	request := &cobra.Command{Use: "request WORDS...", Short: "Suggest a refused wish to the owner (people in the owner's circles; words only, nothing applied)", Args: cobra.MinimumNArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		c, err := client(cmd)
		if err != nil {
			return err
		}
		raw, err := c.Do(cmd.Context(), http.MethodPost, "/api/v1/desktop/request", map[string]any{"wish": strings.Join(a, " "), "reason": reason})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}}
	request.Flags().StringVar(&reason, "reason", "", "why it was refused (the refusal text)")
	root.AddCommand(request)
	root.AddCommand(&cobra.Command{Use: "undo", Short: "Step back one change (repeat to keep stepping back)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error { return write(cmd, "undo", map[string]any{}, false) }})
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

	root.AddCommand(newJourneyCmd("journey", client))
	root.AddCommand(newDesktopPolicyCmd())
	return root
}

// NewJourneyCmd is `bashy journey`: the same journey state the desktop
// shows, for an agent. It shares the desktop's account, credentials and
// transport; `bashy desktop journey` is the same command.
func NewJourneyCmd() *cobra.Command {
	var master, token string
	var acct accountFlag
	client := func(cmd *cobra.Command) (*boundary.Client, error) {
		u, t, e := cloudOverrides(cmd.Context(), &acct, master, token)
		if e != nil {
			return nil, e
		}
		return NewMemberClient(u, t)
	}
	root := newJourneyCmd("journey", client)
	root.PersistentFlags().StringVar(&master, "master", "", "Cloudbox URL")
	root.PersistentFlags().StringVar(&token, "token", "", "Bearer token (prefer the saved account credential)")
	root.PersistentFlags().Bool("json", true, "machine-readable output")
	acct.bindPersistent(root)
	return root
}

// newJourneyCmd reads the person's journey (law 2: an invitation is offered
// only after the rung before it was used) and records what they did with
// one. status and next only read; shown, take and dismiss record one event.
func newJourneyCmd(use string, client func(*cobra.Command) (*boundary.Client, error)) *cobra.Command {
	root := &cobra.Command{Use: use, Short: "Read your journey: what you have used and the one invitation earned next", SilenceUsage: true, SilenceErrors: true}
	read := func(cmd *cobra.Command) ([]byte, error) {
		c, err := client(cmd)
		if err != nil {
			return nil, err
		}
		return c.Do(cmd.Context(), http.MethodGet, "/api/v1/desktop/journey", nil)
	}
	root.AddCommand(&cobra.Command{Use: "status", Short: "Show the competence events and every invitation's state", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		raw, err := read(cmd)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}})
	root.AddCommand(&cobra.Command{Use: "next", Short: "Show the one invitation to offer now, or null (reading never marks it shown)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		raw, err := read(cmd)
		if err != nil {
			return err
		}
		var j struct {
			Next json.RawMessage `json:"next"`
		}
		if err = json.Unmarshal(raw, &j); err != nil {
			return err
		}
		if len(j.Next) == 0 {
			j.Next = json.RawMessage("null")
		}
		out, err := json.Marshal(map[string]json.RawMessage{"next": j.Next})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(out))
		return err
	}})
	for _, e := range []struct{ event, short string }{
		{"shown", "Record that an earned invitation was shown to you (once; repeats change nothing)"},
		{"take", "Record that you took an invitation up"},
		{"dismiss", "Dismiss an invitation for good"},
	} {
		event := map[string]string{"shown": "shown", "take": "taken", "dismiss": "dismissed"}[e.event]
		root.AddCommand(&cobra.Command{Use: e.event + " ID", Short: e.short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
			c, err := client(cmd)
			if err != nil {
				return err
			}
			raw, err := c.Do(cmd.Context(), http.MethodPost, "/api/v1/desktop/journey", map[string]any{"invitation": a[0], "event": event})
			if err != nil {
				return err
			}
			_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
			return err
		}})
	}
	return root
}

// newDesktopPolicyCmd sets the daily wish allowance: the master's superadmin
// for people without their own plane, a satellite's owner for its members.
// The route admits a signed-in session only, never an API token, so the
// session is read from the same environment variable as bashy superadmin.
func newDesktopPolicyCmd() *cobra.Command {
	var master string
	policy := &cobra.Command{Use: "policy", Short: "Set the daily wish allowance and the wish route, and read suggested wishes (superadmin, or a satellite's owner)"}
	policy.PersistentFlags().StringVar(&master, "master", "", "Cloudbox or satellite URL (default: the saved account's)")
	client := func(cmd *cobra.Command) (*boundary.Client, error) {
		session := strings.TrimSpace(os.Getenv(SuperadminSessionEnv))
		if session == "" {
			return nil, fmt.Errorf("desktop policy: no signed-in session: sign in (superadmin or satellite owner) and pass the session as %s (API tokens are refused)", SuperadminSessionEnv)
		}
		base := strings.TrimRight(master, "/")
		if base == "" {
			u, _, err := cloudOverrides(cmd.Context(), &accountFlag{}, "", "")
			if err != nil {
				return nil, err
			}
			base = u
		}
		return &boundary.Client{BaseURL: base, Token: session, HTTP: httpClient()}, nil
	}
	call := func(cmd *cobra.Command, method, path string, body any) error {
		c, err := client(cmd)
		if err != nil {
			return err
		}
		raw, err := c.Do(cmd.Context(), method, path, body)
		if err != nil {
			return err
		}
		if len(raw) == 0 {
			raw = []byte(`{"ok":true}`)
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), string(raw))
		return err
	}
	const path = "/api/v1/desktop-policy/allowance"
	policy.AddCommand(&cobra.Command{Use: "ls", Short: "List the default (*) and per-account limits, and the fallback", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return call(cmd, http.MethodGet, path, nil)
	}})
	policy.AddCommand(&cobra.Command{Use: "set OWNER LIMIT", Short: "Set the daily limit for * (everyone) or one account email; 0 switches wishes off", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, a []string) error {
		limit, err := strconv.Atoi(a[1])
		if err != nil {
			return fmt.Errorf("LIMIT must be a whole number: %q", a[1])
		}
		return call(cmd, http.MethodPut, path, map[string]any{"owner": a[0], "daily_limit": limit})
	}})
	policy.AddCommand(&cobra.Command{Use: "unset OWNER", Short: "Remove a limit so the account (or the default) falls back", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		return call(cmd, http.MethodDelete, path+"?owner="+url.QueryEscape(a[0]), nil)
	}})
	policy.AddCommand(newDesktopRouteCmd(call))
	requests := &cobra.Command{Use: "requests", Short: "Refused wishes people in your circles suggested (most asked first)"}
	requests.AddCommand(&cobra.Command{Use: "ls", Short: "List suggestions with how many people asked (never who)", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return call(cmd, http.MethodGet, "/api/v1/desktop-policy/requests", nil)
	}})
	requests.AddCommand(&cobra.Command{Use: "rm ID", Short: "Dismiss a suggestion", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, a []string) error {
		if _, err := strconv.ParseUint(a[0], 10, 64); err != nil {
			return fmt.Errorf("ID must be a number: %q", a[0])
		}
		return call(cmd, http.MethodDelete, "/api/v1/desktop-policy/requests?id="+a[0], nil)
	}})
	policy.AddCommand(requests)
	return policy
}

// newDesktopRouteCmd changes the wish route (URL, model, key) as data, so a
// rotation applies to the next wish without a deploy. The key is read only
// from stdin, never from a flag or an argument, and is never printed.
func newDesktopRouteCmd(call func(*cobra.Command, string, string, any) error) *cobra.Command {
	const path = "/api/v1/desktop-policy/route"
	route := &cobra.Command{Use: "route", Short: "Get, set, unset or test the wish route (URL, model, key)"}
	route.AddCommand(&cobra.Command{Use: "get", Short: "Show the resolved route: url, model, token_set, source", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return call(cmd, http.MethodGet, path, nil)
	}})
	var routeURL, model string
	var tokenStdin, noProbe bool
	set := &cobra.Command{Use: "set", Short: "Set the route; probes it with one wish before saving unless --no-probe", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if f := cmd.Flags().Lookup("token"); f != nil && f.Changed {
			return errors.New("desktop policy route set: the route key is read only from stdin (--token-stdin), never from --token")
		}
		body := map[string]any{"url": routeURL, "model": model, "probe": !noProbe}
		if tokenStdin {
			raw, err := io.ReadAll(cmd.InOrStdin())
			if err != nil {
				return fmt.Errorf("desktop policy route set: read the key from stdin: %w", err)
			}
			key := strings.TrimSpace(string(raw))
			if key == "" {
				return errors.New("desktop policy route set: --token-stdin read an empty key")
			}
			body["token"] = key
		}
		return call(cmd, http.MethodPut, path, body)
	}}
	set.Flags().StringVar(&routeURL, "url", "", "full OpenAI-compatible chat-completions URL, e.g. https://host/v1/chat/completions")
	set.Flags().StringVar(&model, "model", "", "model name sent with each wish")
	set.Flags().BoolVar(&tokenStdin, "token-stdin", false, "read the route key from stdin (the only way to pass it)")
	set.Flags().BoolVar(&noProbe, "no-probe", false, "save without the probe wish")
	_ = set.MarkFlagRequired("url")
	_ = set.MarkFlagRequired("model")
	route.AddCommand(set)
	route.AddCommand(&cobra.Command{Use: "unset", Short: "Remove the route so wishes fall back to the environment", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		return call(cmd, http.MethodDelete, path, nil)
	}})
	var wish string
	test := &cobra.Command{Use: "test", Short: "Send one probe wish through the resolved route and report the result", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		body := map[string]any{}
		if wish != "" {
			body["wish"] = wish
		}
		return call(cmd, http.MethodPost, path+"/test", body)
	}}
	test.Flags().StringVar(&wish, "wish", "", "wish text for the probe (default: the server's)")
	route.AddCommand(test)
	return route
}

// DesktopExitCode is 0 on success, 3 when the server refused with a reason
// (conflict, invalid change, allowance, unavailable, forbidden), 1 otherwise.
func DesktopExitCode(err error) int {
	if err == nil {
		return 0
	}
	var apiErr *boundary.APIError
	if errors.As(err, &apiErr) && (apiErr.Status == 403 || apiErr.Status == 409 || apiErr.Status == 422 || apiErr.Status == 429 || apiErr.Status == 503) {
		return 3
	}
	return 1
}

// DesktopErrorJSON renders an error as the JSON envelope printed under
// --json, so an agent reads the refusal, its HTTP status and reason code.
func DesktopErrorJSON(err error) string {
	out := map[string]any{"error": err.Error(), "exit": DesktopExitCode(err)}
	var apiErr *boundary.APIError
	if errors.As(err, &apiErr) {
		out["error"] = apiErr.Message
		out["status"] = apiErr.Status
		if apiErr.Code != "" {
			out["code"] = apiErr.Code
		}
	}
	b, _ := json.Marshal(out)
	return string(b)
}
