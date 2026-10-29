---
id: 89439651f273
kind: bug
title: 'S524.7f yoke: account refresh CLI uses RefreshOne/Adopt (remove duplicate refresh path)'
seq: 2915
status: done
priority: p0
labels:
    - accounts
created: 2026-10-29T12:41:24.90169Z
weave: 697
assignee: claude-sonnet5.5
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T12:47:11.689232Z
resolution: fixed
closed_by: claude-sonnet5.5-u
---

Part of S524.7 (umbrella 607ba5d10846), Sprint 524: live check after S524.7e found the CLI still does not adopt. Base: yoke main de0d0b2a.
`bashy account refresh <alias|--all>` goes through pkg/accounts/cmd.go commandState.refreshOne (cmd.go:613), a second refresh implementation that never calls the fixed library path RefreshOne/RefreshAll (refresh.go:38/118, which calls Store.Adopt). Live result: manual `do-eph` (secret: ref) refreshes OK but stays unresolved and a separate unaliased resolved record appears; `infra list --account do-eph` -> account_unsupported.
Fix: make refreshCmd (single + --all) call accounts.RefreshOne / RefreshAll (pass the command's store + registry) and delete the duplicated refresh logic in cmd.go (keep output/JSON envelope shape and snapshot-age printing). Add a CLI-level test through NewAccountCmd(WithStore, WithRegistry) with a fake DO-like adapter: `account add x --kind digitalocean --credential secret:FAKE` -> `account refresh x` -> `account list --json` has exactly ONE record for that identity, alias x, status resolved, provider_account_id set; then Store.Resolve(x, op) succeeds. Same for --all with two accounts (two identities stay separate). Secret sentinel absent.
Gates: go test -race ./pkg/accounts/..., go test ./pkg/fleet/ -run Account, go vet ./pkg/accounts/..., go build ./..., GOOS=windows go build ./pkg/... . No per-run GOCACHE. Commits: Sprint: #524, Story: <this yoke story #N>, Story-ID: <full 12-hex>, Agent: <your binding>.
