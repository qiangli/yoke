# LLM Budget Meter

`pkg/llmbudget` is the local-first client-side meter and gate for LLM calls.
It is brand-neutral and reads model economics from the fleet catalog: `kind`
classifies the authentication lane, `billing` classifies the payment lane, and
`cost_micro` gives the local cost weight for metered budget decisions.

The gate has two lanes:

- API-key models (`kind: api`, `billing: metered`) are governed by estimated USD
  cost, total budget caps, per-provider quota caps, and provider token buckets.
- Subscription models (`kind: subscription`, or flat-billed API models such as
  GLM) are governed by plan usage: daily/weekly token ceilings, daily/weekly
  request ceilings, near-limit exhaustion signals, and provider token buckets.

`Check(model, estTokens)` returns one of:

- `Allow`
- `Downgrade(model)`
- `Queue(delay)`
- `Block`
- `RouteAlt(model)`

Every `Downgrade` and `RouteAlt` destination is checked again. A subscription
exhaustion route may use another subscription normally; an API-key destination
requires the explicit `--allow-premium` / `BASHY_ALLOW_PREMIUM=1` authorization.

`Record(model, promptTokens, completionTokens, costUSD)` updates local model,
provider, and plan counters after a response. Model/provider cost has day and
week windows plus an all-time compatibility total; subscription plans carry
token/request day and week windows. Bashy records estimated usage for external
agent CLIs; first-party clients such as ycode should call `Check` before the HTTP
request and `Record` with provider-reported token counts and actual call cost
from the same interceptor that emits their LLM OTel span.

State defaults to `~/.bashy/llm-budget.json`. Override with
`BASHY_LLM_BUDGET_STATE`.

Useful env knobs:

- `BASHY_LLM_BUDGET_DAILY_USD`
- `BASHY_LLM_PROVIDER_<PROVIDER>_DAILY_USD`
- `BASHY_LLM_PROVIDER_<PROVIDER>_RATE_TOKENS`
- `BASHY_LLM_PROVIDER_<PROVIDER>_RATE_PER`
- `BASHY_LLM_MODEL_<MODEL>_DAILY_TOKENS`
- `BASHY_LLM_MODEL_<MODEL>_WEEKLY_TOKENS`
- `BASHY_LLM_MODEL_<MODEL>_DAILY_REQUESTS`
- `BASHY_LLM_MODEL_<MODEL>_WEEKLY_REQUESTS`
- `BASHY_LLM_DOWNGRADE_<MODEL>`
- `BASHY_LLM_ROUTE_ALT_<MODEL>`
- `BASHY_ALLOW_PREMIUM=1` or CLI `--allow-premium`

`CheckContext(ctx, model, estTokens)` attaches a bind to the LLM call span;
`RecordContext` is available for the matching post-response interceptor. This
is the integration point used by a first-party client such as ycode: call Check
immediately before its HTTP request, and Record immediately after it from the
same interceptor that emits the LLM OTel span. Bashy's chat, invoke, and meet
paths already use that shared gate (meet delegates to chat).

Missing price or subscription-limit metadata fails open with a warning; it never silently
blocks. Every budget bind, provider quota bind, rate-limit bind, and
subscription near-limit bind emits `telemetry.BoundHit`, so `bashy otel bounds`
shows where the meter intervened.
