package browsercmd

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
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

// browserSetup handles `bashy browser setup live` — extract the embedded
// extension so the user can Load unpacked it in Chrome.
func browserSetup(rc *tool.RunContext, args []string, asJSON bool) int {
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
		fmt.Fprintf(rc.Out, "extension extracted to: %s\n", abs)
		fmt.Fprintln(rc.Out, "load it: chrome://extensions → Developer mode → Load unpacked → pick that folder,")
		fmt.Fprintln(rc.Out, "then start the hub: bashy browser hub")
		return 0
	case "probe":
		fmt.Fprintln(rc.Out, "probe needs no setup — start Chrome with --remote-debugging-port=9222.")
		return 0
	case "solo":
		fmt.Fprintln(rc.Out, "solo needs no setup — host Chrome is auto-detected.")
		return 0
	default:
		fmt.Fprintf(rc.Err, "browser setup: unknown mode %q (live|probe|solo)\n", mode)
		return 2
	}
}
