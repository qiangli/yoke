# Sprint 457 / Story 2290 / 83739c5261a3 qualification evidence

Status: **delivered for local/loopback scope**, 2026-10-17. Checks that need a
real account (gh account reads, gcloud project calls, remote rclone backends) are
moved to **Sprint 465** by operator decision 2026-10-17 and were not attempted.
Artifact SBOM/notice evidence for clients bashy does not ship stays **unverified**.
Base `921053bf3a2d35799ea1b7316185a3855070846f`; the candidate is the commit
containing this note (a commit cannot contain its own hash). This is the single
Yoke-side supplement to the umbrella `docs/research/n8n-cli/candidates.csv` and
`license-evidence.json`, keyed by their candidate names — not a second catalog.

## Delivered behavior

- **Reviewed release pins (gh, rclone).** `external/gh.DefaultVersion` is
  `v2.102.0` (cli/cli, published 2026-09-30) and `external/rclone.DefaultVersion`
  is `v1.75.2` (rclone/rclone, published 2026-10-09), both the current stable
  releases. Per-platform archive SHA-256s for darwin/linux/windows × amd64/arm64
  are committed in `pkg/binmgr/pins.go` — the same committed-pin registry the
  `external/registry` gate uses — so `binmgr.Ensure` verifies the download
  against our digest, not the release's. `gh.Ensure` / `rclone.Path` now fail
  closed **before any network** for `latest` or any unpinned version, mirroring
  `registry.Entry.Ensure`. gh/rclone stay in their own packages rather than
  `external/registry` entries: bashy mounts `gh.NewGhCmd`/`rclone.NewRcloneCmd`
  directly and also mounts every `registry.Names()` verb, so a registry row
  would register each verb twice.
