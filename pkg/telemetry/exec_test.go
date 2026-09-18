//go:build !aix

package telemetry

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	"go.opentelemetry.io/otel/trace/noop"
	"mvdan.cc/sh/v3/interp"
)

// withRecorder turns telemetry on against an in-memory exporter, so a test asserts what
// was ACTUALLY EMITTED rather than that a function was called.
//
// That distinction is the entire lesson of the week: ConversationMessage.Usage,
// ExemptFromMasking, StreamOptions, and three config fields were all declared, wired,
// reviewed — and dead. A test that mocks the emitter proves the emitter was invoked. A
// test that reads the spans proves the data exists.
func withRecorder(t *testing.T) *tracetest.SpanRecorder {
	t.Helper()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	// Install it GLOBALLY, because that is how a real host provides one — bashy via
	// telemetry.Init, ycode via its own internal/telemetry/otel. The middleware reads
	// the global provider precisely so it works in both.
	//
	// The first version of this helper set a package-LOCAL tracer, and so tested a code
	// path production does not take. It passed while the middleware emitted nothing at
	// all inside ycode.
	old := otel.GetTracerProvider()
	otel.SetTracerProvider(tp)
	t.Cleanup(func() { otel.SetTracerProvider(old) })

	return sr
}

func attrs(t *testing.T, sr *tracetest.SpanRecorder) map[string]string {
	t.Helper()
	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("expected exactly 1 span, got %d", len(spans))
	}
	out := map[string]string{}
	for _, kv := range spans[0].Attributes() {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}

// THE EXIT CODE IS THE POINT.
//
// "All three harnesses exit 0 when they fail — a harness's exit code is not evidence"
// is this project's own finding, and then ycode did it too. An exit status that is
// RECORDED can be argued with. One that is only inspected by the thing that produced it
// is not evidence of anything.
func TestAFailingCommandRecordsItsExitCode(t *testing.T) {
	sr := withRecorder(t)

	h := ExecMiddleware(func(ctx context.Context, args []string) error {
		return interp.ExitStatus(3)
	})
	err := h(context.Background(), []string{"gate.sh", "--check"})
	if !errors.Is(err, interp.ExitStatus(3)) {
		t.Fatalf("middleware changed the error: %v", err)
	}

	a := attrs(t, sr)
	if a["cmd.exit_code"] != "3" {
		t.Errorf("cmd.exit_code = %q, want 3 — an unrecorded exit code is how a failure passes for a success", a["cmd.exit_code"])
	}
	if a["cmd.name"] != "gate.sh" {
		t.Errorf("cmd.name = %q, want gate.sh", a["cmd.name"])
	}
}

// A SPAN LEAVES THE MACHINE. An argv carrying `--api-key sk-…` into a collector turns an
// observability feature into an exfiltration one — and this repo has already leaked two
// live keys into a transcript, which is exactly how confident one should be here.
func TestArgvIsRedactedBeforeItLeavesTheProcess(t *testing.T) {
	sr := withRecorder(t)

	h := ExecMiddleware(func(ctx context.Context, args []string) error { return nil })
	_ = h(context.Background(), []string{
		"curl", "-H", "Authorization: Bearer sk-live-abcdefghijklmnop",
		"--api-key", "sk-secret-value-abcdefgh",
		"--token=ghp_reallylongtokenvalue123",
		"https://api.example.com",
	})

	a := attrs(t, sr)
	argv := a["cmd.argv"]

	for _, leaked := range []string{"sk-live-abcdefghijklmnop", "sk-secret-value-abcdefgh", "ghp_reallylongtokenvalue123"} {
		if strings.Contains(argv, leaked) {
			t.Errorf("a credential reached the span: %q\nargv: %s", leaked, argv)
		}
	}
	// The SHAPE survives — "which command, which flags" must stay answerable, or the
	// redaction has destroyed the thing we are here to observe.
	for _, keep := range []string{"curl", "--api-key", "https://api.example.com"} {
		if !strings.Contains(argv, keep) {
			t.Errorf("redaction destroyed the command's shape: %q missing from %s", keep, argv)
		}
	}
}

// A BOUND YOU CANNOT SEE IS NOT A BOUND, IT IS A TRAP.
//
// Every failure this package answers was a limit that bound silently: a 25-iteration cap
// that stopped an agent and exited 0; a 15-iteration subagent cap that discarded its
// findings; a 4096-byte pty write that truncated a prompt.
func TestBoundHitIsRecordedEvenWhenTheRunRecovers(t *testing.T) {
	sr := withRecorder(t)

	ctx, span := otel.Tracer("test").Start(context.Background(), "agent.turn")
	BoundHit(ctx, "iterations", 25, 25, "agent had not finished")
	span.End()

	spans := sr.Ended()
	if len(spans) != 1 {
		t.Fatalf("want 1 span, got %d", len(spans))
	}

	var found bool
	for _, e := range spans[0].Events() {
		if e.Name == "bound.hit" {
			found = true
			for _, kv := range e.Attributes {
				if string(kv.Key) == "bound.kind" && kv.Value.Emit() != "iterations" {
					t.Errorf("bound.kind = %q", kv.Value.Emit())
				}
			}
		}
	}
	if !found {
		t.Fatal("a bound bound and no bound.hit event was emitted — the trap is still a trap")
	}

	var flagged bool
	for _, kv := range spans[0].Attributes() {
		if string(kv.Key) == "bound.was_hit" {
			flagged = true
		}
	}
	if !flagged {
		t.Error("the span was not flagged bound.was_hit — a query cannot find what is not marked")
	}
}

// A NUMBER WITHOUT ITS PROVENANCE CANNOT BE AUDITED.
//
// The only reason a dead usage-plumbing bug was caught was one log line printing WHERE a
// token count came from, next to the count. 6482 tells you nothing. 6482 from the
// PROVIDER tells you the gate works; 6482 ESTIMATED tells you it does not.
func TestProvenanceRecordsWhereTheNumberCameFrom(t *testing.T) {
	sr := withRecorder(t)

	ctx, span := otel.Tracer("test").Start(context.Background(), "agent.turn")
	Provenance(ctx, "context.tokens", 6482, "provider")
	span.End()

	var sources []string
	for _, e := range sr.Ended()[0].Events() {
		if e.Name != "value" {
			continue
		}
		for _, kv := range e.Attributes {
			if string(kv.Key) == "value.source" {
				sources = append(sources, kv.Value.Emit())
			}
		}
	}
	if !slices.Contains(sources, "provider") {
		t.Errorf("value.source not recorded (got %v) — an unauditable number will eventually be "+
			"wrong in a way nobody can see", sources)
	}
}

// A shell that pays for telemetry it is not exporting is a shell nobody uses.
func TestDisabledTelemetryIsAPureNoOp(t *testing.T) {
	// No global provider at all — the default state of any process that never configured
	// telemetry. The global default is a no-op whose spans never record.
	sr := tracetest.NewSpanRecorder()
	otel.SetTracerProvider(noop.NewTracerProvider())
	t.Cleanup(func() { otel.SetTracerProvider(noop.NewTracerProvider()) })

	called := false
	h := ExecMiddleware(func(ctx context.Context, args []string) error {
		called = true
		return nil
	})
	if err := h(context.Background(), []string{"ls"}); err != nil {
		t.Fatal(err)
	}

	if !called {
		t.Fatal("the middleware swallowed the command when telemetry was off")
	}
	if n := len(sr.Ended()); n != 0 {
		t.Errorf("telemetry is off and %d spans were still produced", n)
	}
}
