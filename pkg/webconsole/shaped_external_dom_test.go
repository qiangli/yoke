//go:build verifydom

package webconsole

import (
	"context"
	"encoding/json"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	goruntime "runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/cdproto/target"
	"github.com/chromedp/chromedp"
)

// TestDOMExternalShapeCheckpoint observes an operator-prepared console. It does
// not start a server or perform lifecycle mutations. No URL means no execution.
func TestDOMExternalShapeCheckpoint(t *testing.T) {
	origin := os.Getenv("BASHY_E2E_CONSOLE_URL")
	if origin == "" {
		t.Skip("external console checkpoint not requested")
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		t.Fatal("invalid external console URL")
	}
	ip := net.ParseIP(u.Hostname())
	if u.Scheme != "https" && !(u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())) {
		t.Fatal("console requires HTTPS or loopback HTTP")
	}
	cookieBytes, err := os.ReadFile(os.Getenv("BASHY_E2E_COOKIE_FILE"))
	if err != nil {
		t.Fatal("cannot read private session file")
	}
	cookie := strings.TrimSpace(string(cookieBytes))
	if cookie == "" || strings.ContainsAny(cookie, "\r\n;") {
		t.Fatal("session file must contain one cookie value")
	}
	app := os.Getenv("BASHY_E2E_APP")
	if app == "" {
		app = "hello-world"
	}
	if !regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString(app) {
		t.Fatal("invalid app name")
	}
	page := os.Getenv("BASHY_E2E_PAGE_PATH")
	p, err := url.Parse(page)
	if err != nil || p.IsAbs() || p.Host != "" || !strings.HasPrefix(page, "/") || p.RawQuery != "" || p.Fragment != "" || strings.Contains(page, "..") {
		t.Fatal("page path must be a console-relative path without query or fragment")
	}
	greeting := os.Getenv("BASHY_E2E_GREETING")
	revision := os.Getenv("BASHY_E2E_ACTIVE")
	if greeting == "" || !regexp.MustCompile(`^[a-fA-F0-9]{40}$`).MatchString(revision) {
		t.Fatal("expected greeting and full active revision required")
	}
	dir := os.Getenv("BASHY_E2E_ARTIFACT_DIR")
	if !filepath.IsAbs(dir) {
		t.Fatal("private artifact directory must be absolute")
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal("cannot create private artifact directory")
	}
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() || goruntime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		t.Fatal("artifact directory must be private and must not be a symlink")
	}
	// Each checkpoint owns fresh files; never overwrite another observation.
	for _, name := range []string{"page.png", "shape.png", "checkpoint.json"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatal("checkpoint artifact already exists or cannot be inspected")
		}
	}
	type observation struct {
		Greeting         string   `json:"greeting"`
		ExpectedGreeting string   `json:"expected_greeting"`
		ActiveUI         string   `json:"active_ui"`
		ExpectedRevision string   `json:"expected_revision"`
		Errors           []string `json:"browser_errors"`
		Failure          string   `json:"failure,omitempty"`
		Passed           bool     `json:"passed"`
	}
	result := observation{ExpectedGreeting: greeting, ExpectedRevision: revision}
	var mu sync.Mutex
	failCheckpoint := func(stage string, err error) {
		message := strings.ReplaceAll(stage+": "+err.Error(), cookie, "[session redacted]")
		mu.Lock()
		result.Failure = message
		mu.Unlock()
		t.Fatal(message)
	}
	defer func() {
		mu.Lock()
		defer mu.Unlock()
		result.Passed = !t.Failed()
		data, _ := json.MarshalIndent(result, "", "  ")
		if err := os.WriteFile(filepath.Join(dir, "checkpoint.json"), data, 0600); err != nil {
			t.Error("cannot write checkpoint observation")
		}
	}()
	var alloc context.Context
	var stop context.CancelFunc
	var browserOptions []chromedp.ContextOption
	remoteBrowser := false
	if endpoint := os.Getenv("BASHY_E2E_BROWSER_WS"); endpoint != "" {
		remote, err := url.Parse(endpoint)
		if err != nil || remote.Host == "" || remote.User != nil || remote.RawQuery != "" || remote.Fragment != "" || (remote.Scheme != "ws" && remote.Scheme != "wss") {
			t.Fatal("browser CDP endpoint must be a loopback ws/wss URL without credentials or query")
		}
		address := net.ParseIP(remote.Hostname())
		if remote.Hostname() != "localhost" && (address == nil || !address.IsLoopback()) {
			t.Fatal("browser CDP endpoint must use loopback")
		}
		if !strings.HasPrefix(remote.Path, "/devtools/browser/") {
			t.Fatal("browser CDP endpoint must identify the prepared browser")
		}
		alloc, stop = chromedp.NewRemoteAllocator(context.Background(), endpoint, chromedp.NoModifyURL)
		remoteBrowser = true
	} else {
		alloc, stop = chromedp.NewExecAllocator(context.Background(), browserOpts()...)
	}
	defer stop()
	if remoteBrowser {
		// Initialize an ordinary remote tab first. New headless Chrome can
		// reject an incognito target before its first browser window exists.
		root, closeRoot := chromedp.NewContext(alloc)
		defer closeRoot()
		ready, cancelReady := context.WithTimeout(root, 90*time.Second)
		defer cancelReady()
		if err := chromedp.Run(ready); err != nil {
			failCheckpoint("remote browser initialization failed", err)
		}
		executor := cdp.WithExecutor(ready, chromedp.FromContext(root).Browser)
		isolated, err := target.CreateBrowserContext().WithDisposeOnDetach(true).Do(executor)
		if err != nil {
			failCheckpoint("remote private browser context failed", err)
		}
		defer func() {
			if err := target.DisposeBrowserContext(isolated).Do(executor); err != nil {
				t.Error("remote private browser context cleanup failed")
			}
		}()
		// Explicit newWindow is needed by current headless Chrome for an
		// incognito target; chromedp's default target creation omits it.
		pageID, err := target.CreateTarget("about:blank").WithBrowserContextID(isolated).WithNewWindow(true).Do(executor)
		if err != nil {
			failCheckpoint("remote private browser tab failed", err)
		}
		browserOptions = []chromedp.ContextOption{chromedp.WithTargetID(pageID)}
		alloc = root
	}
	browser, closeBrowser := chromedp.NewContext(alloc, browserOptions...)
	defer closeBrowser()
	ctx, cancel := context.WithTimeout(browser, 90*time.Second)
	defer cancel()
	chromedp.ListenTarget(ctx, func(event any) {
		var message string
		switch e := event.(type) {
		case *runtime.EventExceptionThrown:
			message = "uncaught exception: " + e.ExceptionDetails.Error()
		case *runtime.EventConsoleAPICalled:
			if e.Type == "error" {
				for _, arg := range e.Args {
					message += string(arg.Value) + " "
				}
			}
		}
		if message != "" {
			mu.Lock()
			result.Errors = append(result.Errors, strings.ReplaceAll(message, cookie, "[session redacted]"))
			mu.Unlock()
		}
	})
	base := strings.TrimRight(u.String(), "/")
	saveScreenshot := func(name string) chromedp.Action {
		return chromedp.ActionFunc(func(ctx context.Context) error {
			var image []byte
			if err := chromedp.FullScreenshot(&image, 90).Do(ctx); err != nil {
				return err
			}
			return os.WriteFile(filepath.Join(dir, name), image, 0600)
		})
	}
	if err := chromedp.Run(ctx,
		network.Enable(),
		chromedp.ActionFunc(func(ctx context.Context) error {
			return network.SetCookie("bashy_console", cookie).WithURL(base + "/").WithHTTPOnly(true).WithSecure(u.Scheme == "https").Do(ctx)
		}),
		chromedp.Navigate(base+page),
		chromedp.WaitVisible("#greeting", chromedp.ByQuery),
		chromedp.Text("#greeting", &result.Greeting, chromedp.ByQuery),
		saveScreenshot("page.png"),
	); err != nil {
		failCheckpoint("external greeting checkpoint failed", err)
	}
	if result.Greeting != greeting {
		t.Error("rendered greeting differs from expected")
	}
	quoted, _ := json.Marshal(revision[:10])
	if err := chromedp.Run(ctx,
		chromedp.Navigate(base+"/shape/"+app+"/"),
		chromedp.WaitVisible("#shape-active", chromedp.ByQuery),
		chromedp.Poll(`document.getElementById("shape-active").textContent.includes(`+string(quoted)+`)`, nil, chromedp.WithPollingTimeout(15*time.Second)),
		chromedp.Text("#shape-active", &result.ActiveUI, chromedp.ByQuery),
		saveScreenshot("shape.png"),
	); err != nil {
		failCheckpoint("external active-version UI checkpoint failed", err)
	}
	mu.Lock()
	errors := len(result.Errors)
	mu.Unlock()
	if errors != 0 {
		t.Error("browser reported errors; inspect private checkpoint observation")
	}
}
