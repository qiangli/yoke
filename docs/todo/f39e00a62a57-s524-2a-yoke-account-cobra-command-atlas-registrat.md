---
id: f39e00a62a57
kind: feature
title: 'S524.2a yoke: account cobra command + atlas registration'
seq: 2900
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T08:45:07.241265Z
weave: 681
assignee: codex-gpt5.6-sol
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T08:58:42.665425Z
resolution: fixed
closed_by: codex-gpt5.6-sol-e
---

Part of S524.2 (umbrella story 0f9695efe8d8, Sprint 524), slice A (yoke only): the `account` cobra command + atlas registration/effects. The bashy dispatch wiring (agentos.go shim/dispatch, cobra_schema, commands, sections, skills doc) is a LATER bashy slice after the yoke pin bump: do NOT edit bashy in this run; the facts below about bashy files are for context only. Design contract (private, read-only; never copy its text into OSS files/commits): ../docs/bashy-account-plan.md and docs/sprint-524-master-execution-plan.md. Build ON yoke/pkg/accounts (S524.1, on yoke main): use its schema, store, Metric type + refusal helpers, adapter registry (one registration line), and the accountstest secret sentinel in every test. OSS hygiene: placeholder hosts/users only (owl, otter, alice, bob, example.com); no real hostnames/emails/IPs; no cloudbox internals. Do NOT edit pkg/webconsole, pkg/src, pkg/shape (another sprint). Gates: focused go test of touched packages, go build ./..., go vet of touched packages, GOOS=windows go build ./... . Do not push.
Wiring facts: add "account" to alwaysShimVerbs (bashy/internal/agentos/agentos.go:157) and a dispatch case like secret's (agentos.go:1375 -> NewXCmd, setCobraArgs, dispatchExit); flag schema cobra_schema.go:146; verbSynopsis commands.go:593; help section commands_sections.go:60; atlas addVerb (yoke/pkg/atlas/atlas.go:1447) + effects eff/EffCred/rev/stab (:1607/:1647/:1791/:1864) + packages.go:203 (yoke side, same story); MCP is automatic from atlas (mcp_verbs.go:40); doc under skills/bashy/commands/. Put the cobra command itself in yoke (pkg/accounts/cmd or similar) like secrets.NewSecretsCmd, so bashy only dispatches.
Verbs: list; show ID (id prefix shorthand + alias); add ALIAS --kind <category> [--user --endpoint --provider --team ... --credential secret:<name>|ssh-key:<fp>|env:<NAME>|profile:<name>] (reject raw secrets); set ID --set k=v/--unset k; rm ID (refuses referenced; metadata only; suppresses rediscovery) + restore ID; discover [--dry-run]; refresh ID|--all; usage ID --period day|week|month; resources ID; resolve ID --for OPERATION; schema. --json (versioned envelope), --fields, filters (--category --provider --team --status), deterministic order, structured errors, noninteractive. Cached reads never touch the network.
Acceptance: CLI fixture tests for every verb incl. invalid fields, missing accounts, filters, dry-run, deletion guard, host-login add with --credential secret:<n>; secret sentinel absent from stdout/JSON/errors; atlas/dispatch tests pass. Trailers: Sprint: #524, Story: S524.2, Story-ID: 0f9695efe8d8.
