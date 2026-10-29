---
id: a1a24d7ee440
kind: feature
title: 'S524.4 yoke: DigitalOcean and AWS account adapters'
seq: 2902
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T08:45:07.733859Z
weave: 685
assignee: codex-gpt6-astra
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T09:33:24.234045Z
resolution: fixed
closed_by: codex-gpt6-astra-i
---

Part of S524.4 (umbrella story d87adfb6395f, Sprint 524): DigitalOcean and AWS accounts, resources and billing. Design contract (private, read-only; never copy its text into OSS files/commits): ../docs/bashy-account-plan.md and docs/sprint-524-master-execution-plan.md. Build ON yoke/pkg/accounts (S524.1, on yoke main): use its schema, store, Metric type + refusal helpers, adapter registry (one registration line), and the accountstest secret sentinel in every test. OSS hygiene: placeholder hosts/users only (owl, otter, alice, bob, example.com); no real hostnames/emails/IPs; no cloudbox internals. Do NOT edit pkg/webconsole, pkg/src, pkg/shape (another sprint). Gates: focused go test of touched packages, go build ./..., go vet of touched packages, GOOS=windows go build ./... . Do not push.
Facts: yoke/pkg/provider/digitalocean takes Config.Token/ClientConfig.Token (Bearer); has /v2/account (validate op, digitalocean.go:234, offers.go:177), ListProjects/GetProject (hardening.go:486/497), ListDropletsByTag (trash.go:143), GetDroplet; NO ssh-key list, balance or team call yet — add small read-only client calls for GET /v2/account (team uuid/name, email, status, droplet_limit), /v2/account/keys, /v2/customers/my/balance, /v2/droplets (paged). Fake: digitalocean/fakedo (extend it). AWS: yoke/pkg/provider/aws AccountID via STS (sts.go:155), DescribeBudget/MonthToDateSpend (budgets.go), MonthToDateUSD/BudgetPreflight (budget.go); profiles via yoke/external/aws/auth.go (ResolveCredentials, defaultSharedFile). Reuse Sprint 522 code; do not duplicate it. NEVER use doctl global context or `bashy doctl`: tokens are injected explicitly per call.
Scope: digitalocean and aws adapters registered in pkg/accounts: identity (DO team uuid as provider account id; AWS account id + profile/role), resources (projects, droplets with ssh-key associations via fingerprints, regions), usage/billing Metrics (USD, provider-reported, window; balance; budget) using the shared Metric type — unavailable stays nil. Credential refs only (secret:<name>, env:<NAME>, profile:<aws profile>). "Other managed services" = generic adapter hook + manual account add only; unsupported enrichment returns ErrUnsupported explicitly.
Acceptance: two DO teams and two AWS profiles stay separate; identity/resource attribution correct; partial access (403 on balance) = partial coverage not zero; monetary units and sources correct; secret sentinel absent everywhere. Fake transports only; no live calls, no create/delete. Trailers: Sprint: #524, Story: S524.4, Story-ID: d87adfb6395f.
