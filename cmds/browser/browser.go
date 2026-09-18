package browsercmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/browser"
	"github.com/qiangli/yoke/pkg/browser/live"
	"github.com/qiangli/yoke/pkg/browser/probe"
	"github.com/qiangli/yoke/pkg/browser/solo"
	"github.com/qiangli/yoke/pkg/browser/wire"
)

var cmd = &tool.Tool{
	Name:     "browser",
	Synopsis: "Browser automation: status, fetch, and CDP-backed page actions.",
	Usage: "browser [--json] [--mode solo|probe|live] [--probe-url URL] <subcommand> [args]\n" +
		"\n" +
		"Modes (--mode) — how it gets a browser:\n" +
		"  solo   (default) private headless Chrome, zero setup; `navigate URL` returns title/url/content — best for one-shot scrapes\n" +
		"  probe  attach to a Chrome you started with --remote-debugging-port=9222 — persistent session\n" +
		"  live   drive your real logged-in Chrome via `browser hub` + the MV3 extension (cookies/SSO intact)\n" +
		"\n" +
		"Subcommands: status navigate extract eval dispatch-event click type wait-for-selector\n" +
		"  screenshot cookies-get scroll keyboard-press back tabs fetch hub setup login\n" +
		"Each has its OWN page: `browser <subcommand> --help` documents its operands,\n" +
		"which modes implement it, and its output contract.\n" +
		"(--json emits {success,title,url,content,error}; `bashy fetch URL` is the non-browser HTTP client.)\n" +
		"Guide: coreutils/docs/browser.md",
}

func init() { cmd.Run = run; tool.Register(cmd) }

const noBrowserMessage = "no browser: start Chrome with --remote-debugging-port=9222 or run `bashy browser login`"

// helpForSubcommand intercepts `browser <sub> --help` BEFORE the
// shared flagset swallows --help and prints the tool-wide page. That
// interception is the whole mechanism behind per-verb help.
func helpForSubcommand(args []string) (string, bool) {
	var sub string
	wantHelp := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			break
		}
		if a == "--help" || a == "-h" {
			wantHelp = true
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		if sub == "" {
			sub = a
		}
	}
	if !wantHelp || sub == "" {
		return "", false
	}
	return sub, true
}

