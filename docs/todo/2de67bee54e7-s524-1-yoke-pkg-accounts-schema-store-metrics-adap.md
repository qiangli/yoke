---
id: 2de67bee54e7
kind: feature
title: 'S524.1 yoke: pkg/accounts schema, store, metrics, adapter interface'
seq: 2899
status: done
priority: p0
labels:
    - accounts
created: 2026-10-29T08:32:57.236187Z
weave: 680
assignee: codex-gpt6-sol
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T08:44:44.754351Z
resolution: fixed
closed_by: codex-gpt6-sol-d
---

Part of S524.1 (umbrella story 7c676f1d4038, Sprint 524). Create the new package yoke/pkg/accounts: the shared, host-local account inventory that later stories (CLI in bashy, Cloudbox/DO/AWS/LLM adapters, --account selection, webconsole panel) build on. Design contract (private, read-only, never copy its text into OSS files or commits): ../docs/bashy-account-plan.md and ../docs/sprint-524-master-execution-plan.md.

Scope (this story only, no CLI, no adapters beyond fakes, no webconsole):
1. Schema v1 (JSON, `schema_version` field): Account {ID stable local id, Alias unique editable, Category (cloudbox, satellite, digitalocean, aws, llm, host-login, other), Provider, ProviderAccountID, Name, Username/Email, AvatarURL, ProfileURL/DashboardURL, Team/Org/Project/Tenant, Role, Region, Environment, Lane (subscription|api|...), Connections []Connection, Resources []ResourceRef, SSHKeys []SSHKeyRef, Links (satellites/hosts/models/agents), Status (resolved|unresolved|error|suppressed), Provenance/Source, ObservedAt, LastError}. Connection {ID, Endpoint/Profile, CredentialRef (only a reference: `secret:<vault name>`, `ssh-key:<fingerprint>`, `env:<NAME>`, `profile:<name>`), Scopes, ExpiresAt, AuthMethod}. Satellite = linked instance, modelled explicitly (not a Connection). Host-login category: OS user, outpost endpoint host:port, auth method, credential ref; two users on the same host are two accounts; a rejected username is stored as unresolved and never merged.
2. Observations vs manual overrides are stored separately; the effective view = observations overlaid by user overrides; refresh/reconcile never clobbers overrides.
3. Reconciliation: Observe(adapter, []Observation) is idempotent and deterministic; match ONLY on (category, provider, provider account id / endpoint+user) — never merge by email, display name or plan. Unverifiable identities stay unresolved. `rm` = metadata suppression marker that blocks automatic rediscovery until explicit Restore; Remove refuses while referenced (bindings) unless the caller removes them first.
4. Store: $BASHY_HOME (fall back to ~/.bashy) /accounts/accounts.json (follow existing yoke conventions for BASHY_HOME resolution); dir 0700, file 0600, cross-process file lock (reuse an existing yoke lock helper if one fits; Windows must build), atomic write (temp + rename), schema version check (refuse newer major). Deterministic ordering in List.
5. Usage metric type (shared, used by later stories): Metric {Name, Value *float64 (nil = unavailable, never 0), Unit, Currency, Source, Classification (provider-reported|local-observation|estimate), ObservedAt, WindowStart/End or Period, Coverage (complete|partial|unknown), Limitations}. Helpers that REFUSE: summing different currencies/units, summing overlapping windows from the same source family, treating current-context tokens as cumulative. Tests for each refusal.
6. Adapter interface: capability flags (Discover, Verify, Resources, Usage, Billing, Select) + methods; a registry with one-line registration per adapter; unsupported capability returns a typed ErrUnsupported. Selection resolution type: Resolve(accountRef, operation) -> safe metadata (endpoint/profile/credential REF), never secret values.
7. Secret sentinel test helper (exported from an internal test-support package, e.g. pkg/accounts/accountstest): a canary secret value + AssertNoSecret(t, surfaces...) for stdout/JSON/errors/logs; use it in this story's tests. Records must reject any field that looks like a raw secret where a reference is expected (CredentialRef must parse as a reference).

Acceptance (focused, red/green first; run `go test ./pkg/accounts/...` and `go build ./...` and `go vet ./pkg/accounts/...` in yoke; GOOS=windows go build ./pkg/accounts/... too):
- concurrent writes from several goroutines AND two processes (helper-process test) lose nothing;
- repeatable reconciliation (same observations twice = byte-identical store); stable IDs across refresh;
- unresolved identities; no merging by email/name/plan; two DO teams / two AWS profiles / two host-login users on one host stay separate;
- user overrides survive refresh; suppression blocks rediscovery until Restore; referenced Remove refuses;
- secret sentinel never appears in records, JSON or errors; raw secret in CredentialRef rejected.
OSS hygiene: placeholder hosts/users only (owl, otter, alice, bob, example.com); no real hostnames, emails or IPs; no cloudbox internals. Commit with trailers Sprint: #524, Story: S524.1, Story-ID: 7c676f1d4038 (and this yoke story's id). Do not edit pkg/webconsole, pkg/src or pkg/shape (another sprint owns them).
