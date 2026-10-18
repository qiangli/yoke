package saasclients

import (
	"fmt"
	"strings"

	"github.com/qiangli/yoke/external/gws"
)

// Mode is an execution mode. Admission attaches to an exact operation and
// mode, not a product name.
type Mode string

const (
	ModeShortLived      Mode = "short-lived"      // one bounded invocation, no listener or resident child
	ModeWebhookForward  Mode = "webhook-forward"  // forwards provider events to a local/remote URL (resident)
	ModeWebhookServe    Mode = "webhook-serve"    // listens for provider callbacks (resident)
	ModeResident        Mode = "resident"         // tail/stream/socket/dev-server (resident)
	ModeInteractiveAuth Mode = "interactive-auth" // browser/device login that opens a local callback listener
)

// ClassifyArgs classifies a client invocation by its argv. Unknown shapes are
// short-lived; the resident/webhook shapes below are the ones the reviewed
// CLIs document. The match is deliberately on the command words only.
func ClassifyArgs(client string, argv []string) Mode {
	words := commandWords(argv)
	has := func(prefix ...string) bool {
		if len(words) < len(prefix) {
			return false
		}
		for i, p := range prefix {
			if words[i] != p {
				return false
			}
		}
		return true
	}
	switch client {
	case "stripe":
		switch {
		case has("listen"):
			return ModeWebhookForward
		case has("login"):
			return ModeInteractiveAuth
		case has("logs", "tail"), has("samples", "create"):
			return ModeResident
		}
	case "twilio":
		switch {
		case has("serverless:start"), has("dev-phone"), has("login"):
			return ModeResident
		case has("phone-numbers:update") && containsAny(argv, "--sms-url", "--voice-url"):
			// repoints a live number's webhook: a mutation of delivery routing, not a read
			return ModeWebhookServe
		}
	case "slack":
		switch {
		case has("run"), has("platform", "run"), has("trigger", "create") && containsAny(argv, "--webhook"):
			return ModeResident
		case has("login"):
			return ModeInteractiveAuth
		}
	case "sf":
		switch {
		case has("apex", "tail", "log"), has("data", "tree", "stream"), has("force", "apex", "log", "tail"):
			return ModeResident
		case has("org", "login", "web"), has("org", "login", "device"):
			return ModeInteractiveAuth
		}
	}
	return ModeShortLived
}

// commandWords returns argv up to the first flag, dropping the flags' values.
func commandWords(argv []string) []string {
	var out []string
	for _, a := range argv {
		if strings.HasPrefix(a, "-") {
			break
		}
		out = append(out, a)
	}
	return out
}

func containsAny(argv []string, flags ...string) bool {
	for _, a := range argv {
		name, _, _ := strings.Cut(a, "=")
		for _, f := range flags {
			if name == f {
				return true
			}
		}
	}
	return false
}

// AdmitMode is the policy gate: only ModeShortLived is admitted. Every other
// mode needs its own policy admission (service Rule 1 for listeners and
// resident children — a timeout does not turn a server into a client) and is
// refused here without execution.
func AdmitMode(client string, m Mode) *gws.Error {
	if m == ModeShortLived {
		return nil
	}
	return gws.NewError(gws.CodeUnsupported,
		fmt.Sprintf("%s: mode %q is not admitted — only short-lived client invocations are; webhook forwarding/serving, tails, sockets and callback-listener logins need separate policy admission (supply-chain Rule 1)", client, m), nil).
		WithOp("admit-mode", client+":"+string(m))
}

// AdmitArgs classifies and gates an argv in one step.
func AdmitArgs(client string, argv []string) (Mode, *gws.Error) {
	m := ClassifyArgs(client, argv)
	return m, AdmitMode(client, m)
}