func run(rc *tool.RunContext, args []string) int {
	if sub, ok := helpForSubcommand(args); ok {
		if printSubHelp(rc, sub) {
			return 0
		}
	}

	fs := tool.NewFlags(cmd.Name)
	asJSON := fs.Bool("json", false, "emit JSON result envelopes")
	mode := fs.String("mode", "solo", "browser mode: solo (zero-setup headless Chrome), probe, or live")
	probeURL := fs.String("probe-url", probe.DefaultURL, "Chrome remote debugging URL for probe mode")
	livePort := fs.Int("live-port", live.DefaultPort, "loopback port of the live-mode hub")
	chromePath := fs.String("chrome-path", "", "Chrome/Chromium executable path for solo mode")
	userDataDir := fs.String("user-data-dir", "", "Chrome user-data-dir for solo mode")
	headed := fs.Bool("headed", false, "run solo Chrome headed instead of headless")
	successURL := fs.String("success-url", "", "login completion URL substring")
	tokenSelector := fs.String("token-selector", "", "CSS selector whose value/text is the login token")
	cookieName := fs.String("cookie", "", "cookie name that indicates login completion")
	dryRun := fs.Bool("dry-run", false, "for login, print what would be polled")
	loginTimeout := fs.Duration("timeout", 2*time.Minute, "login polling timeout")

	// screenshot output contract
	output := fs.String("output", "", "screenshot: write the PNG here and print only the path")
	base64Out := fs.Bool("base64", false, "screenshot: emit inline base64 instead of writing a file")
	fullPage := fs.Bool("full-page", false, "screenshot: capture beyond the viewport")
	settleMs := fs.Int("settle-ms", 0, "screenshot: ms to wait for a paint before capturing (default 150)")
	maxBytes := fs.Int("max-bytes", 0, "screenshot: spill to a file when inline base64 would exceed this")
	// element addressing
	index := fs.Int("index", 0, "click/type: element index from `extract` (1-based)")
	matchText := fs.String("text", "", "click: match an element by its visible text")
	scope := fs.String("scope", "", "extract/click: CSS selector constraining the search root")
	includeHidden := fs.Bool("include-hidden", false, "extract/click: keep elements that are present but not displayed")
	limit := fs.Int("limit", 0, "extract: max elements to return (default 50)")
	offset := fs.Int("offset", 0, "extract: skip this many elements")
	// tab addressing
	tabURL := fs.String("url", "", "tabs: address a tab by URL substring")
	tabTitle := fs.String("title", "", "tabs: address a tab by title substring")
	tabByID := fs.Int("id", 0, "tabs: address a tab by its browser tab id")
	// wait / events
	timeoutMs := fs.Int("timeout-ms", 0, "wait-for-selector: budget in ms (default 5000)")
	state := fs.String("state", "", "wait-for-selector: visible|attached|detached (default visible)")
	detail := fs.String("detail", "", "dispatch-event: JSON detail for a CustomEvent")
	on := fs.String("on", "", "dispatch-event: window|document|<css-selector> (default window)")

	operands, code := tool.Parse(rc, cmd, fs, args)
	if code >= 0 {
		return code
	}
	if len(operands) == 0 {
		return tool.UsageError(rc, cmd, "missing subcommand; expected one of: %s", strings.Join(subcommandNames(), ", "))
	}
	if _, known := subcommands[operands[0]]; !known {
		return tool.UsageError(rc, cmd, "%s", unknownSubcommand(operands[0]))
	}
	ctx := rc.Ctx
	if ctx == nil {
		ctx = context.Background()
	}
	soloCfg := solo.Config{ChromePath: *chromePath, UserDataDir: *userDataDir, Headed: *headed}

	switch operands[0] {
	case "status":
		return browserStatus(rc, ctx, *mode, *probeURL, *livePort, soloCfg, *asJSON)
	case "hub":
		return browserHub(rc, ctx, operands[1:], *asJSON)
	case "setup":
		return browserSetup(rc, operands[1:], *asJSON)
	case "fetch":
		return browserFetch(rc, ctx, operands[1:], *asJSON)
	case "login":
		return browserLogin(rc, ctx, operands[1:], loginOptions{
			Mode:          *mode,
			ProbeURL:      *probeURL,
			LivePort:      *livePort,
			ChromePath:    *chromePath,
			UserDataDir:   *userDataDir,
			Headed:        *headed,
			SuccessURL:    *successURL,
			TokenSelector: *tokenSelector,
			Cookie:        *cookieName,
			DryRun:        *dryRun,
			Timeout:       *loginTimeout,
			JSON:          *asJSON,
		})
	}

	action, err := actionFromArgs(operands, actionFlags{
		Output:        *output,
		Base64:        *base64Out,
		FullPage:      *fullPage,
		SettleMs:      *settleMs,
		MaxBytes:      *maxBytes,
		Index:         *index,
		MatchText:     *matchText,
		Scope:         *scope,
		IncludeHidden: *includeHidden,
		Limit:         *limit,
		Offset:        *offset,
		TabURL:        *tabURL,
		TabTitle:      *tabTitle,
		TabByID:       *tabByID,
		TimeoutMs:     *timeoutMs,
		State:         *state,
		Detail:        *detail,
		On:            *on,
	})
	if err != nil {
		return tool.UsageError(rc, cmd, "%s", err)
	}
	client, err := clientForMode(ctx, *mode, *probeURL, *livePort, soloCfg)
	if err != nil {
		return printNoBrowser(rc, *asJSON, *mode, err)
	}
	defer client.Close()
	res, err := client.Execute(ctx, action)
	if err != nil {
		return printNoBrowser(rc, *asJSON, *mode, err)
	}
	return printResult(rc, res, *asJSON)
}

type loginOptions struct {
	Mode          string
	ProbeURL      string
	LivePort      int
	ChromePath    string
	UserDataDir   string
	Headed        bool
	SuccessURL    string
	TokenSelector string
	Cookie        string
	DryRun        bool
	Timeout       time.Duration
	JSON          bool
}

