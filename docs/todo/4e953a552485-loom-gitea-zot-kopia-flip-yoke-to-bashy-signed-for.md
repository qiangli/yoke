---
id: 4e953a552485
kind: task
title: 'loom(gitea)/zot/kopia: flip yoke to bashy-signed fork releases (bashy-v28.1.0-2, bashy-v2.1.22-2, bashy-v0.23.1-2)'
seq: 2009
status: done
priority: p1
labels:
    - supply-chain
created: 2026-10-13T02:24:24.989171Z
weave: 224
assignee: muse-spark1.2
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-13T02:45:20.871009Z
closed_by: s434-manager
---

SCOPE NOW: gitea (loom) + zot only. kopia's release does not exist yet (darwin no-egress gate failed in run 38336217370) - do NOT touch external/kopia in this story; the manager adds kopia later.

Flip three yoke services to their bashy-signed fork releases, exactly like the seaweedfs flip (yoke 830346b7, external/seaweedfs) and the Victoria flip (yoke 23a0f331): binmgr.Tool + Sigstore (exact identity https://github.com/qiangli/<repo>/.github/workflows/bashy-build.yml@refs/tags/<bashy tag>, issuer binmgr.GitHubActionsIssuer), raw executables, per-asset sha256 read from each release's SHA256SUMS (download it, never the API listing), "latest" and every upstream download path removed, Windows fails closed with the supply-chain policy error.
Releases (bashy main 2530a8b4 declarations; check exact asset names in each release):
- gitea (used by external/loom; today upstream go-gitea/gitea v28.1.0 via binmgr): https://github.com/qiangli/gitea/releases/tag/bashy-v28.1.0-2
- zot (external/zot, today "latest" project-zot/zot): https://github.com/qiangli/zot/releases/tag/bashy-v2.1.22-2 - this is a MINIMAL build (no search/sync/imagetrust/ui extensions; BinaryType bashy-minimal). Verify yoke's seeded zot config.json enables none of those (it should already have an empty extensions map) and add a test asserting it.
- kopia (external/kopia, today "latest" kopia/kopia): https://github.com/qiangli/kopia/releases/tag/bashy-v0.23.1-2 (keep yoke a089a6db's argv/env: --otlp-trace global, KOPIA_CHECK_FOR_UPDATES=false).
Also update pkg/fleet/baseline/services/{loom?,zot,kopia}.yaml versions if they pin one.
Acceptance: unit tests (no network) assert each is a VERIFIED pin (identity, issuer, every asset URL + sha256 + bundle, raw executable) and no upstream path remains; live check on dragon: binmgr installs darwin-arm64 of each (signature verified) and runs its version command (gitea --version, zot --version or `zot verify`/help, kopia --version) - paste output. Gate: go vet + go test -count=1 ./external/loom/... ./external/zot/... ./external/kopia/... ./pkg/binmgr/... ./pkg/fleet/... green. Commit with trailers; sprint submit with SHA + gate + live output. Do not start before all three releases exist (they publish within minutes of assignment).
