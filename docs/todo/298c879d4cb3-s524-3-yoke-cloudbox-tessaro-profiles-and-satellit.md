---
id: 298c879d4cb3
kind: feature
title: 'S524.3 yoke: Cloudbox/Tessaro profiles and satellites adapter'
seq: 2901
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T08:45:07.47603Z
weave: 682
assignee: claude-sonnet5.5
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T08:50:38.657779Z
resolution: fixed
closed_by: claude-sonnet5.5-f
---

Part of S524.3 (umbrella story e9b678ee1a02, Sprint 524): Cloudbox/Tessaro profiles and multiple satellites as accounts. Design contract (private, read-only; never copy its text into OSS files/commits): ../docs/bashy-account-plan.md and docs/sprint-524-master-execution-plan.md. Build ON yoke/pkg/accounts (S524.1, on yoke main): use its schema, store, Metric type + refusal helpers, adapter registry (one registration line), and the accountstest secret sentinel in every test. OSS hygiene: placeholder hosts/users only (owl, otter, alice, bob, example.com); no real hostnames/emails/IPs; no cloudbox internals. Do NOT edit pkg/webconsole, pkg/src, pkg/shape (another sprint). Gates: focused go test of touched packages, go build ./..., go vet of touched packages, GOOS=windows go build ./... . Do not push.
Facts: outpost profiles are managed by bashy/examples/outpost-profile/outpost-profile.bsh under ${XDG_CONFIG_HOME:-$HOME/.config}/matrix/: agent.json (active pairing), profiles/<name>/agent.json + one-line server and host sidecars, .active = current profile. Public JSON surfaces: `outpost status --json` (agent_name; works with daemon down), `outpost config show --json`, `outpost remote list --json` (~/.config/outpost/remotes/*.json). bashy runs outpost via yoke/external/meshagent (Resolve/Exec) — exec, never link; yoke/pkg/principal/host.go:124 reads agent_name without importing outpost (follow that pattern).
Scope: a cloudbox adapter in yoke (e.g. pkg/accounts/adapters/cloudbox) with Discover: read the profile files READ-ONLY (never copy tokens — record only a credential REF like profile:<name>), plus outpost status/config JSON via an injectable exec seam; one account per (server endpoint, user identity), connections per profile, satellites (linked hosts/instances) modelled as Satellite links; several endpoints and users on one host stay separate; endpoint-scoped credential binding. Cached/last-good snapshot with LastError when unreachable/403/expired.
Acceptance: fixtures with 2 endpoints x 2 users x several satellites: separate selectable connections, no credential crossover, correct endpoint scoping, useful cached output when unreachable/forbidden/expired; secret sentinel (token value placed in fixture agent.json) never appears in records/JSON/errors. Then a scoped read-only live Discover against this host's real profile dir is run by the manager later (not by you). Trailers: Sprint: #524, Story: S524.3, Story-ID: e9b678ee1a02.
