// Package telemetry gives bashy an OpenTelemetry voice.
//
// bashy could already RUN an observability stack (`bashy otel` — collector, Jaeger,
// VictoriaMetrics, Perses) and emitted NOTHING into it. A collector with no data. The
// umbrella's own trace-propagation contract lists service.name values for ycode,
// cloudbox-hub, outpost, loom and act_runner — and not for bashy. The foundation of the
// stack was the one tier that was invisible.
//
// # What this is FOR, which is not "instrument everything"
//
// Six hours of debugging produced five bugs, and every one of them was invisible in the
// same way:
//
//	"the agent can't converge"    -> our 25-turn cap, and truncation EXITED 0
//	"its delegates fail"          -> our cap discarded their work; the error carried nothing
//	"rate limits killed the run"  -> three 429s, all recovered; no per-provider signal existed
//	"the config didn't apply"     -> the field was never merged, and nothing said so
//	"the model did nothing"       -> a 4096-byte pty truncation nobody could see
//
// Not one was a mystery about WHAT the code did. Every one was a number that got used
// without saying where it came from, or a bound that got hit without saying so.
//
// So this package has exactly two jobs, and they are the two that would have caught all
// five:
//
//	PROVENANCE — a value recorded next to WHERE IT CAME FROM. A number without its
//	             provenance cannot be audited. (The one signal that did catch a bug today
//	             was a log line printing `from_provider=false` next to a token count.)
//
//	BOUNDS     — a limit records when it BINDS, always, as a first-class event. A bound
//	             you cannot see is not a bound, it is a trap. Truncation, caps, timeouts,
//	             rate limits: each is a fact about the harness, and each was being
//	             silently absorbed.
//
// # Discipline
//
// Configuration is pure standard OTEL env vars (OTEL_EXPORTER_OTLP_ENDPOINT,
// OTEL_SERVICE_NAME, OTEL_RESOURCE_ATTRIBUTES, OTEL_TRACES_SAMPLER). With no endpoint
// configured, spans default to the bounded local file spool; set
// OTEL_TRACES_EXPORTER=none for a complete no-op. Network export remains opt-in: set
// an OTLP endpoint or exporter explicitly. The global propagator is installed even in
// no-op mode, so the wire-format contract survives without a collector.
package telemetry

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracehttp"
	"go.opentelemetry.io/otel/propagation"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/term"
)

// ServiceName is bashy's identity on the OTel plane. It joins the umbrella's existing
// service.name set (ycode, cloudbox-hub, outpost, loom, act_runner) — which it was
// missing from.
const ServiceName = "bashy"

// instrumentationName identifies this instrumentation library in every span it emits.
const instrumentationName = "github.com/qiangli/yoke/pkg/telemetry"

var (
	initOnce  sync.Once
	provider  *sdktrace.TracerProvider
	mprovider *sdkmetric.MeterProvider
	enabled   bool
)

// Enabled reports whether telemetry is actually exporting. Callers should not need this
// — every helper here is safe when it is false — but a `bashy doctor` line that says so
// out loud is worth more than a silent no-op.
func Enabled() bool { return enabled }

