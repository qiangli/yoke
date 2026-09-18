# Agent Run Observability (OpenTelemetry)

**Status:** P2  
**Scope:** Brand-neutral, OSS

## Problem

Agent runs execute as headless, isolated jobs. When a run fails, succeeds, or hangs, the reason is often opaque without deep-diving into execution logs. Furthermore, the individual steps an agent takes—such as harness interactions, gateway calls (`llm.request`), or sub-agent invocations—lack a unified trace context. This makes it impossible to query distributed tracing systems to understand an entire agent run from end to end.

## Goal

Provide a unified, single-trace view of an entire agent run by establishing a "trace root" at the orchestrator (`weave start`) level, and threading that context down to all child processes (agents, gateways, harnesses).

## Design

1. **`weave.run` Span (Root Span)**
   Every `weave start` execution creates a root `weave.run` span. This span encompasses the entire lifecycle of the agent run for a specific issue.
   
2. **Context Propagation**
   The trace context (Traceparent/Tracestate) of the `weave.run` span is injected into the child environment (`weaveChildEnv`, the `weave start` launch environment path) using standard W3C trace context format (`TRACEPARENT` env var).

3. **Child Spans**
   Any traceparent-aware agent, harness, or gateway will extract the trace context from the environment. Bashy's model-call path emits an internal `gen_ai.turn` span with a `chat {model}` client span beneath it. Those spans automatically nest under an active orchestrator span, creating a single trace tree.

4. **Outcome Attributes**
   The `weave.run` span is enriched with terminal evidence at the end of the run. It captures:
   - `agent` and `nick`: The agent owner identifier.
   - `band`: The capability band of the agent.
   - `tool:model`: The canonical tool and model string.
   - `issue`: The specific issue ID being worked on.
   - `outcome`: The terminal queue state recorded for the run.
   - `converged`: A boolean indicating if the run resulted in a `submitted`, `merged`, or `verified` state.
   - `gate_result`: The exit code of the verify/gate step.
   - `turns`: The total number of conversational turns the agent took.
   - `duration`: Total execution time of the run.

5. **Metrics**
   A histogram metric `fleet.run.turns` is emitted upon completion, tagged by `agent` and `band`. This enables fleet-level visibility into agent efficiency and loops.

6. **Configuration & No-Op**
   Telemetry relies on standard OpenTelemetry environment variables. With no OTLP endpoint, spans go to the bounded owner-only local spool; `OTEL_TRACES_EXPORTER=none` selects the pure no-op path. Network export is opt-in through an endpoint or explicit exporter. Routine startup status appears on an interactive stderr only (or with `BASHY_TELEMETRY_NOTICE=1` / `BASHY_TELEMETRY_DEBUG=1`); `BASHY_TELEMETRY_QUIET` suppresses it in every mode. Initialization failures always remain visible. The global propagator remains installed in either mode so wire context survives hops.

## GenAI Tier 1 coverage

The model-call emitter targets OpenTelemetry GenAI semantic conventions schema
1.42.0. It records structure and accounting metadata only: operation, provider,
requested model, token counts, finish reason when the harness reports one, span
duration, and bashy-private cost, pricing-known, usage-source, venue, and
coverage fields. It has no API for prompts, completions, system instructions,
tool arguments, or tool results.

Bashy's `chat.Invoke` and ACP prompt paths launch another vendor's executable.
The actual provider calls happen inside that subprocess, so these spans cover
the harness-observed turn, not every internal retry or tool-loop model call.
Every such span therefore carries
`bashy.gen_ai.coverage.scope=subprocess_harness_turn` and
`bashy.gen_ai.coverage.complete=false`. ACP supplies a structured finish reason;
other subprocess paths leave it unknown. Token usage is marked
`bashy.gen_ai.usage.source=harness_estimate`.

Claude, Codex, OpenCode, agy, and other vendor binaries are not claimed as
directly instrumented. Provider-reported usage from their event or summary
surfaces is not captured here. Fleet panels must filter or group by the coverage
fields rather than treating these observations as a complete provider-call
inventory.

The execution venue is supplied through context with
`telemetry.WithGenAIVenue`. When the host does not provide one, the emitter
writes `bashy.execution.venue=UNKNOWN`; it never invents a tier.

## Implementation Notes

- `pkg/weave` starts `weave.run` immediately before the agent process launch and ends it after terminal evidence is recorded.
- `weaveChildEnv` remains the child environment choke point for `weave start`; the active span context is injected into that environment as standard W3C `TRACEPARENT`/`TRACESTATE` entries.
- The `weave.run` span carries `agent`, `nick`, `band`, `tool:model`, `issue`, `outcome`, `converged`, `gate_result`, `turns`, and `duration`.
- `fleet.run.turns` is recorded as a histogram with `agent` and `band` attributes when the launched tool reports `num_turns`.
- Tests verify that a traceparent-aware mock agent creates an `agent.turn` span under the same trace and parent span, and that `OTEL_TRACES_EXPORTER=none` selects the pure no-op path.
