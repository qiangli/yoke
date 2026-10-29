---
id: 6324f383d8fc
kind: feature
title: 'S524.6 yoke: automatic account collection hooks + manual/observed identity merge'
seq: 2909
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T09:39:03.177573Z
weave: 692
assignee: claude-opus5
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T10:22:14.106508Z
resolution: fixed
closed_by: claude-opus5-p
---

Part of S524.6 (umbrella story 763ca15f5b1c, Sprint 524): automatic account collection during setup and operations (yoke + a small bashy hook slice later). Builds on yoke/pkg/accounts (store Observe/Add, adapters cloudbox/llm/digitalocean/aws, refresh snapshots from S524.2c).
1. Store fix found in review: a manually added account (Store.Add, key "manual\x00alias") and a later observation of the SAME identity (e.g. host-login endpoint+user, or provider account id) create TWO records. Make Add compute the identity key when the record is identifiable (same identity() rules) so a later Observe updates that record (manual alias/labels kept as overrides); unresolvable manual records keep the manual key. Tests: manual host-login then observed connect = one record, alias preserved.
2. Observation hook API in pkg/accounts: ObserveSuccess(ctx, category, facts) — best-effort, bounded (timeout), never returns an error to the caller's exit path (log at debug only), records proven identity/resource relationships (e.g. host-login endpoint+user+auth method after a successful connect; DO team after a successful provider call; LLM account after a run). Concurrency-safe (store lock), deduplicated, respects suppression (rm'd accounts are not re-added), no secrets.
3. Wire the hook (yoke side) into: pkg/provider/digitalocean + aws successful calls (team/account id already known), llmbudget/quotaobs observation path, and outpost-profile/login completion where yoke owns the code. The bashy `connect` hook is done in the S524.7 bashy slice — export what it needs.
4. Tests: recording failure (read-only store dir) does not change the caller's result; retries/duplicates idempotent; concurrent operations; logout/expiry transitions mark status, keep last good; secret sentinel.
Disk is low: no per-run GOCACHE copies. Gates: go test -race ./pkg/accounts/... plus touched provider/llmbudget packages (pkg/provider is a nested module? test by import path), vet, go build ./..., GOOS=windows build. Do not edit pkg/webconsole/src/shape. Trailers: Sprint: #524, Story: S524.6, Story-ID: 763ca15f5b1c (plus this yoke story id).
