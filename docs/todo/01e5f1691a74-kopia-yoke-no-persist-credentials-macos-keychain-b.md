---
id: 01e5f1691a74
kind: bug
title: 'kopia: yoke --no-persist-credentials (macOS Keychain blocks headless) + flip to bashy-signed qiangli/kopia bashy-v0.23.1-3'
seq: 2010
status: done
priority: p1
labels:
    - supply-chain
created: 2026-10-13T02:56:22.978772Z
assignee: muse-spark1.2
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-13T03:13:21.191987Z
closed_by: s434-manager
---

Two kopia changes in yoke external/kopia (start only once https://github.com/qiangli/kopia/releases/tag/bashy-v0.23.1-3 exists - run 38339234382):
1. BUG (found by S434 story 14e9a668): external/kopia/kopia.go:167-193 and 235-239 pass KOPIA_PASSWORD but omit --no-persist-credentials on repository create/connect (and wherever the server would persist), so on macOS kopia writes the password to the Keychain by default and blocks headless (the server never listens). Mirror bashy builds/kopia (bashy main 98300aeb: files/no-egress-test.sh) exactly; cite the flag at v0.23.1. Keep --otlp-trace global and KOPIA_CHECK_FOR_UPDATES=false (yoke a089a6db).
2. Flip to the bashy-signed release, exactly like the gitea/zot flip (yoke 4085f659: external/loom, external/zot): binmgr.Tool + exact Sigstore identity https://github.com/qiangli/kopia/.github/workflows/bashy-build.yml@refs/tags/bashy-v0.23.1-3, issuer binmgr.GitHubActionsIssuer, raw executables, sha256 per asset from the release SHA256SUMS; remove "latest" and the upstream kopia/kopia path; Windows fails closed. Update pkg/fleet/baseline/services/kopia.yaml if it pins a version.
Tests (no network): VERIFIED pin; argv/env assert --no-persist-credentials + KOPIA_CHECK_FOR_UPDATES=false + no repository-only flag in the global position. Live check on dragon (darwin-arm64, no containers): install via binmgr (signature verified), `kopia --version`, then a headless `repository create filesystem` + `server start` on 127.0.0.1 with a temp dir that serves HTTP 200 and does NOT touch the Keychain - paste output.
Gate: go vet + go test -count=1 ./external/kopia/... ./external/internal/... ./pkg/binmgr/... ./pkg/fleet/ green. Commit with trailers; sprint submit WITH the SHA, the gate output and the live output.
