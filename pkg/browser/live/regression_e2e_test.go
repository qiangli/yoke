package live

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"

	cdpbrowser "github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"

	"github.com/qiangli/yoke/pkg/browser/wire"
)

func TestExtensionE2ELegacyTabLifecycleAndHubResume(t *testing.T) {
	isolateGate(t)
	svc, browserCtx := startExtensionE2E(t, map[string]string{
		"/one": "<title>Baseline One</title><input id='note' aria-label='Note'>",
		"/two": "<title>Baseline Two</title><input id='note' aria-label='Note'>",
	}, "/one")
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	client, err := NewClient(ctx, svc.Port())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	run := func(a wire.Action) *wire.Result {
		t.Helper()
		r, err := client.Execute(ctx, a)
		if err != nil || !r.Success {
			t.Fatalf("%s/%s: %+v, %v", a.Type, a.TabAction, r, err)
		}
		return r
	}

	first := run(wire.Action{Type: wire.ActionExtract, MatchTitle: "Baseline One"})
	base := strings.TrimSuffix(first.URL, "/one")
	second := run(wire.Action{Type: wire.ActionTabs, TabAction: "new", URL: base + "/two"})
	if first.TabID == 0 || second.TabID == 0 || first.TabID == second.TabID {
		t.Fatalf("expected distinct tabs, got %d and %d", first.TabID, second.TabID)
	}
	run(wire.Action{Type: wire.ActionWaitForSelector, TabID: second.TabID, Selector: "#note"})
	if r := run(wire.Action{Type: wire.ActionTabs, TabAction: "switch", TabID: first.TabID, State: "id"}); r.TabID != first.TabID {
		t.Fatalf("switch selected tab %d, want %d", r.TabID, first.TabID)
	}
	list := run(wire.Action{Type: wire.ActionTabs, TabAction: "list", MatchTitle: "Baseline"})
	var tabs []struct {
		ID     int
		Active bool
		Driven bool
	}
	if err := json.Unmarshal([]byte(list.Data), &tabs); err != nil || len(tabs) != 2 {
		t.Fatalf("tab listing: %s, %v", list.Data, err)
	}
	for _, tab := range tabs {
		want := tab.ID == first.TabID
		if tab.Active != want || tab.Driven != want {
			t.Fatalf("tab %d active/driven = %t/%t, want %t", tab.ID, tab.Active, tab.Driven, want)
		}
	}
	run(wire.Action{Type: wire.ActionType, Selector: "#note", Text: "before pause"})
	if g, err := SetGate(ctx, svc.Port(), true, "regression-test"); err != nil || !g.Paused {
		t.Fatalf("pause: %+v, %v", g, err)
	}
	if r, err := client.Execute(ctx, wire.Action{Type: wire.ActionType, Selector: "#note", Text: "blocked"}); err == nil && (r == nil || r.Success) {
		t.Fatalf("write succeeded while paused: %+v", r)
	}
	var value string
	if err := chromedp.Run(browserCtx, chromedp.Value("#note", &value, chromedp.ByQuery)); err != nil || value != "before pause" {
		t.Fatalf("paused write changed the fixture: %q, %v", value, err)
	}
	if g, err := SetGate(ctx, svc.Port(), false, "regression-test"); err != nil || g.Paused {
		t.Fatalf("resume: %+v, %v", g, err)
	}
	for !LiveStatus(ctx, svc.Port()).Reachable {
		select {
		case <-ctx.Done():
			t.Fatal("real extension did not reconnect after resume")
		case <-time.After(100 * time.Millisecond):
		}
	}

	if r := run(wire.Action{Type: wire.ActionExtract}); r.TabID != first.TabID {
		t.Fatalf("reconnect lost the live target: %+v", r)
	}
	run(wire.Action{Type: wire.ActionTabs, TabAction: "switch", TabID: second.TabID, State: "id"})
	run(wire.Action{Type: wire.ActionTabs, TabAction: "close", TabID: second.TabID, State: "id"})
	if r := run(wire.Action{Type: wire.ActionExtract}); r.TabID != first.TabID {
		t.Fatalf("legacy fallback after closing the driven tab selected %d, want %d", r.TabID, first.TabID)
	}
	r, err := client.Execute(ctx, wire.Action{Type: wire.ActionExtract, TabID: second.TabID})
	if err == nil && (r == nil || r.Success || !strings.Contains(r.Error, "no tab")) {
		t.Fatalf("explicitly closed tab should be refused: %+v", r)
	}
}