type loginSpec struct {
	SuccessURL    string `json:"success_url,omitempty"`
	TokenSelector string `json:"token_selector,omitempty"`
	Cookie        string `json:"cookie,omitempty"`
	Domain        string `json:"domain,omitempty"`
}

type loginState struct {
	URL     string
	Token   string
	Cookies []loginCookie
}

type loginCookie struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Domain string `json:"domain"`
}

type loginCompletion struct {
	Done   bool   `json:"done"`
	Reason string `json:"reason,omitempty"`
	Token  string `json:"token,omitempty"`
	Cookie string `json:"cookie,omitempty"`
	URL    string `json:"url,omitempty"`
}

func browserLogin(rc *tool.RunContext, ctx context.Context, args []string, opt loginOptions) int {
	if len(args) != 1 {
		return tool.UsageError(rc, cmd, "login requires URL")
	}
	loginURL := args[0]
	if opt.SuccessURL == "" && opt.TokenSelector == "" && opt.Cookie == "" {
		return tool.UsageError(rc, cmd, "login requires --success-url, --token-selector, or --cookie")
	}
	spec := loginSpec{
		SuccessURL:    opt.SuccessURL,
		TokenSelector: opt.TokenSelector,
		Cookie:        opt.Cookie,
		Domain:        domainForURL(loginURL),
	}
	if opt.DryRun {
		if opt.JSON {
			return writeJSON(rc, map[string]any{"url": loginURL, "poll": spec, "dry_run": true})
		}
		fmt.Fprintf(rc.Out, "url=%s success_url=%q token_selector=%q cookie=%q domain=%q\n", loginURL, spec.SuccessURL, spec.TokenSelector, spec.Cookie, spec.Domain)
		return 0
	}

	client, err := clientForMode(ctx, opt.Mode, opt.ProbeURL, opt.LivePort, solo.Config{ChromePath: opt.ChromePath, UserDataDir: opt.UserDataDir, Headed: opt.Headed})
	if err != nil {
		return printNoBrowser(rc, opt.JSON, opt.Mode, err)
	}
	defer client.Close()
	if res, err := client.Execute(ctx, wire.Action{Type: wire.ActionNavigate, URL: loginURL}); err != nil {
		return printNoBrowser(rc, opt.JSON, opt.Mode, err)
	} else if !res.Success {
		return printResult(rc, res, opt.JSON)
	}

	deadline := time.Now().Add(opt.Timeout)
	for {
		state := pollLoginState(ctx, client, spec)
		done := DetectLoginCompletion(spec, state)
		if done.Done {
			if opt.JSON {
				return writeJSON(rc, done)
			}
			switch {
			case done.Token != "":
				fmt.Fprintln(rc.Out, done.Token)
			case done.Cookie != "":
				fmt.Fprintln(rc.Out, done.Cookie)
			default:
				fmt.Fprintln(rc.Out, done.URL)
			}
			return 0
		}
		if time.Now().After(deadline) {
			if opt.JSON {
				return writeJSON(rc, map[string]any{"done": false, "error": "login timed out"})
			}
			fmt.Fprintln(rc.Err, "browser login: timed out waiting for completion")
			return 1
		}
		select {
		case <-ctx.Done():
			fmt.Fprintf(rc.Err, "browser login: %v\n", ctx.Err())
			return 1
		case <-time.After(time.Second):
		}
	}
}

func pollLoginState(ctx context.Context, client browser.Client, spec loginSpec) loginState {
	var state loginState
	if res, err := client.Execute(ctx, wire.Action{Type: wire.ActionEvaluate, Script: "location.href"}); err == nil && res != nil && res.Success {
		state.URL = res.Data
	}
	if spec.TokenSelector != "" {
		script := fmt.Sprintf(`(function(){var el=document.querySelector(%q); return el ? (el.value || el.textContent || "") : "";})()`, spec.TokenSelector)
		if res, err := client.Execute(ctx, wire.Action{Type: wire.ActionEvaluate, Script: script}); err == nil && res != nil && res.Success {
			state.Token = res.Data
		}
	}
	if spec.Cookie != "" {
		res, err := client.Execute(ctx, wire.Action{Type: wire.ActionCookiesGet, Name: spec.Cookie, Domain: spec.Domain})
		if err == nil && res != nil && res.Success && res.Data != "" {
			_ = json.Unmarshal([]byte(res.Data), &state.Cookies)
		}
	}
	return state
}