// Init wires the OTel plane from standard env vars, once.
//
// With no exporter or endpoint configured it uses the bounded local file spool.
// OTEL_TRACES_EXPORTER=none installs only the global propagator: no exporter, batcher,
// or goroutine.
func Init(ctx context.Context) (shutdown func(context.Context) error) {
	shutdown = func(context.Context) error { return nil }

	initOnce.Do(func() {
		// The propagator goes in either way. A hop that drops traceparent orphans every
		// span downstream of it, and that is true whether or not THIS process exports.
		otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
			propagation.TraceContext{},
			propagation.Baggage{},
		))
		exporter := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_TRACES_EXPORTER")))
		switch exporter {
		case "", "file", "console", "none", "otlp", "grpc", "http", "http/protobuf":
		default:
			os.Stderr.WriteString("bashy: telemetry disabled — unsupported OTEL_TRACES_EXPORTER=" + exporter + "\n")
			return
		}

		// File sink, chosen by the standard OTEL_TRACES_EXPORTER=file.
		//
		// Checked BEFORE the endpoint gate because it deliberately needs no
		// endpoint: the point is telemetry that works with nothing running.
		// An OTLP endpoint requires a collector to be up at the instant a span
		// is produced — anything running before it, or while it is down, or on
		// a host that never started it, exports into a closed port and is lost
		// silently. Spooling to a file removes that precondition; the stack
		// becomes an optional upgrade for retention and alerting.
		if isFileExporter() {
			spoolPath := SpoolPath()
			sexp, serr := newSpoolExporter(spoolPath)
			if serr != nil {
				os.Stderr.WriteString("bashy: telemetry disabled — spool: " + serr.Error() + "\n")
				return
			}
			svc := os.Getenv("OTEL_SERVICE_NAME")
			if svc == "" {
				svc = ServiceName
			}
			res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
				semconv.SchemaURL,
				semconv.ServiceName(svc),
			))
			provider = sdktrace.NewTracerProvider(
				sdktrace.WithBatcher(sexp),
				sdktrace.WithResource(res),
			)
			otel.SetTracerProvider(provider)
			enabled = true
			if shouldReportTelemetryStatus(isTerminal(os.Stderr)) {
				os.Stderr.WriteString("bashy: telemetry on → " + displayPath(sexp.path) + " (service=" + svc + ")\n")
			}
			shutdown = func(ctx context.Context) error {
				ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
				defer cancel()
				return provider.Shutdown(ctx)
			}
			return
		}

		endpoint := networkEndpoint(exporter)
		if endpoint == "" {
			return // no-op mode. Deliberately, and completely.
		}

		// BOTH PROTOCOLS, chosen by the standard env var. Not one, hard-coded.
		//
		// The embedded stack no longer runs an OTel Collector — the only thing in it that
		// spoke gRPC. Every Victoria component ingests OTLP/HTTP natively and the proxy
		// fans /v1/{traces,logs,metrics} out to them, so the stack's own endpoint is HTTP.
		//
		// But hard-coding HTTP would have broken every gRPC collector in the world,
		// including the umbrella's own otlp-receiver — which is exactly how I found out,
		// because the first version silently sent to nothing and I nearly recorded another
		// process's spans as proof that it worked.
		//
		// OTEL_EXPORTER_OTLP_PROTOCOL is the standard knob: "grpc" or "http/protobuf".
		// The spec's default is http/protobuf, and so is ours.
		var exp sdktrace.SpanExporter
		var err error
		protocol := strings.ToLower(strings.TrimSpace(os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL")))
		if protocol == "" && exporter == "grpc" {
			protocol = "grpc"
		}
		switch protocol {
		case "grpc":
			exp, err = otlptracegrpc.New(ctx)
		default:
			exp, err = otlptracehttp.New(ctx)
		}
		if err != nil {
			// A shell must not fail to start because a collector is down. Say so once,
			// on stderr, and carry on in no-op mode — SILENTLY degrading here would be
			// the very bug this package exists to make visible.
			os.Stderr.WriteString("bashy: telemetry disabled — OTLP exporter: " + err.Error() + "\n")
			return
		}

		svc := os.Getenv("OTEL_SERVICE_NAME")
		if svc == "" {
			svc = ServiceName
		}
		res, _ := resource.Merge(resource.Default(), resource.NewWithAttributes(
			semconv.SchemaURL,
			semconv.ServiceName(svc),
		))

		provider = sdktrace.NewTracerProvider(
			sdktrace.WithBatcher(exp),
			sdktrace.WithResource(res),
		)
		otel.SetTracerProvider(provider)
		var mexp sdkmetric.Exporter
		switch protocol {
		case "grpc":
			mexp, _ = otlpmetricgrpc.New(ctx)
		default:
			mexp, _ = otlpmetrichttp.New(ctx)
		}
		if mexp != nil {
			mprovider = sdkmetric.NewMeterProvider(
				sdkmetric.WithReader(sdkmetric.NewPeriodicReader(mexp)),
				sdkmetric.WithResource(res),
			)
			otel.SetMeterProvider(mprovider)
		}

		enabled = true

		if shouldReportTelemetryStatus(isTerminal(os.Stderr)) {
			os.Stderr.WriteString("bashy: telemetry on → " + endpoint + " (service=" + svc + ")\n")
		}

		shutdown = func(ctx context.Context) error {
			ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			if mprovider != nil {
				_ = mprovider.Shutdown(ctx)
			}
			return provider.Shutdown(ctx)
		}
	})

	return shutdown
}

