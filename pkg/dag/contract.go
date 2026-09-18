// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

package dag

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"mvdan.cc/sh/v3/expand"
	"mvdan.cc/sh/v3/interp"
	"mvdan.cc/sh/v3/syntax"

	"github.com/qiangli/coreutils/pkg/weavecli"
	"github.com/qiangli/coreutils/shell"
)

// Effect is one declared capability a target may use. The vocabulary mirrors the
// dhnt contract model (read/write/net/spend/destroy/time). dag P2 *declares* and
// *attests* effects; hard enforcement (proving a body stayed within its cap) is
// the job of the sandbox execution layer (podman), not the runner.
var knownEffects = map[string]bool{
	"read": true, "write": true, "net": true,
	"spend": true, "destroy": true, "time": true,
}

// CheckResult is the outcome of one Ensure postcondition.
type CheckResult struct {
	Expr   string `json:"expr"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail,omitempty"`
}

// Attestation is the sealed verdict for a target: did its body exit cleanly AND
// do all its Ensure postconditions hold, with its declared Effects recorded.
// Valid is the agent-trustable bit — success judged by world-state, not just the
// exit code (which is what make stops at).
type Attestation struct {
	Target  string        `json:"target"`
	Valid   bool          `json:"valid"`
	Checks  []CheckResult `json:"checks,omitempty"`
	Effects []string      `json:"effects,omitempty"`
	// Clause is "require" when a precondition sealed this verdict before the
	// body ran; empty for the postcondition verdict (the shape shipped today).
	Clause string `json:"clause,omitempty"`
}

// requireHolds evaluates a target's Require preconditions BEFORE its body runs.
// The first failing check seals an invalid Attestation naming it and the body
// is never run — that is the whole difference from Ensure, which judges the
// world after the body. nil means every precondition held (or none declared).
func requireHolds(ctx context.Context, t *Task, dir string, env []string) *Attestation {
	for _, expr := range t.Require {
		cr := evalCheck(ctx, dir, env, expr)
		if !cr.Pass {
			return &Attestation{Target: t.Name, Valid: false, Checks: []CheckResult{cr}, Effects: t.Effects, Clause: "require"}
		}
	}
	return nil
}

// attest evaluates a target's Ensure postconditions after its body ran and seals
// an Attestation. bodyOK reports whether the body itself exited 0.
func attest(ctx context.Context, t *Task, dir string, env []string, bodyOK bool) *Attestation {
	if len(t.Ensure) == 0 && len(t.Effects) == 0 {
		return nil
	}
	a := &Attestation{Target: t.Name, Effects: t.Effects}
	allPass := bodyOK
	for _, expr := range t.Ensure {
		cr := evalCheck(ctx, dir, env, expr)
		a.Checks = append(a.Checks, cr)
		if !cr.Pass {
			allPass = false
		}
	}
	a.Valid = allPass
	return a
}

// firstFailedCheck returns an error naming the first failed check and its
// clause ("precondition" / "postcondition"), for the result's Err / the
// human-mode message.
func firstFailedCheck(a *Attestation, clause string) error {
	for _, c := range a.Checks {
		if !c.Pass {
			detail := c.Detail
			if detail != "" {
				detail = " (" + detail + ")"
			}
			return errf(weavecli.ExitPrecondFail, "%s failed: %s%s", clause, c.Expr, detail)
		}
	}
	return errf(weavecli.ExitPrecondFail, "contract not satisfied")
}

// evalCheck evaluates one Require/Ensure expression. Sugar forms:
//
//	file-exists <path> | file-exists path=<path>
//	file-absent <path> | file-absent path=<path>
//	http-ok <url>      | http-ok url=<url>
//	cmd <shell...>     — explicit shell command, exit 0 = pass
//
// Anything else is treated as a shell command (so `Ensure: test -f dist/out`
// just works), run through the in-process shell + coreutils userland.
func evalCheck(ctx context.Context, dir string, env []string, expr string) CheckResult {
	fields := strings.Fields(expr)
	if len(fields) == 0 {
		return CheckResult{Expr: expr, Pass: false, Detail: "empty check"}
	}
	switch fields[0] {
	case "file-exists", "file-absent":
		arg := argValue(strings.TrimSpace(strings.TrimPrefix(expr, fields[0])), "path")
		if arg == "" {
			return CheckResult{Expr: expr, Pass: false, Detail: "missing path"}
		}
		_, err := os.Stat(filepath.Join(dir, arg))
		exists := err == nil
		want := fields[0] == "file-exists"
		return CheckResult{Expr: expr, Pass: exists == want,
			Detail: detailIf(exists != want, ternary(want, "does not exist", "still exists"))}
	case "http-ok":
		url := argValue(strings.TrimSpace(strings.TrimPrefix(expr, "http-ok")), "url")
		return httpOK(ctx, expr, url)
	case "cmd":
		return shellCheck(ctx, dir, env, expr, strings.TrimSpace(strings.TrimPrefix(expr, "cmd")))
	default:
		return shellCheck(ctx, dir, env, expr, expr)
	}
}

// argValue accepts either `key=value` or a bare value.
func argValue(s, key string) string {
	s = strings.TrimSpace(s)
	if v, ok := strings.CutPrefix(s, key+"="); ok {
		return strings.TrimSpace(v)
	}
	return s
}

func httpOK(ctx context.Context, expr, url string) CheckResult {
	if url == "" {
		return CheckResult{Expr: expr, Pass: false, Detail: "missing url"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return CheckResult{Expr: expr, Pass: false, Detail: err.Error()}
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return CheckResult{Expr: expr, Pass: false, Detail: err.Error()}
	}
	defer resp.Body.Close()
	ok := resp.StatusCode >= 200 && resp.StatusCode < 300
	return CheckResult{Expr: expr, Pass: ok, Detail: detailIf(!ok, resp.Status)}
}

// shellCheck runs script through the in-process shell; exit 0 = pass.
func shellCheck(ctx context.Context, dir string, env []string, expr, script string) CheckResult {
	prog, err := syntax.NewParser().Parse(strings.NewReader(script), "ensure")
	if err != nil {
		return CheckResult{Expr: expr, Pass: false, Detail: err.Error()}
	}
	runner, err := interp.New(
		interp.Dir(dir),
		interp.Env(expand.ListEnviron(env...)),
		interp.StdIO(nil, io.Discard, io.Discard),
		interp.ExecHandlers(shell.Handler()),
	)
	if err != nil {
		return CheckResult{Expr: expr, Pass: false, Detail: err.Error()}
	}
	code, _ := exitCodeFromErr(runner.Run(ctx, prog))
	return CheckResult{Expr: expr, Pass: code == 0, Detail: detailIf(code != 0, "exit "+itoa(code))}
}

func detailIf(cond bool, s string) string {
	if cond {
		return s
	}
	return ""
}

func ternary(b bool, t, f string) string {
	if b {
		return t
	}
	return f
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
