---
id: 8824e6c76f8f
kind: feature
title: 'S524.7d yoke: --account for DO/AWS infra, cloudbox (secret/fleet), LLM delegate'
seq: 2913
status: done
priority: p0
labels:
    - accounts
created: 2026-10-29T11:11:43.476781Z
weave: 695
assignee: claude-opus5
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T12:30:09.054342Z
resolution: fixed
closed_by: claude-opus5-s
---

Part of S524.7 (umbrella story 607ba5d10846, Sprint 524), slice D (yoke): `--account <alias|id>` for DigitalOcean, AWS, Cloudbox and LLM operations. Host-login (bashy connect) is done; follow its pattern (bashy/internal/agentos/connect.go accountTarget: resolve per invocation, structured exit-2 errors, credential only for this call). Base: yoke main fb9285ce. Contract (private, read-only): ../docs/bashy-account-plan.md "CLI and app" + S524.7.
Rules for every family: resolve via pkg/accounts Store.Resolve/adapter Select for the operation; the credential is materialized ONLY for this invocation (in-process override or child-only env), never os.Setenv on the bashy process, never written anywhere, never printed (errors carry codes only); no fallback to another identity; no change to global provider state (no doctl context, no AWS_PROFILE export, no cloudbox profile switch); explicit conflicting flags fail BEFORE any network call/side effect with code account_conflict; unsupported category for a verb fails with account_unsupported before execution; without --account behaviour is byte-identical (tests).
0. Shared helper in pkg/accounts (e.g. materialize.go): Materialize(ctx, Selection) -> per-call credential (secret: via pkg/secrets Config.Resolve()+Client.Get; env: reads the named env; profile: for cloudbox reads that profile's token from the outpost profile dir READ-ONLY; for AWS profile: use external/aws ResolveProfileKeys). Also fix: DO adapter discovers env DIGITALOCEAN_TOKEN but fleet uses DIGITALOCEAN_ACCESS_TOKEN — accept both.
1. DigitalOcean: `bashy infra provision|list|inspect|destroy` (pkg/fleet/infra_provision.go, infra.go, infra_lifecycle.go) and direct-client verbs `infra ip|firewall|harden|trash` (resolveDOClient, pkg/fleet/infra_ip.go). Seams: resolveProviderCredential (infra_builtin.go:253) and resolveDOClient — add a per-invocation override (context value or explicit param). Conflict: --credential-ref. NOTE `infra provision --token` is the cloudbox bearer and `--profile` is a satellite workload profile — not DO/AWS conflicts.
2. AWS (Lightsail via infra; budget guard ProvisioningAllowed): per-invocation keys from the account's profile via ResolveProfileKeys into the builtin `open` closure (infra_builtin.go:151) and the budget guard. Conflicts: --credential-ref; env AWS_ACCESS_KEY_ID/AWS_PROFILE set together with --account -> account_conflict.
3. Cloudbox: fleet.ResolveCloud(urlOverride, tokenOverride) (pkg/fleet/cloudbox.go:65) and `bashy secret` secrets.Config — `--account` supplies URL (connection endpoint) + token (materialized). Conflicts: --url, --token, --master. `bashy tessaro` / `login` (exec outpost) -> account_unsupported.
4. LLM: `bashy delegate --account <id>` maps to the agent binding linked to that account (Account.Links.Agents; exactly one usable binding else account_ambiguous) and behaves as `--agent <binding>`. Conflicts: --agent, --self, --model.
Register `--account` in the yoke atlas flag schema for each touched verb (bashy MCP/help pick it up). Tests: per family, a fake transport/recorder proving the selected identity is used (two DO teams, two AWS profiles, two cloudbox endpoints, two LLM accounts -> each call uses its own), concurrency test with two different accounts in parallel goroutines never crossing, conflict + unsupported fail before side effects, no-flag unchanged, secret sentinel (pkg/accounts/accountstest) absent from all output/errors.
Gates: go test -race of every touched package (pkg/accounts/..., pkg/fleet/..., pkg/chat/..., pkg/secrets/..., external/aws/...; pkg/provider is a nested module — test it by cd into it if touched), go vet same, go build ./..., GOOS=windows go build ./... . Do not edit pkg/webconsole, pkg/src, pkg/shape. No per-run GOCACHE (Dragon disk). Placeholder names only (owl/otter/alice/bob/example.com). Every commit: Sprint: #524, Story: <this yoke story #N>, Story-ID: <full 12-hex>, Agent: <your binding>. Do not push.