func DetectLoginCompletion(spec loginSpec, state loginState) loginCompletion {
	if spec.SuccessURL != "" && strings.Contains(state.URL, spec.SuccessURL) {
		return loginCompletion{Done: true, Reason: "redirect", URL: state.URL}
	}
	if spec.TokenSelector != "" && strings.TrimSpace(state.Token) != "" {
		return loginCompletion{Done: true, Reason: "token", Token: strings.TrimSpace(state.Token), URL: state.URL}
	}
	if spec.Cookie != "" {
		for _, c := range state.Cookies {
			if c.Name == spec.Cookie && c.Value != "" {
				return loginCompletion{Done: true, Reason: "cookie", Cookie: c.Value, URL: state.URL}
			}
		}
	}
	return loginCompletion{Done: false}
}

func domainForURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return ""
	}
	return u.Hostname()
}

func clientForMode(ctx context.Context, mode, probeURL string, livePort int, soloCfg solo.Config) (browser.Client, error) {
	switch mode {
	case "", "probe":
		c := probe.New(probeURL)
		if !c.Available(ctx) {
			return nil, fmt.Errorf("probe target %s not reachable", c.URL())
		}
		if err := c.EnsureReady(ctx); err != nil {
			return nil, err
		}
		return c, nil
	case "solo":
		c := solo.New(soloCfg)
		if !c.Available(ctx) {
			return nil, fmt.Errorf("solo Chrome not found")
		}
		if err := c.EnsureReady(ctx); err != nil {
			return nil, err
		}
		return c, nil
	case "live":
		// Attach to the running hub (started by `bashy browser hub`) and
		// forward actions to the connected extension via /dispatch. If no
		// hub is up, EnsureReady binds one in-process — but a one-shot CLI
		// process exits right after, so the durable path is a separate
		// `bashy browser hub`.
		return live.NewClient(ctx, livePort)
	default:
		return nil, fmt.Errorf("unknown mode %q; expected one of: solo, probe, live", mode)
	}
}

// browserStatus answers, per mode, "would an action issued right now
// reach a page?".
//
// It used to answer `mode "live" is not supported` while every sibling
// subcommand drove live mode happily — a refusal that routed the
// caller to a weaker channel and cost most of a session. Two rules
// now hold: no subcommand claims a mode is unsupported when its
// siblings support it, and a field that does not apply to the mode is
// OMITTED rather than defaulted (a `probe_url` reported in live mode
// is noise that reads as evidence).
func browserStatus(rc *tool.RunContext, ctx context.Context, mode, probeURL string, livePort int, soloCfg solo.Config, asJSON bool) int {
	switch mode {
	case "live":
		st := live.LiveStatus(ctx, livePort)
		if asJSON {
			return writeJSON(rc, st)
		}
		fmt.Fprintf(rc.Out, "mode=live reachable=%t hub_port=%d hub_up=%t extension_connected=%t",
			st.Reachable, st.HubPort, st.HubUp, st.Connected)
		if st.ExtVersion != "" {
			fmt.Fprintf(rc.Out, " extension_version=%s", st.ExtVersion)
		}
		fmt.Fprintln(rc.Out)
		if st.Message != "" {
			fmt.Fprintln(rc.Out, st.Message)
		}
		return 0
	case "", "probe":
		c := probe.New(probeURL)
		reachable := c.Available(ctx)
		message := ""
		if !reachable {
			message = noBrowserMessage
		}
		if asJSON {
			return writeJSON(rc, map[string]any{
				"mode": "probe", "probe_url": probeURL,
				"reachable": reachable, "message": message,
			})
		}
		fmt.Fprintf(rc.Out, "mode=probe reachable=%t probe_url=%s\n", reachable, probeURL)
		if message != "" {
			fmt.Fprintln(rc.Out, message)
		}
		return 0
	case "solo":
		c := solo.New(soloCfg)
		reachable := c.Available(ctx)
		message := ""
		if !reachable {
			message = "no browser: Chrome or Chromium was not found for solo mode (pass --chrome-path)"
		}
		if asJSON {
			return writeJSON(rc, map[string]any{
				"mode": "solo", "reachable": reachable, "message": message,
			})
		}
		fmt.Fprintf(rc.Out, "mode=solo reachable=%t\n", reachable)
		if message != "" {
			fmt.Fprintln(rc.Out, message)
		}
		return 0
	}
	message := fmt.Sprintf("unknown mode %q; expected one of: solo, probe, live", mode)
	if asJSON {
		return writeJSON(rc, map[string]any{"mode": mode, "reachable": false, "message": message})
	}
	fmt.Fprintf(rc.Err, "browser: %s\n", message)
	return 2
}

