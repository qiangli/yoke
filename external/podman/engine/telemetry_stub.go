package engine

import "context"

// Minimal exec-span stub replacing ycode's internal/telemetry/otel. The relocated
// container engine does not carry ycode's unified OTEL exec signal; the embedding
// host (bashy/outpost) owns process-level telemetry. Metrics still flow through
// the package-local otel.go (RecordContainerExec et al.).
type execScopeT string

const execScopeContainer execScopeT = "container"

// startExecSpan returns the context unchanged and a no-op finisher.
func startExecSpan(ctx context.Context, _ execScopeT, _ string, _ []string) (context.Context, func(int, error)) {
	return ctx, func(int, error) {}
}
