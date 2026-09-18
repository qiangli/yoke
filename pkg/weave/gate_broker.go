package weave

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/qiangli/coreutils/tool"
	_ "github.com/qiangli/yoke/cmds/browser"
	"github.com/qiangli/yoke/pkg/agentpty"
)

// The gate classifier and router moved to pkg/agentpty — an agent that stalls at
// a trust prompt is a problem for anything that launches one, not just for a
// weave worker.
//
// What stays here is the half that is weave's opinion, and could not be anything
// else: WHERE a gate escalates to (a comment on the run in the queue), and HOW a
// browser gets opened (the browser tool). Injecting those is what keeps
// cmds/browser and the work queue out of the import graph of a package whose job
// is to run a subprocess.

// Aliases so weave's call sites and tests keep speaking weave's language.
type (
	GateKind       = agentpty.GateKind
	GateVerdict    = agentpty.GateVerdict
	routeDeps      = agentpty.RouteDeps
	gateRouteState = agentpty.GateRouteState
	gateBroker     = agentpty.GateBroker
)

const (
	GateNone         = agentpty.GateNone
	GateTrust        = agentpty.GateTrust
	GateBrowserOAuth = agentpty.GateBrowserOAuth
	GateDeviceCode   = agentpty.GateDeviceCode
	GateAPIKey       = agentpty.GateAPIKey
	GateHuman        = agentpty.GateHuman

	gateBrokerDebounce = agentpty.DefaultGateDebounce
)

func classifyGate(tail string) GateVerdict { return agentpty.ClassifyGate(tail) }

func routeGate(verdict GateVerdict, deps routeDeps) (string, error) {
	return agentpty.RouteGate(verdict, deps)
}

func newGateBroker(deps routeDeps, debounce time.Duration) *gateBroker {
	return agentpty.NewGateBroker(deps, debounce)
}

func gateBrokerSay(ctlSock, payload string) error { return agentpty.BrokerSay(ctlSock, payload) }

// newLiveGateRouteDeps wires a run's gates to weave's ways out: keystrokes over
// its control socket, the browser tool, and a blocker comment on the run itself.
func newLiveGateRouteDeps(cmdErr io.Writer, dir string, issueID int64, ctlSock string) routeDeps {
	return routeDeps{
		State: &gateRouteState{},
		Say: func(payload string) error {
			return agentpty.BrokerSay(ctlSock, payload)
		},
		BrowserLogin: func(rawURL string) error {
			return gateBrokerBrowserLogin(context.Background(), rawURL)
		},
		Escalate: func(msg string) error {
			if cmdErr != nil {
				fmt.Fprintf(cmdErr, "weave wait --broker: run #%d blocked: %s\n", issueID, msg)
			}
			return gateBrokerEscalate(dir, issueID, msg)
		},
	}
}

func runGateBrokerForItem(brokers map[int64]*gateBroker, errw io.Writer, dir string, it *weaveItem, now time.Time) {
	if it == nil || it.State != "working" || it.LogPath == "" {
		return
	}
	b := brokers[it.ID]
	if b == nil {
		b = newGateBroker(newLiveGateRouteDeps(errw, dir, it.ID, it.CtlSock), gateBrokerDebounce)
		brokers[it.ID] = b
	}
	tail := weaveReadThrottleLogTail(it.LogPath)
	verdict, action, err := b.ObserveTail(tail, now)
	if verdict.Kind == GateNone || action == "none" || action == "debounce" || action == "dedupe" {
		return
	}
	if err != nil && errw != nil {
		fmt.Fprintf(errw, "weave wait --broker: run #%d route %s failed: %v\n", it.ID, action, err)
	}
}

func gateBrokerBrowserLogin(ctx context.Context, rawURL string) error {
	t := tool.Lookup("browser")
	if t == nil {
		return fmt.Errorf("browser tool is not registered")
	}
	successURL := inferBrowserLoginSuccessURL(rawURL)
	args := []string{"login", "--success-url", successURL, rawURL}
	var out, errb bytes.Buffer
	code := t.Run(&tool.RunContext{
		Ctx:   ctx,
		Dir:   ".",
		Env:   os.Environ(),
		FS:    tool.NewLocalFS(),
		Stdio: tool.Stdio{Out: &out, Err: &errb},
	}, args)
	if code != 0 {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = strings.TrimSpace(out.String())
		}
		if msg == "" {
			msg = fmt.Sprintf("exit code %d", code)
		}
		return fmt.Errorf("%s", msg)
	}
	return nil
}

func inferBrowserLoginSuccessURL(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "callback"
	}
	if redirect := u.Query().Get("redirect_uri"); redirect != "" {
		if ru, err := url.Parse(redirect); err == nil {
			if ru.Scheme != "" && ru.Host != "" {
				return ru.Scheme + "://" + ru.Host + ru.Path
			}
			return redirect
		}
	}
	return "callback"
}

func gateBrokerEscalate(dir string, issueID int64, msg string) error {
	return withWeaveQueueLock(dir, func(q *weaveQueue) error {
		it := findWeaveItem(q, issueID)
		if it == nil {
			return fmt.Errorf("run #%d not found", issueID)
		}
		weaveAppendComment(it, "broker", "blocker", msg)
		return nil
	})
}
