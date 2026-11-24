package browsercmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/browser"
	"github.com/chromedp/cdproto/cdp"
	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/page"
	"github.com/chromedp/cdproto/runtime"
	"github.com/chromedp/chromedp"
	"github.com/qiangli/yoke/pkg/browser/live"
)

// TestCopySheetSecretRevealAndCopyInChrome is the S529.7 end-to-end check: a
// real hub serves a rendered sheet, headless Chrome clicks Reveal and Copy,
// and the value reaches the page and the clipboard but not the sheet file.
func TestCopySheetSecretRevealAndCopyInChrome(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") != "1" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a headless Chrome)")
	}
	t.Setenv("BASHY_HOME", t.TempDir())
	const value = "sk-e2e-0123456789"
	if err := live.PutSheetSecret("e2e-key", value); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	var hubLog bytes.Buffer
	live.SetLogger(live.NewWriterLogger(&hubLog))
	t.Cleanup(func() { live.SetLogger(nil) })
	svc := live.New(port)
	if err := svc.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	s := &copySheet{Title: "E2E", Sections: []copySection{{Fields: []copyField{{Label: "API key", Secret: "e2e-key"}}}}}
	if err := os.MkdirAll(live.SheetDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(live.SheetDir(), fmt.Sprintf("e2e-secret-%d.html", time.Now().UnixNano()))
	if err := os.WriteFile(path, []byte(renderCopySheet(s, nil, false)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	if b, _ := os.ReadFile(path); bytes.Contains(b, []byte(value)) {
		t.Fatal("the value is in the sheet file")
	}

	body, _ := json.Marshal(map[string]string{"path": path})
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/sheet", port), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct{ URL string }
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	if reg.URL == "" {
		t.Fatalf("register: %s", resp.Status)
	}

	ctx, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Flag("headless", "new"))...)
	defer cancel()
	ctx, cancel = chromedp.NewContext(ctx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	var shown, copied, button, note string
	focusCopySheet(t, ctx, reg.URL, origin)
	runCopySheetStep(t, ctx, "reveal the fixture value",
		chromedp.Evaluate(`document.querySelector('[data-secret] button[data-act=reveal]').click()`, nil),
		chromedp.Poll(`(() => {
			const text = document.querySelector('[data-secret] pre').textContent;
			return text !== '••••••••' && text;
		})()`, &shown, chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(10*time.Second)),
	)
	runCopySheetStep(t, ctx, "copy the fixture value",
		chromedp.Evaluate(`document.querySelector('[data-secret] button[data-act=copy]').click()`, nil),
		chromedp.Poll(`(() => {
			const button = document.querySelector('[data-secret] button[data-act=copy]').textContent;
			return (button === 'Copied' || document.querySelector('.sx').style.color !== '') && button;
		})()`, &button, chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`document.querySelector('.sx').textContent`, &note),
	)
	runCopySheetStep(t, ctx, "read the fixture clipboard",
		chromedp.Evaluate(`navigator.clipboard.readText()`, &copied, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }),
	)
	if shown != value {
		t.Errorf("reveal showed %q", shown)
	}
	if button != "Copied" || copied != value {
		t.Errorf("copy: button %q, clipboard %q, note %q", button, copied, note)
	}
	if strings.Contains(note, "refused") {
		t.Errorf("note: %q", note)
	}
	if strings.Contains(hubLog.String(), value) || !strings.Contains(hubLog.String(), "sheet secret served") {
		t.Errorf("hub log leaks the value or misses the event:\n%s", hubLog.String())
	}
}

