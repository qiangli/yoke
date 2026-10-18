package msgclients

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

// childEnv scrubs ambient messaging variables from base, then applies the
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

// materializeAux writes each aux content to a 0600 temp file and returns
// the argv with sentinels replaced by real paths, plus a cleanup. File
// contents (kcat SASL config, the rabbitmqadmin payload) never travel on
// argv; temp files are removed before Run returns.
func materializeAux(aux map[string]string) (subst func(string) string, cleanup func(), err error) {
	type made struct{ sentinel, path string }
	var files []made
	cleanup = func() {
		for _, f := range files {
			_ = os.Remove(f.path)
		}
	}
	for sentinel, content := range aux {
		f, cerr := os.CreateTemp("", "msgclients-*")
		if cerr != nil {
			cleanup()
			return nil, func() {}, cerr
		}
		name := f.Name()
		_ = f.Chmod(0o600)
		if _, cerr = f.WriteString(content); cerr != nil {
			_ = f.Close()
			_ = os.Remove(name)
			cleanup()
			return nil, func() {}, cerr
		}
		_ = f.Close()
		files = append(files, made{sentinel, name})
	}
	return func(a string) string {
		for _, f := range files {
			if a == f.sentinel {
				return f.path
			}
		}
		return a
	}, cleanup, nil
}

// Run executes the invocation under its deadline and the caller's context.
// Cancellation or the deadline kills the child; the output read stops at
// the byte bound and kills it too. Every error message is redacted against
// the credential before it leaves this function.
func Run(ctx context.Context, inv Invocation) (*Result, *gws.Error) {
	op, _ := inv.Op.normalized()
	if inv.Op.Timeout <= 0 {
		inv.Op.Timeout = DefaultTimeout
	}
	subst, cleanup, merr := materializeAux(inv.Aux)
	if merr != nil {
		return nil, gws.NewError(gws.CodeProviderFailure, inv.Client+": cannot stage invocation files", nil)
	}
	defer cleanup()
	args := make([]string, len(inv.Args))
	for i, a := range inv.Args {
		args[i] = subst(a)
	}
	rctx, cancel := context.WithTimeout(ctx, inv.Op.Timeout)
	defer cancel()
	cmd := binmgr.Command(rctx, inv.Bin, args...)
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
		return nil, gws.NewError(gws.CodeUnsupported, fmt.Sprintf("%s: cannot start client: %s", inv.Client, gws.RedactText(err.Error(), inv.Secret)), nil).WithOp(inv.Op.Mode, inv.Client)
	}
	res, perr2 := Parse(inv.Client, inv.Op.Topic, out, op)
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
	if inv.Op.Mode == ModePublish {
		res.Mutation = "published"
		return
	}
	res.Mutation = "none"
}

// failure builds the redacted typed error. After a publish has started,
// outcomes other than a definite pre-mutation refusal are reported as
// partial/unknown: cancelling the local request cannot guarantee the
// broker state.
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
	case inv.Op.Mode == ModePublish && !definite:
		msg += " — publish outcome UNKNOWN; verify broker state before retrying"
		code = gws.CodePartialMutation
	case inv.Op.Mode == ModeSubscribe:
		msg += " — bounded read ended without completing; nothing was published"
	}
	return gws.NewError(code, msg, nil).WithOp(inv.Op.Mode, inv.Client)
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
	case has("password", "wrongpass", "noauth", "authentication failed", "invalid credentials", "incorrect username or password", "unauthorized", "not authorized", "403", "401"):
		return gws.CodeMissingCredentials, "authentication failed"
	case has("expired"):
		return gws.CodeExpiredCredentials, "credentials expired"
	case has("permission denied", "noperm", "insufficient privileges", "access refused", "forbidden"):
		return gws.CodePermissionDenied, "permission denied"
	case has("does not exist", "unknown", "not found", "404", "no such", "no queue", "no topic"):
		return gws.CodeNotFound, "target not found"
	case has("too many requests", "rate limit"):
		return gws.CodeRateLimit, "rate limited"
	default:
		return gws.CodeProviderFailure, "client failed"
	}
}
