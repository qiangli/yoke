package telemetry

import (
	"context"
	"reflect"
	"strings"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

// GenAISemConvVersion is the OpenTelemetry GenAI semantic-conventions schema
// implemented by this emitter. GenAI conventions are still Development status,
// so pinning the schema is part of the wire contract rather than documentation.
const GenAISemConvVersion = "1.42.0"

const genAISchemaURL = "https://opentelemetry.io/schemas/gen-ai/" + GenAISemConvVersion

// UnknownVenue is emitted when the host did not attach an execution venue.
// Unknown is a value in its own right: it must never be silently collapsed into
// userland, workspace, or any other real venue.
const UnknownVenue = "UNKNOWN"

type genAIVenueKey struct{}

// WithGenAIVenue lets a host attach its execution-tier policy to model calls.
// The telemetry package deliberately treats venue as an opaque value; it does
// not know the host's tier names or infer them from process-global state.
func WithGenAIVenue(ctx context.Context, venue string) context.Context {
	return context.WithValue(ctx, genAIVenueKey{}, normalizedVenue(venue))
}

// GenAIVenue returns the venue attached by the host, or UNKNOWN.
func GenAIVenue(ctx context.Context) string {
	if ctx != nil {
		if venue, ok := ctx.Value(genAIVenueKey{}).(string); ok {
			return normalizedVenue(venue)
		}
	}
	return UnknownVenue
}

func normalizedVenue(venue string) string {
	if venue = strings.TrimSpace(venue); venue != "" {
		return venue
	}
	return UnknownVenue
}

// GenAITurn describes a host-observed turn. CoverageScope is intentionally
// bashy-private: the GenAI conventions do not define a completeness claim, and
// force-fitting one would let fleet panels mistake subprocess observations for
// direct provider instrumentation.
type GenAITurn struct {
	Venue            string
	CoverageScope    string
	CoverageComplete bool
}

// GenAICall describes the structural facts known when an inference call starts.
// It has deliberately no prompt, message, tool argument, or other content field.
type GenAICall struct {
	Operation        string
	Provider         string
	RequestModel     string
	Venue            string
	CoverageScope    string
	CoverageComplete bool
}

// GenAICallResult describes Tier 1 response metadata. Pointer fields distinguish
// a known zero from unknown; unknown cost is never serialized as zero.
type GenAICallResult struct {
	ResponseModel string
	FinishReasons []string
	InputTokens   *int64
	OutputTokens  *int64
	UsageSource   string
	CostUSD       *float64
	PricingKnown  bool
}

// StartGenAITurn starts the INTERNAL parent for one host-observed turn.
func StartGenAITurn(ctx context.Context, cfg GenAITurn) (context.Context, func(error)) {
	ctx, span := genAITracer().Start(ctx, "gen_ai.turn",
		trace.WithSpanKind(trace.SpanKindInternal),
		trace.WithAttributes(coverageAttributes(cfg.Venue, cfg.CoverageScope, cfg.CoverageComplete)...),
	)
	return ctx, func(err error) {
		setTier1Error(span, err, "gen_ai turn failed")
		span.End()
	}
}

// StartGenAICall starts a GenAI inference span under ctx. The returned closure
// must be called exactly once after the full response has been received.
func StartGenAICall(ctx context.Context, cfg GenAICall) (context.Context, func(GenAICallResult, error)) {
	operation := strings.TrimSpace(cfg.Operation)
	if operation == "" {
		operation = "chat"
	}
	name := operation
	if model := strings.TrimSpace(cfg.RequestModel); model != "" {
		name += " " + model
	}

	attrs := []attribute.KeyValue{
		attribute.String("gen_ai.operation.name", operation),
	}
	if provider := strings.TrimSpace(cfg.Provider); provider != "" {
		attrs = append(attrs, attribute.String("gen_ai.provider.name", provider))
	}
	if model := strings.TrimSpace(cfg.RequestModel); model != "" {
		attrs = append(attrs, attribute.String("gen_ai.request.model", model))
	}
	attrs = append(attrs, coverageAttributes(cfg.Venue, cfg.CoverageScope, cfg.CoverageComplete)...)

	ctx, span := genAITracer().Start(ctx, name,
		trace.WithSpanKind(trace.SpanKindClient),
		trace.WithAttributes(attrs...),
	)
	return ctx, func(result GenAICallResult, err error) {
		var resultAttrs []attribute.KeyValue
		if model := strings.TrimSpace(result.ResponseModel); model != "" {
			resultAttrs = append(resultAttrs, attribute.String("gen_ai.response.model", model))
		}
		if len(result.FinishReasons) > 0 {
			resultAttrs = append(resultAttrs, attribute.StringSlice("gen_ai.response.finish_reasons", result.FinishReasons))
		}
		if result.InputTokens != nil {
			resultAttrs = append(resultAttrs, attribute.Int64("gen_ai.usage.input_tokens", *result.InputTokens))
		}
		if result.OutputTokens != nil {
			resultAttrs = append(resultAttrs, attribute.Int64("gen_ai.usage.output_tokens", *result.OutputTokens))
		}
		if source := strings.TrimSpace(result.UsageSource); source != "" {
			resultAttrs = append(resultAttrs, attribute.String("bashy.gen_ai.usage.source", source))
		}
		resultAttrs = append(resultAttrs, attribute.Bool("bashy.gen_ai.pricing_known", result.PricingKnown))
		if result.PricingKnown && result.CostUSD != nil {
			resultAttrs = append(resultAttrs, attribute.Float64("bashy.gen_ai.cost.usd", *result.CostUSD))
		}
		span.SetAttributes(resultAttrs...)
		setTier1Error(span, err, "gen_ai call failed")
		span.End()
	}
}

func genAITracer() trace.Tracer {
	return otel.Tracer(instrumentationName, trace.WithSchemaURL(genAISchemaURL))
}

func coverageAttributes(venue, scope string, complete bool) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("bashy.execution.venue", normalizedVenue(venue)),
		attribute.Bool("bashy.gen_ai.coverage.complete", complete),
	}
	if scope = strings.TrimSpace(scope); scope != "" {
		attrs = append(attrs, attribute.String("bashy.gen_ai.coverage.scope", scope))
	}
	return attrs
}

// setTier1Error records only a low-cardinality error type and a constant status
// description. Error strings can contain prompts, provider responses, and keys.
func setTier1Error(span trace.Span, err error, status string) {
	if err == nil {
		return
	}
	typ := reflect.TypeOf(err)
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	span.SetAttributes(attribute.String("error.type", typ.PkgPath()+"."+typ.Name()))
	span.SetStatus(codes.Error, status)
}
