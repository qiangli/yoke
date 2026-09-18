package chat

import (
	"context"

	"github.com/qiangli/yoke/pkg/llmbudget"
	"github.com/qiangli/yoke/pkg/telemetry"
)

const subprocessHarnessCoverage = "subprocess_harness_turn"

func startGenAIObservation(ctx context.Context, l Launch) (context.Context, func(telemetry.GenAICallResult, error)) {
	venue := telemetry.GenAIVenue(ctx)
	turnCtx, endTurn := telemetry.StartGenAITurn(ctx, telemetry.GenAITurn{
		Venue:            venue,
		CoverageScope:    subprocessHarnessCoverage,
		CoverageComplete: false,
	})
	callCtx, endCall := telemetry.StartGenAICall(turnCtx, telemetry.GenAICall{
		Operation:        "chat",
		Provider:         genAIProvider(l),
		RequestModel:     genAIRequestModel(l),
		Venue:            venue,
		CoverageScope:    subprocessHarnessCoverage,
		CoverageComplete: false,
	})
	return callCtx, func(result telemetry.GenAICallResult, err error) {
		endCall(result, err)
		endTurn(err)
	}
}

func endGenAIObservation(end func(telemetry.GenAICallResult, error), l Launch, prompt, output, stopReason string, err error) {
	inputTokens, outputTokens := estimateTokens(prompt), estimateTokens(output)
	result := telemetry.GenAICallResult{
		InputTokens:  &inputTokens,
		OutputTokens: &outputTokens,
		UsageSource:  "harness_estimate",
	}
	if stopReason != "" {
		result.FinishReasons = []string{stopReason}
	}
	if cost, known := llmbudget.EstimatedCostUSD(l.ModelName, inputTokens+outputTokens); known {
		result.CostUSD = &cost
		result.PricingKnown = true
	}
	end(result, err)
}

func genAIProvider(l Launch) string {
	if l.ModelName == "" {
		return ""
	}
	if model, ok := newCatalog().Model(l.ModelName); ok {
		return model.Provider
	}
	return ""
}

func genAIRequestModel(l Launch) string {
	if l.Model != "" {
		return l.Model
	}
	return l.ModelName
}