- **gcloud host-only fix.** `registry.Entry.Ensure` no longer falls back to
  `binmgr.CachedBinary` for `PreferHost` entries: a missing host SDK now goes to
  `gcloud.Resolve` and its actionable diagnostic ("gcloud not found on PATH —
  install the Google Cloud SDK — … cloud.google.com/sdk/docs/install") instead
  of an unverified cached binary. `TestQualificationProbeCacheMustNotReplaceHostSDK`
  is now a regular test over both cache layouts; it failed (exit 1) with the fix
  stashed and passes with it. gws (the other PreferHost entry) keeps its explicit
  `$GWS_VERSION` path; its unset path re-resolves through the binmgr GitHub cache
  instead of the bare cache lookup.
- **446 S6 adapt rows** (`pkg/fleet/baseline/adapters`, no new framework):
  `gh` and `gh-api` (preview unsupported; `gh api` JSON is native), `rclone`
  (preview unsupported — covers serve/mount/purge), `rclone-copy` and
  `rclone-sync` (`provider-validated-preview`, `--dry-run`), `rclone-lsjson`
  (native JSON), `curl` (preview unsupported, no JSON mapping), `gcloud`
  (`--format=json`, preview unsupported). `pkg/adapt` `TestCuratedMappings`
  pins every mapping and refusal. No Ollama row: `bashy ollama` is a native verb
  over the embedded runtime, not a foreign CLI.
- **Local/loopback fixtures** (opt-in, explicit absolute client path, isolated
  HOME/config, temp dirs, test cleanup; no account or real data):
  - gh (`external/gh/qualification_test.go`): gh's own `http_unix_socket`
    setting routes all API traffic to a temp unix-socket fake host. Read, `--paginate`
    following a `Link: rel="next"` header across two pages, adapt dry-run refusal,
    canary token absent from a 403 failure and from the `GH_DEBUG=api` trace
    (gh prints `Authorization: token ████`), and cancel → kill → reap.
  - rclone (`external/rclone/rclone_test.go`): local backend `lsjson` read; the
    dry-run is built and executed **through `adapt.Build`/`adapt.Run` with the
    shipped `rclone-copy` row** and leaves the destination empty; cancelling a
    blocked `rcat` kills/reaps the child and writes nothing; a canary password in a
    loopback sftp remote (127.0.0.1:1, refused) is absent — plaintext and obscured —
    from `config redacted` and from the provider-failure diagnostics. Local
    listing has no pagination; rclone remote-backend paging is Sprint 465.
  - curl (`external/curlbin/curlbin_test.go`, unchanged): loopback read of two
    pages, foreign exit 22 with `--fail` suppressing an error-body canary, and
    cancellation/reaping. curl has no dry-run; the adapt `curl` row refuses it.
- **Ollama** unchanged and still refusing: the registry `Resolve` rejects every
  version (`TestOllamaRegistryRefusesDownload`, `external/ollama`
  `TestResolve_RefusesPerSupplyChainPolicy`/`TestRun_*`), pins.go carries no
  ollama digest by design, and nothing here downloads or starts an upstream
  service. Supported paths remain the bashy-signed embedded runner or an existing
  endpoint (`OLLAMA_HOST`; `DefaultURL` connects to `127.0.0.1:11434` and never
  starts anything). No Ollama request was made in this run.

No duplicate transport, storage engine, cursor, error ABI or registry was added.

## Reviewed release pins

Each digest matched two independent sources: the release's own checksum file
(`gh_2.102.0_checksums.txt`, rclone `SHA256SUMS`) and GitHub's published asset
`digest`. darwin/arm64 additionally matched a third time: provisioning through
`gh.Ensure`/`rclone.Path` into an empty temp `BASHY_BIN_CACHE` verified against
the pin and produced the executables used by the fixtures.

| Client | Platform | Archive SHA-256 |
|---|---|---|
| gh v2.102.0 | darwin/amd64 | `b245f24eb2bf5f75b426b4c26da3651a107f8d5b6f4fddfbfccc5679041378b3` |
| gh v2.102.0 | darwin/arm64 | `da922c20d1792e5b2cbf375593d7a658acf034c12c84e007e71c76ef959c337e` |
| gh v2.102.0 | linux/amd64 | `bb766f710eef8ede859c18578c72c327597cd4c8a85b06001b1f3843c6019386` |
| gh v2.102.0 | linux/arm64 | `7862c86c72f43df3a2d93ddde6f473285b4e2af61b494849846827e513ef6484` |
| gh v2.102.0 | windows/amd64 | `ae64e556ecc240b200f7eba60d550e4bb60d78e860e69dd88c449405b86067f4` |
| gh v2.102.0 | windows/arm64 | `5dcf12aa8525eabd0c46ec414f323ab6cf65229fc2cd46543cc705001bbaf223` |
| rclone v1.75.2 | darwin/amd64 | `574e1f31cf1c464e5fb0325cb66fcbdeaf1efa1802bd9b17f13c256fd74b6561` |
| rclone v1.75.2 | darwin/arm64 | `610ab7d48615135dfc60b763858b11a426a0d3d1160c1bb76fb0ae6011b02edd` |
| rclone v1.75.2 | linux/amd64 | `349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702` |
| rclone v1.75.2 | linux/arm64 | `7e1e8d69654941b7b7df84ee74c5f7fb09cce7d5496947fb7bf16b55ff427d10` |
| rclone v1.75.2 | windows/amd64 | `8a9db2bad1dc79a65b3348fd857069598e2aab7cf4d118b77b1df23055aff818` |
| rclone v1.75.2 | windows/arm64 | `7eb9844c9e5b723b9ef28821891caa17f6d9d1a375c49365cbe04114cec8dcd9` |

These are SHA-256-pinned upstream client releases under Rule 2's existing binmgr
exception; no server/long-lived mode (rclone serve/rcd/mount, gh extensions) is
admitted by them. rclone's SHA256SUMS PGP signature was not verified.

## Candidate-linked source/license evidence

The following hashes and source references are inherited from the existing 456
research record, not independently reverified license admission. A research commit
is not a selected stable release/build pin. The gh and rclone release pins below
(section "Reviewed release pins") select client artifacts; they do not upgrade any
license, SBOM or notice field from unverified.

### gh

- Source: `cli/cli`; research revision: `fa92838bef95fe4a29c1587693885d5e9bdb6cf1`.
- License scope: MIT; CLI only; extensions require separate review. No dual-license choice made.
- License text reference: `https://github.com/cli/cli/blob/fa92838bef95fe4a29c1587693885d5e9bdb6cf1/LICENSE`.
- Recorded license-text SHA-256: `6da4adc42392c8485e40b4251c7e332fc3352df1947c9ffade71dd60b14a7a4f`.
- Notices: upstream copyright/license text must accompany distributed copies;
  Apache-2.0 applicable NOTICE content must also be retained. Exact artifact notice
  inventory and distribution location: **unverified; no artifact shipped here**.
- Source archive SHA/build recipe/toolchain/patch provenance for a selected client:
  **unverified**. Direct/transitive dependency lock, optional components, license
  inventory, SBOM and vulnerability audit for the actual client: **unverified**.

### rclone

- Source: `rclone/rclone`; research revision: `35a03a47e00e2cadec744f977e5fa9d851127016`.
- License scope: MIT; client only; serve/mount/rcd and runtime backends require separate review. No dual-license choice made.
- License text reference: `https://github.com/rclone/rclone/blob/35a03a47e00e2cadec744f977e5fa9d851127016/COPYING`.
- Recorded license-text SHA-256: `8cd2e9e750b90a04b7d82dbbca3930c696ae0309d7c10464f90a44f45754cd04`.
- Notices: upstream copyright/license text must accompany distributed copies;
  Apache-2.0 applicable NOTICE content must also be retained. Exact artifact notice
  inventory and distribution location: **unverified; no artifact shipped here**.
- Source archive SHA/build recipe/toolchain/patch provenance for a selected client:
  **unverified**. Direct/transitive dependency lock, optional components, license
  inventory, SBOM and vulnerability audit for the actual client: **unverified**.

### curl

- Source: `curl/curl`; research revision: `5d4eaac8f222643a1a59f3567e0e58498388d4fe`.
- License scope: curl; libcurl/curl source only; TLS, compression and other linked libraries have separate licenses. No dual-license choice made.
- License text reference: `https://github.com/curl/curl/blob/5d4eaac8f222643a1a59f3567e0e58498388d4fe/COPYING`.
- Recorded license-text SHA-256: `82f2f4427d6545ee5aaac4f0b80428da6cc8ba41c2cf5da3a03680ec327b9681`.
- Notices: upstream copyright/license text must accompany distributed copies;
  Apache-2.0 applicable NOTICE content must also be retained. Exact artifact notice
  inventory and distribution location: **unverified; no artifact shipped here**.
- Source archive SHA/build recipe/toolchain/patch provenance for a selected client:
  **unverified**. Direct/transitive dependency lock, optional components, license
  inventory, SBOM and vulnerability audit for the actual client: **unverified**.

### gcloud

- Source: `Google vendor SDK distribution`; research revision: `installed version 575.0.0`.
- License scope: Apache-2.0; core SDK only; optional components and Python/runtime distributions excluded. No dual-license choice made.
- License text reference: `/opt/homebrew/share/google-cloud-sdk/LICENSE`.
- Recorded license-text SHA-256: `d5f7fc037d5a22f75f64a78193fc7b02e49740cbcaa75992207ac6e92f76d432`.
- Notices: upstream copyright/license text must accompany distributed copies;
  Apache-2.0 applicable NOTICE content must also be retained. Exact artifact notice
  inventory and distribution location: **unverified; no artifact shipped here**.
- Source archive SHA/build recipe/toolchain/patch provenance for a selected client:
  **unverified**. Direct/transitive dependency lock, optional components, license
  inventory, SBOM and vulnerability audit for the actual client: **unverified**.

### ollama

- Source: `ollama/ollama`; research revision: `d1ed3fd13ac73f60cff00bbead8af24a68c4200d`.
- License scope: MIT; source client/server only; native runner, libraries, models and remote service licenses separate. No dual-license choice made.
- License text reference: `https://github.com/ollama/ollama/blob/d1ed3fd13ac73f60cff00bbead8af24a68c4200d/LICENSE`.
- Recorded license-text SHA-256: `5934ed2ce0d15154bcdb9c85203210abac0da4314af34081e36df4599f90b226`.
- Notices: upstream copyright/license text must accompany distributed copies;
  Apache-2.0 applicable NOTICE content must also be retained. Exact artifact notice
  inventory and distribution location: **unverified; no artifact shipped here**.
- Source archive SHA/build recipe/toolchain/patch provenance for a selected client:
  **unverified**. Direct/transitive dependency lock, optional components, license
  inventory, SBOM and vulnerability audit for the actual client: **unverified**.

## Per-mode disposition and effects

"Eligible" is policy eligibility on the evidence below, **not** a support claim
for any account, remote backend or platform not listed in the coverage table.

| Candidate | Exercised locally (this run) | Denied / refused | Sprint 465 (real account/backend) |
|---|---|---|---|
| gh | read, `--paginate` (2 pages), 403 failure, token redaction (failure + debug trace), cancellation — fake host over unix socket | `--dry-run` (no preview; adapt refuses); extensions, persistent watch/bridge; any unpinned version | authenticated reads against github.com/GHES, scopes, rate limits, real pagination |
| rclone | local `lsjson` read, adapt `rclone-copy --dry-run` (destination untouched), cancellation of blocked `rcat`, credential redaction (`config redacted` + loopback failure) | serve, rcd, mount (adapt `rclone` row: preview unsupported); `--dry-run` on non-copy/sync verbs; any unpinned version | remote backends: listing/paging, transfer, integrity, write cleanup |
| curl | loopback 2-page read, exit-22 failure with `--fail` canary suppression, cancellation | `--dry-run` (curl has none; adapt refuses); `--json` mapping absent | service APIs with real credentials |
| gcloud | host SDK discovery; missing SDK → actionable diagnostic; unverified cache refused (both layouts); version refusal for every requested version | SDK provisioning/download, cache fallback, `components update`, `--dry-run` | any project/account call (auth, list, read) |
| ollama | refusal tests only (registry + external/ollama) | upstream download, upstream service start, PATH/`OLLAMA_BIN` fallback | n/a — supported runtime is the signed embedded runner or an existing endpoint; no endpoint request made |

Effects follow the adapt rows: every client carries `net.egress` + `proc.spawn`;
gh/gcloud/rclone add remote write/delete; curl and rclone add local `fs.write`.
A remote read still has network effects. Redaction evidence above is
client-behavior evidence on fixtures, not a general bashy redaction layer.

## Platform/artifact coverage

Actual gate host: dev-host, macOS (Darwin 25.5.0), Go 1.27.2, **darwin/arm64 only**.
No Linux or Windows native execution, no cross-compiles, no signed-runner runs.
Table-driven OS/arch cases prove asset-name selection and pin presence only.

| Candidate | Executed artifact (darwin/arm64) | Other platforms |
|---|---|---|
| gh | v2.102.0 from pinned `gh_2.102.0_macOS_arm64.zip`; executable SHA-256 `8a4258433c81106343144857750316241759d06dcf16265cf3c4864a8f2f2ad6` | pins committed for darwin/amd64, linux/{amd64,arm64}, windows/{amd64,arm64}; **not executed** |
| rclone | v1.75.2 from pinned `rclone-v1.75.2-osx-arm64.zip` (go1.26.9, cmount); executable SHA-256 `af0257730173748dc9706ad3c83288bf3a0c5d5e9768fef20235e375e9d835e1` | pins committed for the other five; **not executed** |
| curl | host `/usr/bin/curl` 8.7.1 (`x86_64-apple-darwin25.0` build string), SHA-256 `5ab042572ea0e068644e3b8f9e8dd1ad197bfcf33d199316615b46ddc4390a41` | Windows fallback 8.21.0_2 recorded below, not downloaded/verified |
| gcloud | host SDK 575.0.0 (core 2026.06.26, bundled python 3.14.5) at `/opt/homebrew/share/google-cloud-sdk/bin/gcloud`, entry-script SHA-256 `8628b9f971d07c5e52fd2c6d469ee4a0940c75eb51820d628c3dd068981bd260`; only `--version` run, isolated `CLOUDSDK_CONFIG`, update check and usage reporting off | host-only by design; Linux/Windows unverified |
| ollama | none executed | none |

curl reports libcurl 8.7.1, SecureTransport, LibreSSL 3.3.6, zlib 1.2.12 and nghttp2
1.68.1 — runtime version output, not a transitive dependency/license audit. The
existing Windows fallback is 8.21.0_2 from
`https://curl.se/windows/dl-8.21.0_2/curl-8.21.0_2-win64-mingw.zip`, archive SHA-256
`2a3e951f522be7d9a3a964be57e74ee15d19dd970bcc7b7843445089901a3ed4`; not downloaded
or verified on Windows in this run.

The Ollama registry resolver stays disabled (compatibility constant v0.40.1,
`ollamaAssetMatch` returns false). The checked-out source submodule pin
`b0d0bbba93ab6382b0a75b1bbba1653cea9cbb1f` differs from the research license
revision; the research hash does not qualify the built runtime.

## Reuse by other sprints

Sprint 382 create-to-post `5a592739a607` and Sprint 453 object storage should
reference **this note at the submitted candidate SHA**, rclone rows above, plus
`external/rclone/rclone_test.go` `TestQualificationExplicitDryRun` and its raw gate
below. That conveys pinned-artifact provisioning and **local-backend** read,
dry-run, cancellation and redaction; remote transfer and storage evidence remain
Sprint 465. Neither workflow nor a storage engine is implemented here.

## Raw gate evidence

Run inside the isolated clone; exit codes are process exit codes captured
directly (no pipes). Fixture binaries: `S457_GH_TEST_BINARY` /
`S457_RCLONE_TEST_BINARY` = the pinned darwin/arm64 executables above,
`S457_CURL_TEST_BINARY=/usr/bin/curl`.

```text
$ GOWORK=off go test -count=1 <pkg>      # whole packages, fixture env set
./external/gh        exit=0  ok  github.com/qiangli/yoke/external/gh        0.455s
./external/rclone    exit=0  ok  github.com/qiangli/yoke/external/rclone    0.753s
./external/gcloud    exit=0  ok  github.com/qiangli/yoke/external/gcloud    0.273s
./external/curlbin   exit=0  ok  github.com/qiangli/yoke/external/curlbin   0.309s
./external/registry  exit=0  ok  github.com/qiangli/yoke/external/registry  0.590s
./pkg/adapt          exit=0  ok  github.com/qiangli/yoke/pkg/adapt          0.412s
./pkg/fleet          exit=0  ok  github.com/qiangli/yoke/pkg/fleet          79.174s
./pkg/binmgr         exit=0  ok  github.com/qiangli/yoke/pkg/binmgr         1.296s

$ GOWORK=off go vet ./external/gh ./external/rclone ./external/gcloud ./external/registry ./pkg/adapt ./pkg/binmgr
exit=0

$ GOWORK=off go build ./...
exit=0
```

```text
$ git stash push -- external/registry/registry.go   # fix removed
$ GOWORK=off go test -count=1 -run TestQualificationProbeCacheMustNotReplaceHostSDK ./external/gcloud
    --- FAIL: TestQualificationProbeCacheMustNotReplaceHostSDK/gcloud (0.00s)
        qualification_probe_test.go:38: HOST-ONLY GAP: missing PATH SDK resolved unverified cache fixture (gcloud)
    --- FAIL: TestQualificationProbeCacheMustNotReplaceHostSDK/gcloud/575.0.0/gcloud (0.00s)
        qualification_probe_test.go:38: HOST-ONLY GAP: missing PATH SDK resolved unverified cache fixture (gcloud)
exit=1
$ git stash pop                                      # fix restored
--- PASS: TestQualificationProbeCacheMustNotReplaceHostSDK (0.00s)
exit=0
```

```text
$ GOWORK=off go test -count=1 -v -run 'Qualification|DefaultVersionPinned|OllamaRegistryRefusesDownload|CuratedMappings$' \
    ./external/gh ./external/rclone ./external/gcloud ./external/curlbin ./external/registry ./pkg/adapt
--- PASS: TestDefaultVersionPinnedAndUnpinnedRefused (0.00s)
    qualification_test.go:42: platform=darwin/arm64 executable_sha256=8a4258433c81106343144857750316241759d06dcf16265cf3c4864a8f2f2ad6
    qualification_test.go:118: read+pagination: gh api --paginate fetched both fixture pages
    qualification_test.go:128: dry-run: adapt row gh-api preview_level=unsupported refuses before execution
    qualification_test.go:149: redaction: 403 failure + GH_DEBUG=api trace omit canary token; header sent only to fixture
    qualification_test.go:170: cancellation: child returned failure and was reaped; no account/host support claimed (Sprint 465)
--- PASS: TestQualificationFakeHost (0.92s)
ok  	github.com/qiangli/yoke/external/gh	1.294s
--- PASS: TestQualificationAssetSelection (0.00s)
--- PASS: TestDefaultVersionPinnedAndUnpinnedRefused (0.00s)
--- PASS: TestQualificationExplicitVersion (0.00s)
    rclone_test.go:82: platform=darwin/arm64 executable_sha256=af0257730173748dc9706ad3c83288bf3a0c5d5e9768fef20235e375e9d835e1
    rclone_test.go:122: read: lsjson on local backend returned the fixture
    rclone_test.go:143: dry-run: adapt row rclone-copy, preview_level=provider-validated-preview, destination unchanged
    rclone_test.go:174: cancellation: child returned failure, was reaped, wrote nothing
    rclone_test.go:199: redaction: config redacted and provider failure omit canary (plain and obscured)
    rclone_test.go:200: local read, adapt dry-run, cancellation and redaction passed; no remote backend support claimed (Sprint 465)
--- PASS: TestQualificationExplicitDryRun (1.38s)
ok  	github.com/qiangli/yoke/external/rclone	2.064s
--- PASS: TestQualificationResolveRefusesEveryVersion (0.00s)
--- PASS: TestQualificationProbeCacheMustNotReplaceHostSDK (0.00s)
--- PASS: TestQualificationHostSDKAndMissingDiagnostic (0.01s)
ok  	github.com/qiangli/yoke/external/gcloud	1.210s
--- PASS: TestQualificationMissingHost (0.00s)
    curlbin_test.go:45: platform=darwin/arm64 executable_sha256=5ab042572ea0e068644e3b8f9e8dd1ad197bfcf33d199316615b46ddc4390a41
    curlbin_test.go:94: read /page/1: {"items":[1],"next":"/page/2"}
    curlbin_test.go:94: read /page/2: {"items":[2],"next":null}
    curlbin_test.go:104: denied: foreign exit=22; --fail suppresses provider error-body canary (not a general redaction guarantee)
    curlbin_test.go:120: cancellation: child returned failure and was reaped
--- PASS: TestQualificationExplicitClient (0.36s)
ok  	github.com/qiangli/yoke/external/curlbin	1.847s
--- PASS: TestOllamaRegistryRefusesDownload (0.00s)
ok  	github.com/qiangli/yoke/external/registry	0.901s
--- PASS: TestCuratedMappings (0.01s)
ok  	github.com/qiangli/yoke/pkg/adapt	1.731s
exit=0
```

Not run: `scripts/crossvet.sh`, cross-compiles and other heavy suites (operator
rule for this host); dependents `pkg/feed`/`pkg/mirror` (unchanged code; their
rclone tests now provision the pinned v1.75.2 instead of `latest`). No push.