func browserFetch(rc *tool.RunContext, ctx context.Context, args []string, asJSON bool) int {
	if len(args) != 1 {
		return tool.UsageError(rc, cmd, "fetch requires exactly one URL")
	}
	url := args[0]
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return tool.UsageError(rc, cmd, "fetch URL must start with http:// or https://")
	}
	hctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(hctx, http.MethodGet, url, nil)
	if err != nil {
		fmt.Fprintf(rc.Err, "browser fetch: %v\n", err)
		return 1
	}
	req.Header.Set("User-Agent", "bashy-browser/0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		fmt.Fprintf(rc.Err, "browser fetch: %v\n", err)
		return 1
	}
	defer resp.Body.Close()
	const maxBytes = 1 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBytes+1))
	if err != nil {
		fmt.Fprintf(rc.Err, "browser fetch: %v\n", err)
		return 1
	}
	truncated := len(body) > maxBytes
	if truncated {
		body = body[:maxBytes]
	}
	if asJSON {
		headers := map[string]string{}
		for k := range resp.Header {
			headers[k] = resp.Header.Get(k)
		}
		return writeJSON(rc, map[string]any{
			"url":         url,
			"status":      resp.Status,
			"status_code": resp.StatusCode,
			"headers":     headers,
			"body":        string(body),
			"truncated":   truncated,
		})
	}
	fmt.Fprint(rc.Out, string(body))
	if len(body) == 0 || body[len(body)-1] != '\n' {
		fmt.Fprintln(rc.Out)
	}
	if resp.StatusCode >= 400 {
		return 1
	}
	return 0
}

// actionFlags carries the parsed option values into action building.
type actionFlags struct {
	Output        string
	Base64        bool
	FullPage      bool
	SettleMs      int
	MaxBytes      int
	Index         int
	MatchText     string
	Scope         string
	IncludeHidden bool
	Limit         int
	Offset        int
	TabURL        string
	TabTitle      string
	TabByID       int
	TimeoutMs     int
	State         string
	Detail        string
	On            string
}

// tabActions is the valid set for `tabs`. It is exported through the
// error message so discovery is one command, not eight guesses.
var tabActions = []string{"list", "switch", "new", "close"}

// extraOperands is the guard behind the house rule "anything else
// fails with a clear error (exit 2), never a silent guess". A
// discarded argument that still exits 0 is the same defect class as a
// success that did not happen.
func extraOperands(sub string, rest []string, want int) error {
	if len(rest) <= want {
		return nil
	}
	return fmt.Errorf("%s: unexpected operand %q (see `browser %s --help`)", sub, rest[want], sub)
}