// shouldReportTelemetryStatus keeps routine startup chatter out of captured agent
// stderr. Interactive users still see the selected destination; noninteractive
// callers may request it with BASHY_TELEMETRY_NOTICE or BASHY_TELEMETRY_DEBUG.
// BASHY_TELEMETRY_QUIET remains a compatibility override for every mode.
func shouldReportTelemetryStatus(stderrIsTerminal bool) bool {
	if automationMarker(os.Getenv("BASHY_TELEMETRY_QUIET")) {
		return false
	}
	if automationMarker(os.Getenv("BASHY_TELEMETRY_NOTICE")) || automationMarker(os.Getenv("BASHY_TELEMETRY_DEBUG")) {
		return true
	}
	return stderrIsTerminal && !telemetryAutomation()
}

// telemetryAutomation recognizes positive evidence that a terminal belongs to
// a harness rather than a watching human. Agent runners allocate PTYs to obtain
// correct CLI behavior, so term.IsTerminal alone cannot establish attendance.
func telemetryAutomation() bool {
	for _, name := range []string{"CI", "CODEX_CI", "WEAVE_AGENT", "BASHY_AGENT_ID", "BASHY_PRINCIPAL"} {
		if automationMarker(os.Getenv(name)) {
			return true
		}
	}
	return strings.EqualFold(strings.TrimSpace(os.Getenv("TERM")), "dumb")
}

func automationMarker(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func isTerminal(f *os.File) bool {
	return f != nil && term.IsTerminal(int(f.Fd()))
}

// displayPath keeps startup notices private and compact without changing the
// absolute path used by the exporter. Only a path actually within the current
// user's home is abbreviated; paths elsewhere remain exact.
func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || strings.TrimSpace(home) == "" {
		return path
	}
	home, err = filepath.Abs(home)
	if err != nil {
		return path
	}
	absPath, err := filepath.Abs(path)
	if err != nil {
		return path
	}
	rel, err := filepath.Rel(home, absPath)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(os.PathSeparator)) {
		return path
	}
	if rel == "." {
		return "$HOME"
	}
	return filepath.Join("$HOME", rel)
}

// Tracer returns bashy's tracer. Safe before Init: the global provider is a no-op until
// a host configures one.
func Tracer() trace.Tracer { return otel.Tracer(instrumentationName) }

// standalone starts a span for a fact that has no parent — a bound that bound, or a
// number whose provenance matters, recorded from a code path nobody thought to
// instrument.
//
// It reads the GLOBAL provider, so it is free (a no-op span, never recorded) in a process
// that configured no telemetry, and real in one that did.
func standalone(ctx context.Context, name string) (context.Context, func()) {
	ctx, span := otel.Tracer(instrumentationName).Start(ctx, name)
	return ctx, func() { span.End() }
}

// --- The two things this package exists for --------------------------------------

