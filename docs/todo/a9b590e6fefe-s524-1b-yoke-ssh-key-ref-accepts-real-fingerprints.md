---
id: a9b590e6fefe
kind: bug
title: 'S524.1b yoke: ssh-key ref accepts real fingerprints; text-mode errors printed'
seq: 2912
status: done
priority: p1
labels:
    - accounts
created: 2026-10-29T10:49:18.337114Z
weave: 694
assignee: agy-gemini3.8-flash
sprint: 524
sprint_id: 9c8c2767-6917-5a65-bb86-481e65ce919a
sprint_title: Bashy account inventory, operational selection, and app summary
closed: 2026-10-29T11:02:29.283492Z
resolution: fixed
closed_by: agy-gemini3.8-flash-r
---

Part of S524.1/S524.2 (umbrella stories 7c676f1d4038 / 0f9695efe8d8, Sprint 524): two bugs found in the live host-login check. Base: yoke main.
1. pkg/accounts/types.go refPattern: `ssh-key:` must accept real OpenSSH fingerprints, e.g. `ssh-key:SHA256:oFTyb/wLPkeVpfBjq8i4XoY9zs4+AoWome8V4/m5jxg` (base64 incl. + / and optional = padding) and `ssh-key:MD5:aa:bb:...`. Today it rejects + / =. Tighten nothing else; table test with real-shaped SHA256 and MD5 fingerprints plus rejects (spaces, newlines, empty).
2. `account add` (and other mutating verbs) in TEXT mode exits 1 with NO message on error (the error is only in --json). Print the structured error (code + message) to stderr in text mode; exit code unchanged. Also make the duplicate-identity error name the existing alias/id (e.g. `identity already recorded as <alias> (<id>)`) — never a secret. Tests for both.
Gates: go test -race ./pkg/accounts/..., go vet ./pkg/accounts/..., go build ./..., GOOS=windows go build ./pkg/accounts/... . No per-run GOCACHE. Placeholder names only. Do not edit pkg/webconsole/src/shape. Trailers on every commit: Sprint: #524, Story: <this yoke story #N>, Story-ID: <its full 12-hex id>, Agent: <your binding>.