func TestExtensionE2EStorageCookiesAndDiagnostics(t *testing.T) {
	svc, _ := startExtensionE2E(t, map[string]string{"/": "<title>Baseline</title>"}, "/")
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/sample" {
			_, _ = w.Write([]byte("sample"))
			return
		}
		http.SetCookie(w, &http.Cookie{Name: "baseline_fixture", Value: "local-test", HttpOnly: true, Path: "/"})
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte(`<!doctype html><title>Baseline Diagnostics</title>
			<script>localStorage.setItem("marker", "local"); sessionStorage.setItem("marker", "session");</script>`))
	}))
	t.Cleanup(fixture.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	run := func(a wire.Action) *wire.Result {
		t.Helper()
		r, err := svc.Execute(ctx, a)
		if err != nil || !r.Success {
			t.Fatalf("%s: %+v, %v", a.Type, r, err)
		}
		return r
	}
	run(wire.Action{Type: wire.ActionNavigate, URL: fixture.URL})
	for _, kind := range []string{"local", "session"} {
		r := run(wire.Action{Type: wire.ActionStorageGet, Storage: kind, StorageKey: "marker", MatchTitle: "Baseline Diagnostics"})
		d := e2eData(t, r)
		if d["key"] != "marker" || d["value"] != kind {
			t.Fatalf("%s storage = %v", kind, d)
		}
	}
	cookieResult := run(wire.Action{Type: wire.ActionCookiesGet, Name: "baseline_fixture"})
	var cookies []struct {
		Name     string
		Value    string
		HTTPOnly bool
	}
	if err := json.Unmarshal([]byte(cookieResult.Data), &cookies); err != nil || len(cookies) != 1 ||
		cookies[0].Name != "baseline_fixture" || cookies[0].Value != "local-test" || !cookies[0].HTTPOnly {
		t.Fatalf("cookie filter / HttpOnly data: %s, %v", cookieResult.Data, err)
	}
	run(wire.Action{Type: wire.ActionNetworkList})
	run(wire.Action{Type: wire.ActionConsoleGet})
	run(wire.Action{Type: wire.ActionPerfStart})
	run(wire.Action{Type: wire.ActionEvaluate, Script: `void fetch("/sample").then(() => console.info("baseline console marker"))`})
	for deadline := time.Now().Add(5 * time.Second); ; {
		network := run(wire.Action{Type: wire.ActionNetworkList})
		console := run(wire.Action{Type: wire.ActionConsoleGet})
		if strings.Contains(network.Data, fixture.URL+"/sample") && strings.Contains(console.Data, "baseline console marker") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fixture events missing: network=%s console=%s", network.Data, console.Data)
		}
		time.Sleep(50 * time.Millisecond)
	}
	trace := e2eData(t, run(wire.Action{Type: wire.ActionPerfStop}))
	if ms, ok := trace["duration_ms"].(float64); !ok || ms <= 0 || trace["event_count"] == nil {
		t.Fatalf("trace summary: %v", trace)
	}
}

func TestExtensionE2EClipboard(t *testing.T) {
	svc, browserCtx := startExtensionE2E(t, map[string]string{
		"/": "<title>Baseline Clipboard</title>",
	}, "/")
	var origin string
	err := chromedp.Run(browserCtx,
		page.BringToFront(),
		emulation.SetFocusEmulationEnabled(true),
		chromedp.Poll("document.hasFocus()", nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.Evaluate("location.origin", &origin),
		chromedp.ActionFunc(func(ctx context.Context) error {
			browser := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
			for _, permission := range []string{"clipboard-read", "clipboard-write"} {
				err := cdpbrowser.SetPermission(&cdpbrowser.PermissionDescriptor{
					Name: permission,
				}, cdpbrowser.PermissionSettingGranted).WithOrigin(origin).Do(browser)
				if err != nil {
					return err
				}
			}
			return nil
		}),
	)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	run := func(a wire.Action) *wire.Result {
		t.Helper()
		r, err := svc.Execute(ctx, a)
		if err != nil {
			t.Fatalf("%s: %v", a.Type, err)
		}
		if !r.Success {
			t.Fatalf("%s: %s", a.Type, r.Error)
		}
		return r
	}
	// Headless Chrome supplies its own clipboard. Restore its original value.
	read := wire.Action{Type: wire.ActionClipboardRead}
	original := run(read).Data
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		r, err := svc.Execute(ctx, wire.Action{Type: wire.ActionClipboardWrite, Text: original})
		if err != nil || !r.Success {
			t.Error("could not restore the fixture clipboard")
		}
	})
	const marker = "sprint 704 headless clipboard fixture"
	run(wire.Action{Type: wire.ActionClipboardWrite, Text: marker})
	if run(read).Data != marker {
		t.Fatal("clipboard did not return the fixture value")
	}
}

