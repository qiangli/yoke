package browsercmd

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/qiangli/coreutils/tool"
)

func runTool(t *testing.T, args ...string) (stdout, stderr string, code int) {
	t.Helper()
	var out, errb bytes.Buffer
	rc := &tool.RunContext{
		Ctx:   context.Background(),
		Dir:   t.TempDir(),
		Stdio: tool.Stdio{In: strings.NewReader(""), Out: &out, Err: &errb},
	}
	code = cmd.Run(rc, args)
	return out.String(), errb.String(), code
}

func TestStatusWorksWithoutBrowser(t *testing.T) {
	out, errb, code := runTool(t, "--mode", "probe", "--probe-url", "http://127.0.0.1:1", "status")
	if code != 0 || errb != "" {
		t.Fatalf("status code=%d stderr=%q", code, errb)
	}
	if !strings.Contains(out, "reachable=false") || !strings.Contains(out, "start Chrome") {
		t.Fatalf("unexpected status output: %q", out)
	}
}

func TestStatusJSONWorksWithoutBrowser(t *testing.T) {
	out, _, code := runTool(t, "--json", "--mode", "probe", "--probe-url", "http://127.0.0.1:1", "status")
	if code != 0 {
		t.Fatalf("status json code=%d out=%q", code, out)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env["reachable"].(bool) {
		t.Fatalf("expected unreachable: %#v", env)
	}
}

func TestFetchWorksWithoutBrowser(t *testing.T) {
	restore := stubHTTP(t, 200, "hello\n")
	defer restore()

	out, errb, code := runTool(t, "fetch", "https://example.test")
	if code != 0 || errb != "" || out != "hello\n" {
		t.Fatalf("fetch = out=%q err=%q code=%d", out, errb, code)
	}
}

func TestFetchJSON(t *testing.T) {
	restore := stubHTTP(t, 200, "json body")
	defer restore()

	out, _, code := runTool(t, "--json", "fetch", "https://example.test")
	if code != 0 {
		t.Fatalf("fetch json code=%d out=%q", code, out)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env["body"] != "json body" || env["status_code"].(float64) != 200 {
		t.Fatalf("unexpected envelope: %#v", env)
	}
}

func stubHTTP(t *testing.T, status int, body string) func() {
	t.Helper()
	old := http.DefaultClient
	http.DefaultClient = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: status,
			Status:     "200 OK",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(body)),
			Request:    req,
		}, nil
	})}
	return func() { http.DefaultClient = old }
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

func TestActionWithoutBrowserPrintsClearMessage(t *testing.T) {
	_, errb, code := runTool(t, "--mode", "probe", "--probe-url", "http://127.0.0.1:1", "navigate", "https://example.com")
	if code == 0 || !strings.Contains(errb, noBrowserMessage) {
		t.Fatalf("action without browser = code=%d stderr=%q", code, errb)
	}
}

