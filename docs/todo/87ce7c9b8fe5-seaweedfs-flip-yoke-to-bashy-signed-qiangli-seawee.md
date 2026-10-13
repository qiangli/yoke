---
id: 87ce7c9b8fe5
kind: task
title: 'seaweedfs: flip yoke to bashy-signed qiangli/seaweedfs bashy-4.48-1; telemetry off + loopback on every start path'
seq: 2008
status: done
priority: p1
labels:
    - supply-chain
created: 2026-10-13T01:20:37.313241Z
weave: 222
assignee: muse-spark1.2
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-13T01:30:18.739498Z
closed_by: s434-manager
---

Flip yoke external/seaweedfs (today: binmgr.ResolveGitHub seaweedfs/seaweedfs, DefaultVersion "latest" = upstream archive) to the bashy-signed fork release, exactly like the Victoria flip (yoke 23a0f331, external/otel/stack/execstore.go victoriaTool: binmgr.Tool + Sigstore identity/issuer, raw executables, per-asset sha256 read from the release SHA256SUMS - download it, do not trust the API listing).
Release: https://github.com/qiangli/seaweedfs/releases/tag/bashy-4.48-1 (qiangli/seaweedfs run 38330302167 green: go1.26.9 + golang.org/x/net v0.60.0, 0 reachable). Assets weed-{linux,darwin}-{amd64,arm64} + .sigstore.json; identity https://github.com/qiangli/seaweedfs/.github/workflows/bashy-build.yml@refs/tags/bashy-4.48-1, issuer binmgr.GitHubActionsIssuer. Check the exact asset names in the release.
Also (found by S434 story 88f90c8f, see bashy builds/seaweedfs/README.md): SeaweedFS telemetry DEFAULTS TO ON (-master.telemetry, server.go:113 / master.go:115 / mini.go:442 post to telemetry.seaweedfs.com once 1 GiB is stored). Make sure every yoke start path passes the telemetry-off flag for the subcommand it runs (server/master/mini) and binds loopback (-ip=127.0.0.1 or the subcommand equivalent) unless the mesh needs otherwise - cite upstream lines at 4.48. Check whether an earlier change (external/internal/serviceenv, docs/service-runtime-privacy.md) already does part of this and do not duplicate.
Acceptance: no upstream seaweedfs/seaweedfs download path; "latest" removed; Windows fails closed with the supply-chain policy error; unit test (no network) asserts VERIFIED pin + telemetry-off/loopback argv; one live check on dragon: binmgr installs darwin-arm64 weed (signature verified) and `weed version` prints 4.48. Gate: go vet + go test -count=1 ./external/seaweedfs/... ./pkg/binmgr/... green. Commit with trailers; sprint submit with SHA + gate + live output.
