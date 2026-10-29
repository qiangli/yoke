---
id: 97d2d5d75770
kind: bug
title: 'S524.7e yoke: adapters resolve secret: refs; refresh adopts identity into manual record'
seq: 2914
status: done
priority: p0
labels:
    - accounts
created: 2026-10-29T12:32:10.760911Z
weave: 696
assignee: claude-sonnet5.5
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T12:40:06.54634Z
resolution: fixed
closed_by: claude-sonnet5.5-t
---

Part of S524.7 (umbrella 607ba5d10846) / S524.4 (d87adfb6395f), Sprint 524: two gaps found in the live --account check. Base: yoke main 0b2a7b67.
1. pkg/accounts/digitalocean.go client() (and the AWS adapter's equivalent) accept only env: refs, so an account added the recommended way (`account add do-team --kind digitalocean --credential secret:DO_TEAM_TOKEN`) can never refresh/verify and therefore can never be selected. Resolve the credential through the shared Materialize helper (pkg/accounts/materialize.go): secret:, env:, and for AWS profile:. Credential only in memory for that call.
2. `account refresh <manual alias>` verifies the identity but records it as a NEW unaliased observed record (keyed by team uuid / AWS account+profile) and leaves the manual record unresolved, so `--account <alias>` stays account_unsupported. When refresh/Verify of a MANUAL record proves an identity, adopt it into that same record: set ProviderAccountID, re-key the record to the identity key (keep the stable ID if possible, else keep alias/overrides and drop the manual key), status resolved — no duplicate record. If an observed record with that identity already exists, merge the manual alias/overrides into it and remove the manual one (no data loss). Tests: manual DO + AWS accounts with secret: refs (fake transports + fake secrets client) -> refresh -> exactly one resolved record per identity with the alias kept -> `infra list --account <alias>` selectable; two teams stay separate; secret sentinel absent.
Gates: go test -race ./pkg/accounts/... ./pkg/fleet/ -run 'Account' (the full fleet race suite is too heavy for Dragon; run pkg/fleet non-race), go vet, go build ./..., GOOS=windows go build ./pkg/... . No per-run GOCACHE. Every commit: Sprint: #524, Story: <this yoke story #N>, Story-ID: <full 12-hex>, Agent: <your binding>.