// TestDefaultModeIsSolo guards the zero-setup default: with no --mode, status
// reports solo (reachability then depends on whether a Chrome binary exists,
// so we assert only the mode, never launch a browser).
func TestDefaultModeIsSolo(t *testing.T) {
	out, _, code := runTool(t, "--json", "status")
	if code != 0 {
		t.Fatalf("status code=%d out=%q", code, out)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env["mode"] != "solo" {
		t.Fatalf("default mode = %v, want solo", env["mode"])
	}
}

func TestActionFromArgs(t *testing.T) {
	a, err := actionFromArgs([]string{"type", "#q", "hello", "world"}, actionFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != "type" || a.Selector != "#q" || a.Text != "hello world" {
		t.Fatalf("unexpected action: %#v", a)
	}
}

// TestClickByIndexIsNotASelector pins the fix for the silent no-op:
// `click 36` used to be sent as Selector:"36", which is not valid CSS,
// so the injected querySelector threw, the extension returned an empty
// frame, and the caller was told {"success":true}.
func TestClickByIndexIsNotASelector(t *testing.T) {
	a, err := actionFromArgs([]string{"click", "36"}, actionFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if a.ElementID != 36 {
		t.Fatalf("click 36: ElementID=%d want 36 (action=%#v)", a.ElementID, a)
	}
	if a.Selector != "" {
		t.Fatalf("click 36: Selector=%q, want empty — a bare integer is an index", a.Selector)
	}

	b, err := actionFromArgs([]string{"click"}, actionFlags{Index: 7})
	if err != nil {
		t.Fatal(err)
	}
	if b.ElementID != 7 {
		t.Fatalf("--index 7: ElementID=%d want 7", b.ElementID)
	}

	c, err := actionFromArgs([]string{"click", "[aria-label=\"Open\"]"}, actionFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if c.Selector != "[aria-label=\"Open\"]" || c.ElementID != 0 {
		t.Fatalf("selector form mis-parsed: %#v", c)
	}

	if _, err := actionFromArgs([]string{"click"}, actionFlags{}); err == nil {
		t.Fatal("click with no target must be a usage error, not a no-op")
	}
}

// TestScreenshotPathIsNotDiscarded pins the fix for `screenshot
// /tmp/x.png` exiting 0 having written no file and dumped 223 KB of
// base64 to stdout instead.
func TestScreenshotPathIsNotDiscarded(t *testing.T) {
	a, err := actionFromArgs([]string{"screenshot", "/tmp/shot-test.png"}, actionFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if a.SavePath != "/tmp/shot-test.png" {
		t.Fatalf("positional path dropped: %#v", a)
	}
	b, err := actionFromArgs([]string{"screenshot"}, actionFlags{Output: "/tmp/out.png"})
	if err != nil {
		t.Fatal(err)
	}
	if b.SavePath != "/tmp/out.png" {
		t.Fatalf("--output dropped: %#v", b)
	}
	// Default is a FILE, not 200 KB of base64 into the caller's stdout.
	c, err := actionFromArgs([]string{"screenshot"}, actionFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if c.SavePath == "" {
		t.Fatal("screenshot with no args must default to writing a file")
	}
	if _, err := actionFromArgs([]string{"screenshot"}, actionFlags{Base64: true}); err != nil {
		t.Fatal(err)
	}
	if d, _ := actionFromArgs([]string{"screenshot"}, actionFlags{Base64: true}); d.SavePath != "" {
		t.Fatalf("--base64 must not write a file: %#v", d)
	}
	if _, err := actionFromArgs([]string{"screenshot", "/tmp/a.png"}, actionFlags{Base64: true}); err == nil {
		t.Fatal("--base64 with a path must be a usage error")
	}
}

// TestUnrecognisedOperandsFail pins the house rule: anything else
// fails with a clear error, never a silent guess.
func TestUnrecognisedOperandsFail(t *testing.T) {
	for _, args := range [][]string{
		{"back", "extra"},
		{"screenshot", "/tmp/a.png", "extra"},
		{"extract", "#main", "extra"},
		{"scroll", "sideways"},
	} {
		if _, err := actionFromArgs(args, actionFlags{}); err == nil {
			t.Fatalf("%v: expected a usage error, got none", args)
		}
	}
}

// TestTabActionErrorNamesTheValidSet: the error already knows the
// action is invalid, so it knows the valid set. Discovering
// list/switch/new/close used to take eight guesses.
func TestTabActionErrorNamesTheValidSet(t *testing.T) {
	_, err := actionFromArgs([]string{"tabs", "activate"}, actionFlags{})
	if err == nil {
		t.Fatal("expected an error for an invalid tab action")
	}
	for _, want := range []string{"list", "switch", "new", "close"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %q", err, want)
		}
	}
	// A bare integer is still a 1-based index.
	a, err := actionFromArgs([]string{"tabs", "switch", "4"}, actionFlags{})
	if err != nil {
		t.Fatal(err)
	}
	if a.TabID != 4 {
		t.Fatalf("tabs switch 4: TabID=%d want 4", a.TabID)
	}
	// …and substring addressing is available so scripts need not race.
	b, err := actionFromArgs([]string{"tabs", "switch"}, actionFlags{TabURL: "localhost:5478"})
	if err != nil {
		t.Fatal(err)
	}
	if b.MatchURL != "localhost:5478" {
		t.Fatalf("--url dropped: %#v", b)
	}
}

// TestUnknownSubcommandNamesTheValidSet mirrors the tab-action rule at
// the top level.
func TestUnknownSubcommandNamesTheValidSet(t *testing.T) {
	_, errb, code := runTool(t, "shoot")
	if code != 2 {
		t.Fatalf("unknown subcommand exit=%d want 2", code)
	}
	for _, want := range []string{"screenshot", "navigate", "tabs"} {
		if !strings.Contains(errb, want) {
			t.Fatalf("error %q does not name %q", errb, want)
		}
	}
}

// TestPerSubcommandHelp: `browser tabs --help` used to be
// byte-identical to `browser --help`. Fourteen subcommands shared one
// page that documented none of them.
func TestPerSubcommandHelp(t *testing.T) {
	toolHelp, _, _ := runTool(t, "--help")
	for _, sub := range []string{"tabs", "screenshot", "click", "extract", "status"} {
		out, _, code := runTool(t, sub, "--help")
		if code != 0 {
			t.Fatalf("%s --help exit=%d", sub, code)
		}
		if out == toolHelp {
			t.Fatalf("%s --help is identical to the tool help", sub)
		}
		if !strings.Contains(out, "Output:") {
			t.Fatalf("%s --help does not state its output contract: %q", sub, out)
		}
	}
	// screenshot's page must state the default the caller actually gets.
	shot, _, _ := runTool(t, "screenshot", "--help")
	if !strings.Contains(shot, "path") || !strings.Contains(shot, "--base64") {
		t.Fatalf("screenshot help omits its output contract: %q", shot)
	}
	// tabs must name its vocabulary.
	tabs, _, _ := runTool(t, "tabs", "--help")
	for _, want := range []string{"list", "switch", "new", "close"} {
		if !strings.Contains(tabs, want) {
			t.Fatalf("tabs help omits %q", want)
		}
	}
}

// TestLiveStatusDoesNotDenyLiveMode pins the class-C fix: status must
// not report a mode unsupported while its sibling subcommands support
// it. With no hub running the honest answer is "not reachable, here is
// how to start one" — never "mode is not supported".
func TestLiveStatusDoesNotDenyLiveMode(t *testing.T) {
	out, _, code := runTool(t, "--json", "--mode", "live", "--live-port", "1", "status")
	if code != 0 {
		t.Fatalf("live status exit=%d out=%q", code, out)
	}
	var env map[string]any
	if err := json.Unmarshal([]byte(out), &env); err != nil {
		t.Fatal(err)
	}
	if env["mode"] != "live" {
		t.Fatalf("mode=%v want live", env["mode"])
	}
	msg, _ := env["message"].(string)
	if strings.Contains(msg, "not supported") {
		t.Fatalf("live status still denies the mode: %q", msg)
	}
	if !strings.Contains(msg, "browser hub") {
		t.Fatalf("live status names no remedy: %q", msg)
	}
	// probe_url is meaningless in live mode and must not be reported.
	if _, ok := env["probe_url"]; ok {
		t.Fatalf("live status reports probe_url: %#v", env)
	}
	if _, ok := env["hub_port"]; !ok {
		t.Fatalf("live status omits hub_port: %#v", env)
	}
}

// TestDispatchEventAction pins the CSP remedy's plumbing.
func TestDispatchEventAction(t *testing.T) {
	a, err := actionFromArgs([]string{"dispatch-event", "toggle-activity-panel"},
		actionFlags{Detail: `{"open":true}`, On: "document"})
	if err != nil {
		t.Fatal(err)
	}
	if a.Type != "dispatch_event" || a.Event != "toggle-activity-panel" ||
		a.Detail != `{"open":true}` || a.Selector != "document" {
		t.Fatalf("unexpected action: %#v", a)
	}
}

func TestLoginCompletionDetector(t *testing.T) {
	tests := []struct {
		name  string
		spec  loginSpec
		state loginState
		want  string
		value string
	}{
		{
			name:  "redirect",
			spec:  loginSpec{SuccessURL: "/done"},
			state: loginState{URL: "https://example.test/oauth/done?code=1"},
			want:  "redirect",
			value: "https://example.test/oauth/done?code=1",
		},
		{
			name:  "token",
			spec:  loginSpec{TokenSelector: "#token"},
			state: loginState{Token: " abc "},
			want:  "token",
			value: "abc",
		},
		{
			name:  "cookie",
			spec:  loginSpec{Cookie: "sid"},
			state: loginState{Cookies: []loginCookie{{Name: "sid", Value: "secret"}}},
			want:  "cookie",
			value: "secret",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DetectLoginCompletion(tt.spec, tt.state)
			if !got.Done || got.Reason != tt.want {
				t.Fatalf("unexpected completion: %#v", got)
			}
			switch tt.want {
			case "redirect":
				if got.URL != tt.value {
					t.Fatalf("URL=%q want %q", got.URL, tt.value)
				}
			case "token":
				if got.Token != tt.value {
					t.Fatalf("token=%q want %q", got.Token, tt.value)
				}
			case "cookie":
				if got.Cookie != tt.value {
					t.Fatalf("cookie=%q want %q", got.Cookie, tt.value)
				}
			}
		})
	}
}

func TestLoginDryRun(t *testing.T) {
	out, errb, code := runTool(t, "--dry-run", "--success-url", "/ok", "login", "https://example.test/login")
	if code != 0 || errb != "" {
		t.Fatalf("dry run code=%d err=%q", code, errb)
	}
	if !strings.Contains(out, `success_url="/ok"`) || !strings.Contains(out, `domain="example.test"`) {
		t.Fatalf("unexpected dry run: %q", out)
	}
}
