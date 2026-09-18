// Package autoretry recognizes transient command failures and provides the
// policy for re-running them, so an agent doesn't burn a round trip re-issuing a
// call that failed on a passing network/resource blip — and doesn't retry
// something the shell already retried.
//
// This package is the shared, pure decision layer (classify + eligibility +
// backoff + the report note); the actual re-run loop lives in the consumer that
// holds the command's result (ycode's bash.Execute), because a command's stderr
// — needed to tell a transient failure from a logic error — is not reachable
// from an exec-handler middleware in another package.
//
// Safety: only READ-ONLY / IDEMPOTENT, network-transient-prone commands are
// eligible (re-running reaches the same end state); a transient stderr class is
// required (never retry a logic/auth error); attempts are capped; and the note
// is transparent. Gated by pkg/nudge.Enabled (agent mode / BASHY_HINTS).
package autoretry

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/qiangli/coreutils/pkg/nudge"
	"github.com/qiangli/coreutils/pkg/weavecli"
)

// MaxAttempts is the total number of tries (1 original + up to MaxAttempts-1
// retries).
const MaxAttempts = 3

// eligible is the conservative allowlist of commands safe to auto-retry:
// network/transient-prone AND read-only or idempotent, so re-running reaches the
// same end state. Package managers with non-idempotent partial installs
// (pip/npm/apt) are deliberately excluded.
var eligible = map[string]bool{
	"curl": true, "wget": true, "git": true, "go": true,
	"dig": true, "nslookup": true, "host": true, "drill": true,
	"ping": true, "nc": true, "ncat": true, "ssh": true,
	"rsync": true, "scp": true, "sftp": true,
}

// Eligible reports whether argv0 is safe to auto-retry.
func Eligible(argv0 string) bool { return eligible[argv0] }

// Enabled mirrors the shared hint gate.
func Enabled() bool { return nudge.Enabled() }

var transientPatterns = []struct{ pat, class string }{
	{"connection refused", "connection-refused"},
	{"connection reset", "connection-reset"},
	{"failed to connect", "connect-failed"}, // curl (Linux)
	{"couldn't connect", "connect-failed"},  // curl (macOS)
	{"could not connect", "connect-failed"},
	{"connection closed", "connection-closed"},
	{"connection timed out", "timeout"},
	{"operation timed out", "timeout"},
	{"i/o timeout", "timeout"},
	{"tls handshake timeout", "timeout"},
	{"timed out", "timeout"},
	{"temporary failure in name resolution", "dns-temp"},
	{"could not resolve host", "dns-temp"},
	{"resource temporarily unavailable", "eagain"},
	{"temporarily unavailable", "eagain"},
	{"try again", "try-again"},
	{"network is unreachable", "network-unreachable"},
	{"no route to host", "no-route"},
	{"503 service unavailable", "http-503"},
	{"502 bad gateway", "http-502"},
	{"429 too many requests", "http-429"},
	{"the remote end hung up", "remote-hangup"},
	{"early eof", "early-eof"},
	{"unexpected eof", "unexpected-eof"},
}

// Classify returns a short class name and whether stderr indicates a transient
// (retryable) failure. Case-insensitive substring match; empty class when not
// transient.
func Classify(stderr string) (string, bool) {
	low := strings.ToLower(stderr)
	for _, p := range transientPatterns {
		if strings.Contains(low, p.pat) {
			return p.class, true
		}
	}
	return "", false
}

// Backoff returns the delay before the nth retry (n is 1-based: the wait after
// the nth failure), capped at 3s. 300ms, 900ms, 2.7s, …
func Backoff(n int) time.Duration {
	d := 300 * time.Millisecond
	for i := 1; i < n; i++ {
		d *= 3
	}
	if d > 3*time.Second {
		d = 3 * time.Second
	}
	return d
}

type noteLine struct {
	Schema   string `json:"schema_version"`
	Kind     string `json:"kind"` // "autoretry"
	Class    string `json:"class"`
	Attempts int    `json:"attempts"`
	Outcome  string `json:"outcome"` // "recovered" | "still-failing"
	Note     string `json:"note"`
	Off      string `json:"off"`
}

// Note builds the report appended to the command's stderr after retrying:
// either it recovered, or it exhausted attempts — always saying what was tried
// so the agent doesn't retry the same call itself.
func Note(class string, attempts int, recovered bool) string {
	outcome, human := "still-failing", "still failing"
	if recovered {
		outcome, human = "recovered", "recovered"
	}
	note := "retried " + itoa(attempts-1) + "x (transient: " + class + "); " + human +
		" — do not re-run; the shell already retried."
	if weavecli.IsAgentDriven() {
		b, _ := json.Marshal(noteLine{
			Schema: nudge.SchemaVersion, Kind: "autoretry", Class: class,
			Attempts: attempts, Outcome: outcome, Note: note, Off: "BASHY_HINTS=off",
		})
		return "\n" + string(b) + "\n"
	}
	return "\n─── bashy autoretry ─── " + note + " (silence: BASHY_HINTS=off)\n"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
