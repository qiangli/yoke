package browsercmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	goruntime "runtime"
	"strings"
	"syscall"

	"github.com/qiangli/coreutils/tool"
	"github.com/qiangli/yoke/pkg/browser/live"
)

// browserHub runs the live-mode WebSocket hub and blocks until the
// process is signalled. The already-installed Chrome extension connects
// to ws://127.0.0.1:<port>/ws; subsequent `bashy browser --mode live
// <action>` calls (or POSTs to /dispatch) drive the connected tab.
func browserHub(rc *tool.RunContext, ctx context.Context, args []string, asJSON bool) int {
	fs := flag.NewFlagSet("hub", flag.ContinueOnError)
	fs.SetOutput(rc.Err)
	port := fs.Int("port", live.DefaultPort, "loopback port the extension connects to")
	if err := fs.Parse(args); err != nil {
		return 2
	}

	// The hub is the ONE subcommand that legitimately logs: it is a
	// long-running foreground process and its diagnostics are the
	// point. Every other invocation leaves the sink discarding, so a
	// one-shot `browser --json …` writes exactly one JSON document to
	// stdout and nothing anywhere else.
	live.SetLogger(live.NewWriterLogger(rc.Err))
	defer live.SetLogger(nil)

	svc := live.New(*port)
	if err := svc.EnsureReady(ctx); err != nil {
		fmt.Fprintf(rc.Err, "browser hub: %v\n", err)
		return 1
	}
	fmt.Fprintf(rc.Out, "live hub listening on 127.0.0.1:%d — connect the extension's popup on your target tab.\n", *port)
	fmt.Fprintf(rc.Out, "drive it with: bashy browser --mode live <navigate|eval|cookies-get|...>\n")

	sigCtx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	<-sigCtx.Done()
	_ = svc.Close()
	fmt.Fprintln(rc.Out, "live hub stopped.")
	return 0
}

// setupHooks are the desktop side effects of `setup live`, swappable so
// tests never launch Chrome or touch the clipboard.
type setupHooks struct {
	openExtensions func() error            // open chrome://extensions in Chrome
	reveal         func(dir string) error  // show the folder in Finder/Explorer
	copyText       func(text string) error // put the path on the clipboard
}

var hooks = setupHooks{openExtensions: openChromeExtensions, reveal: revealFolder, copyText: copyToClipboard}

// browserSetup handles `bashy browser setup live` — extract the embedded
// extension and walk the human through the one-time Load unpacked step:
// Chrome opens on chrome://extensions, the folder opens beside it for
// drag-and-drop, and its path is on the clipboard for the file picker.
// After that the extension reconnects and upgrades itself (the hub
// refreshes the folder and asks it to reload). --no-open skips the
// desktop actions (headless hosts, scripts).
func browserSetup(rc *tool.RunContext, args []string, asJSON, noOpen bool) int {
	mode := "live"
	if len(args) > 0 {
		mode = args[0]
	}
	switch mode {
	case "live":
		dst := live.DefaultExtractDir()
		if len(args) > 1 {
			dst = args[1]
		}
		abs, err := live.ExtractExtension(dst)
		if err != nil {
			fmt.Fprintf(rc.Err, "browser setup live: %v\n", err)
			return 1
		}
		fmt.Fprintf(rc.Out, "extension v%s extracted to: %s\n", live.EmbeddedVersion(), abs)
		var did []string
		if !noOpen {
			if hooks.openExtensions() == nil {
				did = append(did, "opened chrome://extensions")
			}
			if hooks.reveal(abs) == nil {
				did = append(did, "opened the folder")
			}
			if hooks.copyText(abs) == nil {
				did = append(did, "copied its path")
			}
		}
		if len(did) > 0 {
			fmt.Fprintf(rc.Out, "(%s)\n", strings.Join(did, ", "))
		}
		fmt.Fprintln(rc.Out, "one-time, in Chrome:")
		fmt.Fprintln(rc.Out, "  1. chrome://extensions → turn on Developer mode (top right)")
		fmt.Fprintln(rc.Out, "  2. drag the folder onto that page — or Load unpacked and paste the path")
		fmt.Fprintln(rc.Out, "  3. on the tab to drive, click the extension icon → Connect this tab")
		fmt.Fprintln(rc.Out, "then: bashy browser hub. Already installed? Nothing to do — the extension")
		fmt.Fprintln(rc.Out, "reconnects by itself and the hub reloads it onto these files.")
		return 0
	case "probe":
		fmt.Fprintln(rc.Out, "probe needs no setup — the first action starts a persistent headless browser; `browser --mode probe stop` ends it.")
		return 0
	case "solo":
		fmt.Fprintln(rc.Out, "solo needs no setup — Chrome, Chromium, or Edge is auto-detected; otherwise a pinned headless shell is provisioned.")
		return 0
	default:
		fmt.Fprintf(rc.Err, "browser setup: unknown mode %q (live|probe|solo)\n", mode)
		return 2
	}
}

func openChromeExtensions() error {
	const url = "chrome://extensions"
	switch goruntime.GOOS {
	case "darwin":
		return exec.Command("open", "-a", "Google Chrome", url).Run()
	case "windows":
		return exec.Command("cmd", "/c", "start", "", "chrome", url).Run()
	default:
		for _, c := range []string{"google-chrome", "google-chrome-stable", "chromium", "chromium-browser"} {
			if p, err := exec.LookPath(c); err == nil {
				return exec.Command(p, url).Start()
			}
		}
		return fmt.Errorf("no Chrome found")
	}
}

func revealFolder(dir string) error {
	switch goruntime.GOOS {
	case "darwin":
		return exec.Command("open", dir).Run()
	case "windows":
		_ = exec.Command("explorer", dir).Run() // explorer exits 1 on success
		return nil
	default:
		return exec.Command("xdg-open", dir).Start()
	}
}

func copyToClipboard(text string) error {
	var cmd *exec.Cmd
	switch goruntime.GOOS {
	case "darwin":
		cmd = exec.Command("pbcopy")
	case "windows":
		cmd = exec.Command("clip")
	default:
		if _, err := exec.LookPath("wl-copy"); err == nil {
			cmd = exec.Command("wl-copy")
		} else {
			cmd = exec.Command("xclip", "-selection", "clipboard")
		}
	}
	cmd.Stdin = strings.NewReader(text)
	return cmd.Run()
}
