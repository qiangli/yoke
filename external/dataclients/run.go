package dataclients

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/qiangli/yoke/external/gws"
	"github.com/qiangli/yoke/external/notrack"
	"github.com/qiangli/yoke/pkg/binmgr"
)

// capBuf keeps at most maxStderr bytes and discards the rest.
type capBuf struct{ bytes.Buffer }

func (b *capBuf) Write(p []byte) (int, error) {
	if room := maxStderr - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}

// childEnv scrubs ambient client variables from base, then applies the
// invocation's explicit additions and the no-tracking opt-outs.
func childEnv(client string, base, add []string) []string {
	var out []string
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, p := range ambientPrefixes[client] {
			if strings.HasPrefix(strings.ToUpper(k), p) {
				drop = true
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return notrack.Env(append(out, add...))
}

// Run executes the invocation under its deadline and the caller's context.
// Cancellation or the deadline kills the child; the output read stops at the
// byte bound and kills it too. Every error message is redacted against the
// credential before it leaves this function.
func Run(ctx context.Context, inv Invocation) (*Result, *gws.Error) {
	op, _ := inv.Op.normalized2()
	if inv.Op.Timeout <= 0 {
		inv.Op.Timeout = DefaultTimeout
	}
	rctx, cancel := context.WithTimeout(ctx, inv.Op.Timeout)
	defer cancel()
	cmd := binmgr.Command(rctx, inv.Bin, inv.Args...)
	cmd.Env = childEnv(inv.Client, os.Environ(), inv.Env)
	cmd.Stdin = strings.NewReader(inv.Stdin)
	var stderr capBuf
	cmd.Stderr = &stderr
	cmd.WaitDelay = 2 * time.Second
	out, perr := cmd.StdoutPipe()
	if perr != nil {
		return nil, gws.NewError(gws.CodeProviderFailure, inv.Client+": cannot attach output", nil)
	}
	if err := cmd.Start(); err != nil {
		return nil, gws.NewError(gws.CodeUnsupported, fmt.Sprintf("%s: cannot start client: %s", inv.Client, gws.RedactText(err.Error(), inv.Secret)), nil).WithOp("query", inv.Client)
	}
	res, perr2 := Parse(inv.Client, out, op, inv.NullToken)
	if res != nil && res.Partial && res.Reason == "byte-limit" {
		cancel() // stop the producer; the limit is reported, not hidden
	}
	waitErr := cmd.Wait()

	switch {
	case errors.Is(rctx.Err(), context.DeadlineExceeded) && ctx.Err() == nil:
		return nil, inv.failure(gws.CodeTimeout, "deadline exceeded", "")
	case ctx.Err() != nil:
		return nil, inv.failure(gws.CodeCancelled, "cancelled", "")
	}
	if res != nil && res.Partial && res.Reason == "byte-limit" {
		inv.stamp(res)
		return res, nil
	}
	if waitErr != nil {
		code, why := classify(stderr.String())
		return nil, inv.failure(code, why, stderr.String())
	}
	if perr2 != nil {
		return nil, perr2
	}
	inv.stamp(res)
	return res, nil
}

func (inv Invocation) stamp(res *Result) {
	switch {
	case !inv.Op.Write:
		res.Mutation = "none"
	case inv.Op.Commit:
		res.Mutation = "committed"
	default:
		res.Mutation = "rolled-back"
	}
}

// failure builds the redacted typed error. After a started commit-mode
// write, outcomes other than a definite pre-mutation refusal are reported as
// partial/unknown: cancelling the local request cannot guarantee remote state.
func (inv Invocation) failure(code gws.Code, why, stderr string) *gws.Error {
	msg := fmt.Sprintf("%s: %s", inv.Client, why)
	if s := strings.TrimSpace(stderr); s != "" {
		if len(s) > 512 {
			s = s[:512] + "…"
		}
		msg += ": " + gws.RedactText(s, inv.Secret)
	}
	definite := code == gws.CodeMissingCredentials || code == gws.CodeExpiredCredentials || code == gws.CodePermissionDenied || code == gws.CodeNotFound
	switch {
	case inv.Op.Write && inv.Op.Commit && !definite:
		msg += " — commit outcome UNKNOWN; verify remote state before retrying"
		code = gws.CodePartialMutation
	case inv.Op.Write && !inv.Op.Commit:
		msg += " — uncommitted transaction; nothing committed (the server rolls back on disconnect)"
	}
	return gws.NewError(code, msg, nil).WithOp("query", inv.Client)
}

func classify(stderr string) (gws.Code, string) {
	s := strings.ToLower(stderr)
	has := func(subs ...string) bool {
		for _, x := range subs {
			if strings.Contains(s, x) {
				return true
			}
		}
		return false
	}
	switch {
	case has("password authentication failed", "wrongpass", "noauth", "authentication failed", "invalid credentials", "incorrect username or password"):
		return gws.CodeMissingCredentials, "authentication failed"
	case has("expired"):
		return gws.CodeExpiredCredentials, "credentials expired"
	case has("permission denied", "noperm", "not authorized", "unauthorized", "insufficient privileges", "read-only transaction"):
		return gws.CodePermissionDenied, "permission denied"
	case has("does not exist", "unknown database", "not found"):
		return gws.CodeNotFound, "target not found"
	case has("too many requests", "rate limit"):
		return gws.CodeRateLimit, "rate limited"
	default:
		return gws.CodeProviderFailure, "client failed"
	}
}