func TestExtensionE2EInstalledCLI(t *testing.T) {
	bin := os.Getenv("BASHY_BROWSER_E2E_BIN")
	if bin == "" {
		t.Skip("set BASHY_BROWSER_E2E_BIN to the candidate executable")
	}
	isolateGate(t)
	svc, browserCtx := startExtensionE2E(t, map[string]string{
		"/": "<title>Baseline CLI</title><label for='note'>Note</label><input id='note'>",
	}, "/")
	var url string
	if err := chromedp.Run(browserCtx, chromedp.Location(&url)); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--mode", "live", "--live-port", strconv.Itoa(svc.Port())}
	run := func(args ...string) map[string]any {
		t.Helper()
		return browserCLIJSON(t, bin, flags, args...)
	}
	if d := run("status"); d["reachable"] != true {
		t.Fatalf("status: %v", d)
	}
	extract := run("--title", "Baseline CLI", "extract")
	if _, ok := extract["data"].(map[string]any); extract["success"] != true || !ok {
		t.Fatalf("extract should expose structured JSON: %v", extract)
	}
	if d := run("type", "label=Note", "installed CLI fixture"); d["success"] != true {
		t.Fatalf("legacy type: %v", d)
	}
	var value string
	if err := chromedp.Run(browserCtx, chromedp.Value("#note", &value, chromedp.ByQuery)); err != nil || value != "installed CLI fixture" {
		t.Fatalf("CLI did not update the intended input: %q, %v", value, err)
	}
	if d := run("--success-url", url, "login", url); d["done"] != true {
		t.Fatalf("local login completion: %v", d)
	}
}

func TestExtensionE2EProfileTakeoverFromPopup(t *testing.T) {
	isolateGate(t)
	svc, _ := startExtensionE2E(t, map[string]string{"/": "<title>First profile</title>"}, "/")
	dir := t.TempDir()
	extractFixtureExtension(t, dir, svc.Port())
	alloc, stop := chromedp.NewExecAllocator(context.Background(),
		liveE2EAllocatorOptions(dir, t.TempDir(), e2eChrome(t))...)
	second, closeSecond := chromedp.NewContext(alloc)
	t.Cleanup(func() { closeFixtureBrowser(second, closeSecond, stop) })
	ctx, cancel := context.WithTimeout(second, 30*time.Second)
	defer cancel()
	// The second extension starts with the same automatic pairing intent.
	// Only an explicit Connect from its popup may replace the first profile.
	if err := chromedp.Run(second, chromedp.Navigate("about:blank")); err != nil {
		t.Fatal(err)
	}
	_, base := ourWorker(t, ctx)
	popupURL := base + "popup.html"
	browser := cdp.WithExecutor(ctx, chromedp.FromContext(second).Browser)
	popupID, err := target.CreateTarget(popupURL).Do(browser)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.ActivateTarget(popupID).Do(browser); err != nil {
		t.Fatal(err)
	}
	popup, closePopup := chromedp.NewContext(ctx, chromedp.WithTargetID(popupID))
	defer closePopup()
	if err := chromedp.Run(popup,
		chromedp.WaitReady("#port", chromedp.ByQuery),
		chromedp.Evaluate(fmt.Sprintf(`document.querySelector("#port").value = %q`, strconv.Itoa(svc.Port())), nil),
	); err != nil {
		t.Fatalf("open second profile popup: %v", err)
	}
	for deadline := time.Now().Add(5 * time.Second); ; {
		svc.hub.mu.Lock()
		refused := svc.hub.refusedLogged
		svc.hub.mu.Unlock()
		if refused {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("hub did not refuse the second profile's automatic connection")
		}
		time.Sleep(50 * time.Millisecond)
	}
	readTabs := func() string {
		t.Helper()
		r, err := svc.Execute(ctx, wire.Action{Type: wire.ActionTabs, TabAction: "list"})
		if err != nil || !r.Success {
			t.Fatalf("tabs list: %+v, %v", r, err)
		}
		return r.Data
	}
	if current := readTabs(); !strings.Contains(current, "First profile") {
		t.Fatalf("an unpaired profile changed the route: %s", current)
	}
	if err := chromedp.Run(popup,
		chromedp.Evaluate(`document.querySelector("#connect").click()`, nil),
		chromedp.Poll(`document.querySelector("#status").className === "connected"`, nil,
			chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(5*time.Second)),
	); err != nil {
		t.Fatal(err)
	}
	if current := readTabs(); !strings.Contains(current, popupURL) || strings.Contains(current, "First profile") {
		t.Fatalf("Connect did not replace the first profile: %s", current)
	}
	if err := chromedp.Run(popup,
		chromedp.Evaluate(`document.querySelector("#disconnect").click()`, nil),
		chromedp.Poll(`document.querySelector("#status").textContent === "disconnected"`, nil,
			chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(5*time.Second)),
	); err != nil {
		t.Fatal(err)
	}
}