// Provenance records a VALUE together with WHERE IT CAME FROM.
//
// This is the whole point. A token count of 6482 tells you nothing; a token count of 6482
// that the PROVIDER REPORTED tells you the context gate is working, and a token count of
// 6482 that we ESTIMATED tells you it is not. Today, the only reason a dead
// usage-plumbing bug was caught at all is that one log line printed the second thing next
// to the first.
//
//	source: where the number came from — "provider", "estimate", "config", "default",
//	        "cache", "measured", "declared". Whatever it is, SAY it.
//
// A number without its provenance cannot be audited, and an unauditable number will
// eventually be wrong in a way nobody can see.
func Provenance(ctx context.Context, name string, value int64, source string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		// NO ACTIVE SPAN IS NOT A REASON TO SAY NOTHING.
		//
		// The first version returned here, and it silently emitted nothing at every call
		// site that happened not to sit inside an instrumented span — which was most of
		// them, including the truncation path this exists to record. A helper that
		// quietly does nothing when its precondition is unmet is the exact disease this
		// package was built to detect, committed inside the detector.
		//
		// A fact worth recording is worth its own span.
		var end func()
		ctx, end = standalone(ctx, "value."+name)
		defer end()
		span = trace.SpanFromContext(ctx)
		if !span.IsRecording() {
			return // genuinely no provider configured; now it really is free
		}
	}
	span.AddEvent("value", trace.WithAttributes(
		attribute.String("value.name", name),
		attribute.Int64("value.amount", value),
		attribute.String("value.source", source),
	))
}

// CoordinationAdmission records only aggregate accounting and a content
// digest. Message bodies, senders, topics, IDs, and retrieval paths never enter
// telemetry through this event.
func CoordinationAdmission(ctx context.Context, digest string, inputItems, inputBytes, admittedItems, renderedBytes, omittedItems, omittedBytes int64) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		var end func()
		ctx, end = standalone(ctx, "coordination.admission")
		defer end()
		span = trace.SpanFromContext(ctx)
		if !span.IsRecording() {
			return
		}
	}
	span.AddEvent("coordination.admission", trace.WithAttributes(
		attribute.String("coordination.content_digest", digest),
		attribute.Int64("coordination.input_items", inputItems),
		attribute.Int64("coordination.input_bytes", inputBytes),
		attribute.Int64("coordination.admitted_items", admittedItems),
		attribute.Int64("coordination.rendered_bytes", renderedBytes),
		attribute.Int64("coordination.omitted_items", omittedItems),
		attribute.Int64("coordination.omitted_bytes", omittedBytes),
	))
}

// BoundHit records that a LIMIT BOUND — that something was cut short, capped, truncated,
// throttled or timed out.
//
// Every failure this package was built in response to was a bound that bound silently:
//
//	a 25-iteration cap that stopped an agent mid-investigation and EXITED 0
//	a 15-iteration subagent cap that discarded its findings and returned an error
//	a 4096-byte pty write that truncated a prompt with no indication
//	a rate limit absorbed by a retry that nothing counted
//
// A bound you cannot see is not a bound, it is a trap. If a limit changes what the system
// did, it says so here — always, even when the run recovers. ESPECIALLY when the run
// recovers, because a bound that binds and recovers is the one nobody investigates until
// it stops recovering.
func BoundHit(ctx context.Context, kind string, limit int64, actual int64, detail string) {
	span := trace.SpanFromContext(ctx)
	if !span.IsRecording() {
		// See Provenance: a bound that binds outside an instrumented span still bound.
		// It gets its own span rather than vanishing.
		var end func()
		ctx, end = standalone(ctx, "bound."+kind)
		defer end()
		span = trace.SpanFromContext(ctx)
		if !span.IsRecording() {
			return
		}
	}
	span.AddEvent("bound.hit", trace.WithAttributes(
		attribute.String("bound.kind", kind), // "iterations", "bytes", "rate_limit", "timeout", "context_window"
		attribute.Int64("bound.limit", limit),
		attribute.Int64("bound.actual", actual),
		attribute.String("bound.detail", detail),
	))
	span.SetAttributes(attribute.Bool("bound.was_hit", true))
}
