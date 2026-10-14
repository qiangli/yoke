---
id: 34deac42f064
kind: task
title: 'Victoria stores: flip yoke otel stack to the bashy-signed fork releases (VM v1.154.0, VL v1.53.0, VT v0.12.0)'
seq: 2006
status: done
priority: p1
labels:
    - supply-chain
created: 2026-10-12T22:36:22.449856Z
assignee: muse-spark1.2
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-12T22:47:46.603555Z
closed_by: s434-manager
---

Flip yoke external/otel/stack (execstore.go victoriaSpec, today: upstream VictoriaMetrics/* GitHub archives v1.51.0 / v1.147.0 / v0.9.4) to the bashy-signed fork releases built by bashy main 0607f515 (builds/victoria-*):

- victoria-metrics v1.154.0: https://github.com/qiangli/victoria-metrics/releases/tag/bashy-v1.154.0-1
- victoria-logs v1.53.0: https://github.com/qiangli/victoria-logs/releases/tag/bashy-v1.53.0-1
- victoria-traces v0.12.0: https://github.com/qiangli/victoria-traces/releases/tag/bashy-v0.12.0-1
Assets: raw executables <tool>-{linux,darwin}-{amd64,arm64}, each with <asset>.sigstore.json; SHA256SUMS. Signing identity = https://github.com/qiangli/<tool>/.github/workflows/bashy-build.yml@refs/tags/<bashy tag>, issuer binmgr.GitHubActionsIssuer (see builds/<tool>/README.md in bashy).

Template: yoke 613e424a (tuwunel: first bashy-signed pin - raw executables, per-asset sha256 + sigstore bundle, exact identity/issuer; the test asserts a VERIFIED pin). Use the same binmgr Tool + Sigstore mechanism (pkg/binmgr/sigstore.go), sha256 per asset read from the published SHA256SUMS (download it; do not trust the API listing).

Acceptance:
1. All three stores resolve ONLY the signed fork assets; no upstream VictoriaMetrics/* download path remains. Windows has no signed asset: fail closed with an error naming the supply-chain policy (no upstream fallback).
2. Version constants bumped; check the argv builders (victoria*Args) against the new versions' flags (e.g. -retentionPeriod, -httpListenAddr, -selfScrapeInterval, http.pathPrefix) - cite upstream flag source at the tag if anything changed.
3. Unit test (no network) asserting each store is a VERIFIED pin: identity, issuer, every asset sha256 + bundle URL, raw executable.
4. One live check on dragon (short, no containers): install the darwin-arm64 victoria-metrics via binmgr (signature verified) and run it with -version. Paste output.
5. Gate: go vet + go test -count=1 ./external/otel/... ./pkg/binmgr/... green (external/otel/stack may be a nested module - run from its dir). Commit with trailers; sprint submit with SHA + gate + live-check output.