func TestBrowserE2EProbeOneShotCLIKeepsTab(t *testing.T) {
	bin := os.Getenv("BASHY_BROWSER_E2E_BIN")
	if os.Getenv("BASHY_BROWSER_LIVE") == "" || bin == "" {
		t.Skip("requires the real-browser gate and candidate executable")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write([]byte("<title>Probe fixture</title><label for='note'>Note</label><input id='note'>"))
	}))
	t.Cleanup(fixture.Close)
	port := freePort(t)
	profile := t.TempDir()
	opts := append(chromedp.DefaultExecAllocatorOptions[:0:0], chromedp.DefaultExecAllocatorOptions[:]...)
	opts = append(opts, chromedp.ExecPath(e2eChrome(t)), chromedp.UserDataDir(profile),
		chromedp.Flag("remote-debugging-port", strconv.Itoa(port)), chromedp.Flag("use-mock-keychain", true))
	alloc, stop := chromedp.NewExecAllocator(context.Background(), opts...)
	browserCtx, closeBrowser := chromedp.NewContext(alloc)
	t.Cleanup(func() { closeFixtureBrowser(browserCtx, closeBrowser, stop) })
	if err := chromedp.Run(browserCtx, chromedp.Navigate(fixture.URL)); err != nil {
		t.Fatal(err)
	}
	flags := []string{"--mode", "probe", "--probe-url", fmt.Sprintf("http://127.0.0.1:%d", port)}
	if r := browserCLIJSON(t, bin, flags, "type", "label=Note", "probe fixture value"); r["success"] != true {
		t.Fatalf("probe type: %v", r)
	}
	if r := browserCLIJSON(t, bin, flags, "eval", "document.querySelector('#note').value"); r["success"] != true || r["data"] != "probe fixture value" {
		t.Fatalf("second probe process did not read the first process's tab: %v", r)
	}
	t.Run("solo", func(t *testing.T) {
		r := browserCLIJSON(t, bin, []string{"--mode", "solo", "--chrome-path", e2eChrome(t)}, "navigate", fixture.URL)
		if r["success"] != true || r["title"] != "Probe fixture" {
			t.Fatalf("solo navigate: %v", r)
		}
	})
}

func browserCLIJSON(t *testing.T, bin string, flags []string, args ...string) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	argv := append([]string{"browser", "--json"}, flags...)
	cmd := exec.CommandContext(ctx, bin, append(argv, args...)...)
	out, err := cmd.Output()
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			t.Fatalf("browser %v: %v, stdout %s, stderr %s", args, err, out, exit.Stderr)
		}
		t.Fatalf("browser %v: %v", args, err)
	}
	var data map[string]any
	if err := json.Unmarshal(out, &data); err != nil {
		t.Fatalf("browser %v: malformed JSON %q: %v", args, out, err)
	}
	return data
}
