---
id: 0da1d79fbabd
kind: feature
title: 'S524.5 yoke: LLM account adapter (quota, tokens, cost)'
seq: 2903
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T08:45:07.953535Z
weave: 684
assignee: muse-conductor
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T08:55:46.880891Z
resolution: fixed
closed_by: muse-conductor-h
---

Part of S524.5 (umbrella story bfcd849ca51d, Sprint 524): LLM accounts with quota, tokens and cost. Design contract (private, read-only; never copy its text into OSS files/commits): ../docs/bashy-account-plan.md and docs/sprint-524-master-execution-plan.md. Build ON yoke/pkg/accounts (S524.1, on yoke main): use its schema, store, Metric type + refusal helpers, adapter registry (one registration line), and the accountstest secret sentinel in every test. OSS hygiene: placeholder hosts/users only (owl, otter, alice, bob, example.com); no real hostnames/emails/IPs; no cloudbox internals. Do NOT edit pkg/webconsole, pkg/src, pkg/shape (another sprint). Gates: focused go test of touched packages, go build ./..., go vet of touched packages, GOOS=windows go build ./... . Do not push.
Facts: yoke/pkg/llmbudget: Quota(ctx, QuotaOptions{All,Provider,Refresh,Now}) []QuotaRow (quota.go:18-58: account, provider, plan, window, used %, reset, balance, source, stale/headroom); StatusAll (status.go:51), Forecast (forecast.go:441), CollectReport -> AccountReport/SourceConfig (report.go:15, report_types.go); yoke/pkg/quotaobs Observation (observe.go:91), Read/ReadDelta. bashy overlays seats in bashy/internal/agentos/models_quota.go:182 (overlayQuotaSeats) — the seat data source for agent bindings.
Scope: an llm adapter in pkg/accounts: one account per verified (provider, account/plan identity); distinguish provider/account/pool/lane and subscription vs API; link models/agent bindings; quota windows with resets, input/output/cache tokens where sources have them, balances and cost as Metrics with classification (provider-reported vs local-observation vs estimate) and coverage. Cached reads only (Refresh=false); never trigger probes or inference; never scrape harness credentials.
Acceptance: shared verified accounts aggregate once; unrelated seats with the same plan/email do not merge; missing/stale/unsupported is nil not 0; tests prevent double counting overlapping sources, cross-currency sums and context tokens as cumulative usage; estimates carry limitations; secret sentinel absent. Trailers: Sprint: #524, Story: S524.5, Story-ID: bfcd849ca51d.
Required fix from S524.1 review: pkg/accounts SumMetrics does not enforce Classification/Coverage — tighten it in this story: refuse (typed error) summing mixed classifications (estimate + provider-reported) and propagate Unknown/partial coverage to the result; tests for both.
