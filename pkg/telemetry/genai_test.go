package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"go.opentelemetry.io/otel/attribute"
)

func TestGenAITurnContainsNestedCallWithTier1AttributesOnly(t *testing.T) {
	sr := withRecorder(t)
	ctx, endTurn := StartGenAITurn(WithGenAIVenue(context.Background(), "sandbox"), GenAITurn{
		Venue: "sandbox", CoverageScope: "subprocess_harness", CoverageComplete: false,
	})
	ctx, endCall := StartGenAICall(ctx, GenAICall{
		Operation: "chat", Provider: "openai", RequestModel: "gpt-test",
		Venue: "sandbox", CoverageScope: "subprocess_harness", CoverageComplete: false,
	})
	input, output, cost := int64(11), int64(7), 0.012
	endCall(GenAICallResult{
		FinishReasons: []string{"stop"}, InputTokens: &input, OutputTokens: &output,
		UsageSource: "provider", CostUSD: &cost, PricingKnown: true,
	}, nil)
	endTurn(nil)

	spans := sr.Ended()
	if len(spans) != 2 {
		t.Fatalf("ended spans = %d, want turn + call", len(spans))
	}
	var turn, call = spans[1], spans[0]
	if turn.Name() != "gen_ai.turn" || call.Name() != "chat gpt-test" {
		t.Fatalf("span names = %q -> %q", turn.Name(), call.Name())
	}
	if call.Parent().SpanID() != turn.SpanContext().SpanID() {
		t.Fatalf("call parent = %s, want turn span %s", call.Parent().SpanID(), turn.SpanContext().SpanID())
	}
	got := spanAttributes(call.Attributes())
	for key, want := range map[string]string{
		"gen_ai.operation.name":          "chat",
		"gen_ai.provider.name":           "openai",
		"gen_ai.request.model":           "gpt-test",
		"gen_ai.response.finish_reasons": "[\"stop\"]",
		"gen_ai.usage.input_tokens":      "11",
		"gen_ai.usage.output_tokens":     "7",
		"bashy.gen_ai.cost.usd":          "0.012",
		"bashy.gen_ai.pricing_known":     "true",
		"bashy.execution.venue":          "sandbox",
		"bashy.gen_ai.coverage.complete": "false",
		"bashy.gen_ai.coverage.scope":    "subprocess_harness",
	} {
		if got[key] != want {
			t.Errorf("%s = %q, want %q", key, got[key], want)
		}
	}
}

func TestGenAISpansNeverCapturePromptCompletionOrErrorContent(t *testing.T) {
	const promptSecret = "PROMPT_SECRET_sk-live-never-record"
	const completionSecret = "COMPLETION_SECRET_ghp_never_record"
	sr := withRecorder(t)
	ctx, endTurn := StartGenAITurn(context.Background(), GenAITurn{})
	_, endCall := StartGenAICall(ctx, GenAICall{Operation: "chat", RequestModel: "safe-model"})
	endCall(GenAICallResult{}, errors.New(promptSecret+" "+completionSecret))
	endTurn(nil)

	wire, err := json.Marshal(sr.Ended())
	if err != nil {
		t.Fatal(err)
	}
	text := string(wire)
	for _, forbidden := range []string{
		promptSecret, completionSecret, "gen_ai.input.messages", "gen_ai.output.messages",
		"gen_ai.system_instructions", "gen_ai.tool.call.arguments", "gen_ai.tool.call.result",
		"gen_ai.prompt", "gen_ai.completion",
	} {
		if strings.Contains(text, forbidden) {
			t.Errorf("forbidden Tier 2/3 content reached spans: %q", forbidden)
		}
	}
}

func TestCoordinationAdmissionTelemetryContainsOnlyCountsAndDigest(t *testing.T) {
	sr := withRecorder(t)
	CoordinationAdmission(context.Background(), "sha256:abc", 7, 9000, 3, 4096, 4, 6000)

	spans := sr.Ended()
	if len(spans) != 1 || len(spans[0].Events()) != 1 {
		t.Fatalf("spans/events = %d/%d", len(spans), len(spans[0].Events()))
	}
	got := spanAttributes(spans[0].Events()[0].Attributes)
	want := map[string]string{
		"coordination.content_digest": "sha256:abc",
		"coordination.input_items":    "7",
		"coordination.input_bytes":    "9000",
		"coordination.admitted_items": "3",
		"coordination.rendered_bytes": "4096",
		"coordination.omitted_items":  "4",
		"coordination.omitted_bytes":  "6000",
	}
	if len(got) != len(want) {
		t.Fatalf("telemetry attributes = %v", got)
	}
	for key, value := range want {
		if got[key] != value {
			t.Errorf("%s = %q, want %q", key, got[key], value)
		}
	}
}

func TestGenAIUnknownCostIsNotSerializedAsZero(t *testing.T) {
	sr := withRecorder(t)
	ctx, endTurn := StartGenAITurn(context.Background(), GenAITurn{})
	_, endCall := StartGenAICall(ctx, GenAICall{Operation: "chat"})
	endCall(GenAICallResult{PricingKnown: false}, nil)
	endTurn(nil)

	got := spanAttributes(sr.Ended()[0].Attributes())
	if got["bashy.gen_ai.pricing_known"] != "false" {
		t.Fatalf("pricing_known = %q, want false", got["bashy.gen_ai.pricing_known"])
	}
	if _, exists := got["bashy.gen_ai.cost.usd"]; exists {
		t.Fatal("unknown cost was emitted as a numeric value")
	}
	if got["bashy.execution.venue"] != UnknownVenue {
		t.Fatalf("unknown venue = %q, want %q", got["bashy.execution.venue"], UnknownVenue)
	}
}

func spanAttributes(attrs []attribute.KeyValue) map[string]string {
	out := make(map[string]string, len(attrs))
	for _, kv := range attrs {
		out[string(kv.Key)] = kv.Value.Emit()
	}
	return out
}