func actionFromArgs(args []string, f actionFlags) (wire.Action, error) {
	sub := args[0]
	rest := args[1:]
	switch sub {
	case "navigate":
		if len(rest) != 1 {
			return wire.Action{}, fmt.Errorf("navigate requires exactly one URL")
		}
		return wire.Action{Type: wire.ActionNavigate, URL: rest[0]}, nil

	case "extract":
		a := wire.Action{
			Type: wire.ActionExtract, Scope: f.Scope, MatchText: f.MatchText,
			Limit: f.Limit, Offset: f.Offset, IncludeHidden: f.IncludeHidden,
		}
		if len(rest) > 0 && a.Scope == "" {
			a.Scope = rest[0]
		}
		if err := extraOperands(sub, rest, 1); err != nil {
			return wire.Action{}, err
		}
		return a, nil

	case "click":
		a := wire.Action{
			Type: wire.ActionClick, ElementID: f.Index, MatchText: f.MatchText,
			Scope: f.Scope, IncludeHidden: f.IncludeHidden,
		}
		if len(rest) > 0 {
			// A bare integer is an ELEMENT INDEX, not a selector: no
			// valid CSS selector is a bare integer, and passing one as
			// a selector is what made `click 36` a silent no-op that
			// still reported success.
			if n, err := strconv.Atoi(rest[0]); err == nil && n > 0 {
				a.ElementID = n
			} else {
				a.Selector = rest[0]
			}
		}
		if err := extraOperands(sub, rest, 1); err != nil {
			return wire.Action{}, err
		}
		if a.Selector == "" && a.ElementID == 0 && a.MatchText == "" {
			return wire.Action{}, fmt.Errorf("click requires a CSS selector, an element index, or --text")
		}
		return a, nil

	case "type":
		if len(rest) < 2 && f.Index == 0 {
			return wire.Action{}, fmt.Errorf("type requires a selector and text")
		}
		a := wire.Action{Type: wire.ActionType, ElementID: f.Index}
		if f.Index > 0 && len(rest) >= 1 {
			a.Text = strings.Join(rest, " ")
		} else {
			a.Selector = rest[0]
			a.Text = strings.Join(rest[1:], " ")
		}
		return a, nil

	case "eval", "evaluate":
		if len(rest) == 0 {
			return wire.Action{}, fmt.Errorf("eval requires a script")
		}
		return wire.Action{Type: wire.ActionEvaluate, Script: strings.Join(rest, " ")}, nil

	case "dispatch-event":
		if len(rest) != 1 {
			return wire.Action{}, fmt.Errorf("dispatch-event requires exactly one event name")
		}
		return wire.Action{
			Type: wire.ActionDispatchEvent, Event: rest[0],
			Detail: f.Detail, Selector: f.On,
		}, nil

	case "screenshot":
		a := wire.Action{
			Type: wire.ActionScreenshot, SavePath: f.Output,
			FullPage: f.FullPage, SettleMs: f.SettleMs, MaxBytes: f.MaxBytes,
		}
		if len(rest) > 0 {
			if f.Output != "" {
				return wire.Action{}, fmt.Errorf("screenshot: pass the path either as an operand or as --output, not both")
			}
			a.SavePath = rest[0]
		}
		if err := extraOperands(sub, rest, 1); err != nil {
			return wire.Action{}, err
		}
		if a.SavePath != "" && f.Base64 {
			return wire.Action{}, fmt.Errorf("screenshot: --base64 and a save path are mutually exclusive")
		}
		if a.SavePath == "" && !f.Base64 {
			// Default to a file. An inline capture is ~200 KB of
			// base64 straight into the caller's stdout (and, for an
			// agent, its context window).
			a.SavePath = defaultShotPath()
		}
		return a, nil

	case "cookies-get":
		a := wire.Action{Type: wire.ActionCookiesGet}
		if len(rest) > 0 {
			a.Name = rest[0]
		}
		if len(rest) > 1 {
			a.Domain = rest[1]
		}
		if err := extraOperands(sub, rest, 2); err != nil {
			return wire.Action{}, err
		}
		return a, nil

	case "wait-for-selector":
		if len(rest) != 1 {
			return wire.Action{}, fmt.Errorf("wait-for-selector requires exactly one selector")
		}
		return wire.Action{
			Type: wire.ActionWaitForSelector, Selector: rest[0],
			TimeoutMs: f.TimeoutMs, State: f.State,
		}, nil

	case "tabs":
		a := wire.Action{
			Type: wire.ActionTabs, TabAction: "list",
			MatchURL: f.TabURL, MatchTitle: f.TabTitle,
		}
		if len(rest) > 0 {
			a.TabAction = rest[0]
			if !slices.Contains(tabActions, a.TabAction) {
				return wire.Action{}, fmt.Errorf(
					"unknown tab action %q; expected one of: %s",
					a.TabAction, strings.Join(tabActions, ", "))
			}
		}
		if f.TabByID > 0 {
			a.TabID = f.TabByID
			a.State = "id"
		}
		switch a.TabAction {
		case "new":
			if len(rest) > 1 {
				a.URL = rest[1]
			}
			if err := extraOperands(sub, rest, 2); err != nil {
				return wire.Action{}, err
			}
		default:
			if len(rest) > 1 {
				n, err := strconv.Atoi(rest[1])
				if err != nil || n <= 0 {
					return wire.Action{}, fmt.Errorf(
						"tabs %s: %q is not a 1-based tab index; address a tab with --url or --title instead (indices go stale whenever a tab opens or closes)",
						a.TabAction, rest[1])
				}
				a.TabID = n
				a.State = ""
			}
			if err := extraOperands(sub, rest, 2); err != nil {
				return wire.Action{}, err
			}
		}
		return a, nil

	case "scroll":
		a := wire.Action{Type: wire.ActionScroll, Direction: "down"}
		if len(rest) > 0 {
			if rest[0] != "up" && rest[0] != "down" {
				return wire.Action{}, fmt.Errorf("scroll: unknown direction %q; expected one of: up, down", rest[0])
			}
			a.Direction = rest[0]
		}
		if len(rest) > 1 {
			n, err := strconv.Atoi(rest[1])
			if err != nil {
				return wire.Action{}, fmt.Errorf("scroll: %q is not a pixel amount", rest[1])
			}
			a.Amount = n
		}
		if err := extraOperands(sub, rest, 2); err != nil {
			return wire.Action{}, err
		}
		return a, nil

	case "keyboard-press":
		if len(rest) != 1 {
			return wire.Action{}, fmt.Errorf("keyboard-press requires exactly one key")
		}
		return wire.Action{Type: wire.ActionKeyboardPress, Key: rest[0]}, nil

	case "back":
		if err := extraOperands(sub, rest, 0); err != nil {
			return wire.Action{}, err
		}
		return wire.Action{Type: wire.ActionBack}, nil
	}
	return wire.Action{}, fmt.Errorf("%s", unknownSubcommand(sub))
}

