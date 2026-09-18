//go:build !aix

// The optional shell middleware follows the interpreter support boundary.
// Portable admission telemetry remains available on AIX without importing sh.
package telemetry

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
	"mvdan.cc/sh/v3/interp"
)

// ExecMiddleware emits one span per command bashy runs.
//
// It sits in the same ExecHandler chain as the audit log and the space-time advisor —
// the one chokepoint every dispatched command crosses — so it sees the userland, the
// PATH fallbacks, and every agent-issued command alike, with no per-command wiring.
//
// It is a NO-OP when telemetry is not exporting: no span, no allocation, no wrapper. A
// shell that pays for telemetry it is not sending is a shell nobody uses.
//
// What it records is chosen from what we could not see when it mattered:
//
//	the argv, so "which command" is answerable at all
//	the EXIT CODE, because a failure that exits 0 is the bug we keep finding
//	the duration, because "it hung" and "it was throttled" look identical without it
//	the cwd, because a wrong-directory failure was common enough to build an advisor for
//	the agent principal, because "which agent did this" was unanswerable across a fleet run
func ExecMiddleware(next interp.ExecHandlerFunc) interp.ExecHandlerFunc {
	return func(ctx context.Context, args []string) error {
		if len(args) == 0 {
			return next(ctx, args)
		}

		// THE GLOBAL PROVIDER, not one this package owns.
		//
		// The first version gated on a package-local `enabled` that only THIS package's
		// Init() could set. It worked in bashy and emitted NOTHING in ycode — because
		// ycode configures its own provider (internal/telemetry/otel) and never calls
		// ours. The middleware was installed, the commands ran, and every span was
		// silently dropped.
		//
		// That is the library/binary boundary drawn in the wrong place. A LIBRARY EMITS
		// THROUGH THE GLOBAL PROVIDER; ONLY THE BINARY CONFIGURES ONE. Doing it this way
		// means the same middleware works in bashy, in ycode, and in any host that has
		// set a provider up — and is free in one that has not, because the default global
		// provider is a no-op whose spans never record.
		ctx, span := otel.Tracer(instrumentationName).Start(ctx, "exec "+args[0],
			trace.WithSpanKind(trace.SpanKindInternal),
			trace.WithAttributes(
				attribute.String("cmd.name", args[0]),
				attribute.StringSlice("cmd.argv", redactArgv(args)),
				attribute.Int("cmd.argc", len(args)),
			),
		)
		defer span.End()

		// Not recording (no provider, or sampled out): do no attribute work at all. A
		// shell that pays for telemetry it is not exporting is a shell nobody uses.
		if !span.IsRecording() {
			return next(ctx, args)
		}

		if dir := handlerDir(ctx); dir != "" {
			span.SetAttributes(attribute.String("cmd.cwd", dir))
		}
		// Who ran this. Across a fleet run, "which agent did this" was unanswerable.
		if p := os.Getenv("BASHY_PRINCIPAL"); p != "" {
			span.SetAttributes(attribute.String("agent.principal", p))
		}

		started := time.Now()
		err := next(ctx, args)
		span.SetAttributes(attribute.Int64("cmd.duration_ms", time.Since(started).Milliseconds()))

		// THE EXIT CODE IS THE POINT.
		//
		// "All three harnesses exit 0 when they fail" is this project's own finding, and
		// then ycode did it too. An exit status that is recorded is an exit status that
		// can be argued with; one that is only inspected by the thing that produced it is
		// not evidence of anything.
		code := 0
		if err != nil {
			var status interp.ExitStatus
			if ok := asExitStatus(err, &status); ok {
				code = int(status)
			} else {
				code = -1
				span.RecordError(err)
			}
		}
		span.SetAttributes(attribute.Int("cmd.exit_code", code))
		if code != 0 {
			span.SetStatus(codes.Error, "exit "+itoa(code))
		}

		return err
	}
}