// TestCopySheetEntryRememberInChrome is the S529.8 end-to-end check: a named
// secret with no value shows an entry box; the human types it, ticks
// Remember, saves and copies; the store keeps it and the log does not.
func TestCopySheetEntryRememberInChrome(t *testing.T) {
	if os.Getenv("BASHY_BROWSER_LIVE") != "1" {
		t.Skip("live Chrome test disabled; set BASHY_BROWSER_LIVE=1 to run (launches a headless Chrome)")
	}
	t.Setenv("BASHY_HOME", t.TempDir())
	const value = "typed-by-human-42"
	var hubLog bytes.Buffer
	live.SetLogger(live.NewWriterLogger(&hubLog))
	t.Cleanup(func() { live.SetLogger(nil) })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	svc := live.New(port)
	if err := svc.EnsureReady(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })

	s := &copySheet{Title: "E2E entry", Sections: []copySection{{Fields: []copyField{{Label: "Portal password", Secret: "e2e-new"}}}}}
	_ = os.MkdirAll(live.SheetDir(), 0o755)
	path := filepath.Join(live.SheetDir(), fmt.Sprintf("e2e-entry-%d.html", time.Now().UnixNano()))
	if err := os.WriteFile(path, []byte(renderCopySheet(s, nil, false)), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Remove(path) })
	body, _ := json.Marshal(map[string]string{"path": path})
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/sheet", port), "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var reg struct{ URL string }
	_ = json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()

	ctx, cancel := chromedp.NewExecAllocator(context.Background(), append(chromedp.DefaultExecAllocatorOptions[:], chromedp.Flag("headless", "new"))...)
	defer cancel()
	ctx, cancel = chromedp.NewContext(ctx)
	defer cancel()
	ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	origin := fmt.Sprintf("http://127.0.0.1:%d", port)
	var note, copied string
	focusCopySheet(t, ctx, reg.URL, origin)
	runCopySheetStep(t, ctx, "enter and remember the fixture value",
		chromedp.Poll(`!document.querySelector('[data-secret] .entry').hidden`, nil,
			chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(fmt.Sprintf(`document.querySelector('[data-secret] .sv').value = %q;
			document.querySelector('[data-secret] .remember').click();
			document.querySelector('[data-secret] button[data-act=save]').click()`, value), nil),
		chromedp.Poll(`(() => {
			const note = document.querySelector('.sx').textContent;
			return note.startsWith('Saved') && note;
		})()`, &note, chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(10*time.Second)),
	)
	runCopySheetStep(t, ctx, "copy the remembered fixture value",
		chromedp.Evaluate(`document.querySelector('[data-secret] button[data-act=copy]').click()`, nil),
		chromedp.Poll(`document.querySelector('[data-secret] button[data-act=copy]').textContent === 'Copied'`, nil,
			chromedp.WithPollingInterval(50*time.Millisecond), chromedp.WithPollingTimeout(10*time.Second)),
		chromedp.Evaluate(`navigator.clipboard.readText()`, &copied, func(p *runtime.EvaluateParams) *runtime.EvaluateParams { return p.WithAwaitPromise(true) }),
	)
	if strings.Contains(note, value) || copied != value {
		t.Errorf("note %q, clipboard %q", note, copied)
	}
	if v, err := live.GetSheetSecret("e2e-new"); err != nil || v != value {
		t.Errorf("not remembered: %q %v", v, err)
	}
	if strings.Contains(hubLog.String(), value) || !strings.Contains(hubLog.String(), "sheet secret entered") {
		t.Errorf("hub log leaks the value or misses the event:\n%s", hubLog.String())
	}
}

func runCopySheetStep(t *testing.T, ctx context.Context, name string, actions ...chromedp.Action) {
	t.Helper()
	if err := chromedp.Run(ctx, actions...); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
}

func focusCopySheet(t *testing.T, ctx context.Context, url, origin string) {
	t.Helper()
	runCopySheetStep(t, ctx, "open and focus copy sheet",
		chromedp.Navigate(url),
		page.BringToFront(),
		emulation.SetFocusEmulationEnabled(true),
		chromedp.Poll("document.hasFocus()", nil, chromedp.WithPollingTimeout(5*time.Second)),
		chromedp.ActionFunc(func(ctx context.Context) error {
			browserCtx := cdp.WithExecutor(ctx, chromedp.FromContext(ctx).Browser)
			for _, permission := range []string{"clipboard-read", "clipboard-write"} {
				if err := browser.SetPermission(&browser.PermissionDescriptor{Name: permission},
					browser.PermissionSettingGranted).WithOrigin(origin).Do(browserCtx); err != nil {
					return err
				}
			}
			return nil
		}),
	)
}