// defaultShotPath names a fresh PNG under the OS temp dir.
func defaultShotPath() string {
	return filepath.Join(os.TempDir(),
		fmt.Sprintf("bashy-browser-%d.png", time.Now().UnixNano()))
}

func printNoBrowser(rc *tool.RunContext, asJSON bool, mode string, cause error) int {
	if asJSON {
		return writeJSON(rc, map[string]any{
			"success": false,
			"mode":    mode,
			"error":   noBrowserMessage,
			"cause":   cause.Error(),
		})
	}
	fmt.Fprintf(rc.Err, "browser: %s\n", noBrowserMessage)
	return 1
}

func printResult(rc *tool.RunContext, res *wire.Result, asJSON bool) int {
	if res == nil {
		res = &wire.Result{Error: "no result"}
	}
	if asJSON {
		b, _ := json.Marshal(res)
		fmt.Fprintln(rc.Out, string(b))
		if res.Success {
			return 0
		}
		return 1
	}
	if !res.Success {
		fmt.Fprintf(rc.Err, "browser: %s\n", res.Error)
		return 1
	}
	switch {
	// Path first: a screenshot that was written to disk prints ONLY
	// its path. Everything else about that result (capture method, tab
	// id) is metadata, and dumping it ahead of the path would make the
	// output unpipeable.
	case res.Path != "":
		fmt.Fprintln(rc.Out, res.Path)
	case res.Elements != "":
		fmt.Fprintln(rc.Out, res.Elements)
	case res.Content != "":
		fmt.Fprintln(rc.Out, res.Content)
	case res.Image != "":
		fmt.Fprintln(rc.Out, res.Image)
	case res.Data != "":
		fmt.Fprintln(rc.Out, res.Data)
	default:
		fmt.Fprintln(rc.Out, "ok")
	}
	return 0
}

func writeJSON(rc *tool.RunContext, v any) int {
	b, err := json.Marshal(v)
	if err != nil {
		fmt.Fprintf(rc.Err, "browser: json: %v\n", err)
		return 1
	}
	fmt.Fprintln(rc.Out, string(b))
	return 0
}