// redactArgv keeps the shape of a command without carrying its secrets into a trace.
//
// A span is an artifact that leaves the machine. An argv that carries `--api-key sk-…`
// into a collector has turned an observability feature into an exfiltration one — and
// this repo has ALREADY leaked two live keys into a transcript today, which is precisely
// how confident one should be about this going wrong.
//
// Shape over content: the value of a flag that LOOKS like a credential is replaced, the
// flag itself is kept, so "which command with which flags" stays answerable.
func redactArgv(args []string) []string {
	out := make([]string, len(args))
	copy(out, args)

	for i := 0; i < len(out); i++ {
		arg := out[i]

		// --key=value
		if eq := strings.IndexByte(arg, '='); eq > 0 && strings.HasPrefix(arg, "-") {
			if looksSecret(arg[:eq]) {
				out[i] = arg[:eq] + "=<redacted>"
				continue
			}
		}
		// --key value
		if strings.HasPrefix(arg, "-") && looksSecret(arg) && i+1 < len(out) {
			out[i+1] = "<redacted>"
			i++
			continue
		}
		// A bare token that looks like a key, wherever it appears.
		if looksLikeSecretValue(arg) {
			out[i] = "<redacted>"
		}
	}
	return out
}

func looksSecret(flag string) bool {
	f := strings.ToLower(strings.TrimLeft(flag, "-"))
	for _, s := range []string{"key", "token", "secret", "password", "passwd", "auth", "credential"} {
		if strings.Contains(f, s) {
			return true
		}
	}
	return false
}

// looksLikeSecretValue catches a credential ANYWHERE in an argv token.
//
// The first version matched only a PREFIX, and a test found the hole immediately:
//
//	curl -H "Authorization: Bearer sk-live-abcdef…"
//
// The flag is `-H`, which looks innocent. The credential is buried mid-string, inside a
// header. It sailed straight through and would have landed in a collector.
//
// The lesson is the one this whole day keeps teaching: THE THING YOU ARE LOOKING FOR IS
// NOT WHERE YOU EXPECT IT. Scan the whole token, and treat auth-bearing shapes as secret
// wherever they appear.
//
// Deliberately over-broad. Over-redacting a span costs a little debuggability; under-
// redacting one ships a live credential off the machine — and this repo leaked two keys
// into a transcript today, which is exactly how confident one should be here.
func looksLikeSecretValue(s string) bool {
	if len(s) < 12 {
		return false
	}
	lower := strings.ToLower(s)

	// An auth header carries a credential by definition, whatever shape it takes.
	if strings.Contains(lower, "bearer ") || strings.Contains(lower, "authorization:") {
		return true
	}

	// Known provider key shapes, ANYWHERE in the token — not merely at its start.
	for _, p := range []string{"sk-", "sk_", "ghp_", "gho_", "ghs_", "github_pat_", "xoxb-", "xoxp-", "akia", "glpat-", "hf_"} {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
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

// asExitStatus unwraps the interpreter's exit status from an error.
func asExitStatus(err error, out *interp.ExitStatus) bool {
	var status interp.ExitStatus
	if errors.As(err, &status) {
		*out = status
		return true
	}
	return false
}

// handlerDir reads the shell's cwd WITHOUT being able to crash the shell.
//
// interp.HandlerCtx PANICS when the context carries no HandlerContext. Telemetry sits
// OUTERMOST in the exec chain, so a panic here takes the whole shell down — and it would
// do so only in the contexts the shell did not construct itself, which is exactly where
// nobody is looking.
//
// A test caught it. It is the correct property regardless:
//
//	OBSERVABILITY MUST NEVER BE ABLE TO BREAK THE THING IT OBSERVES.
//
// A missing cwd is a missing attribute. It is not an outage.
func handlerDir(ctx context.Context) (dir string) {
	defer func() {
		if recover() != nil {
			dir = ""
		}
	}()
	return interp.HandlerCtx(ctx).Dir
}
