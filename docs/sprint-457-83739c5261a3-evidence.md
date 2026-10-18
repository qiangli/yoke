# Sprint 457 / Story 2290 / 83739c5261a3 qualification evidence

Status: **partial delivery; admission blocked**, 2026-10-17 (Dragon local date).
Manager: s457-manager. Candidate: the git commit containing this note; base
`e47270a0d5da4961067a26a1de9d910b4a1b34e6`. This is the single Yoke-side
supplement to the existing umbrella `docs/research/n8n-cli/candidates.csv` and
`license-evidence.json`, keyed by their existing candidate names. It is not a
second catalog or a support declaration. The final candidate SHA is supplied in
the story submission and manager message (a commit cannot contain its own hash).

## Delivered behavior

- gh/rclone release matching now requires the tool prefix, complete OS/architecture
  suffix and archive extension. `arm` no longer matches `arm64`; checksum sidecars,
  unrelated archives and native package formats cannot be selected accidentally.
- curl uses Cobra's configured input/output/error writers, allowing the same
  invocation to be captured by callers. Its transport and binmgr are unchanged.
- Focused tests cover gcloud host preference, missing-SDK diagnostics, refusal to
  resolve/download any version and canary-safe version diagnostics. A separate
  opt-in probe reproduces the shared registry's cached-SDK bypass.
- Explicit curl fixture tests exercise two requested pages, read-only HTTP effects,
  provider failure (foreign exit 22), error-body canary suppression with `--fail`,
  and cancellation/reaping. They disable curlrc (`-q` first), bypass proxies,
  isolate HOME/CURL_HOME, use an ephemeral loopback endpoint, and clean up through
  test cleanup. No account or real user data is accessed.
- An opt-in rclone fixture uses an explicitly selected binary, empty config,
  isolated environment and temporary local source/destination. It tests lsjson
  and copy --dry-run without writing the destination. It is **not run here**:
  no rclone test executable is available. No transfer success is claimed.

## Contract consumption and blockers

The reviewed 446 S1 contract selects existing `coreutils/pkg/weavecli` `loom-v2`,
with `schema_version`, and foreign statuses passed through. The approved cursor is
`bashy-cursor-v1`, also with `schema_version`; stale `version` wording in
common-options is superseded by the Operator decisions. No private envelope,
cursor parser, error mapping, registry, lease or transport was added.
The existing `fleet.CommandSchema` is an input contract, not a provider result
adapter. This checkout has no delivered 446 command `adapt` implementation or
candidate adapter rows to consume; `pkg/skills/adapt.go` adapts skills, not foreign
command options. No claim of CLI/MCP typed-result parity follows from these tests.

449's primary-purpose taxonomy is consumed unchanged: userland, workplace,
sandbox, sphere, cluster, cloud. The existing gcloud registry is cloud (6), Ollama
is sphere (4); workspace remains a clone location, not a tier. No taxonomy edits.

Three investigation paths establish the remaining blockage:

1. Existing research records identify source-level license evidence, but no selected
   shipped artifact SBOM, locked transitive-license audit, notices bundle or signed
   runtime evidence. gh/rclone still default to `latest`; selecting a known test
   version in a unit test is not a reviewed release pin.
2. Source inspection finds direct passthrough output, no shared provider cursors,
   no enforced per-operation bounds/redaction/admission, and absent adapt rows.
   The read-only registry owns gcloud resolution. Creating a private replacement
   would violate the task's contract and scope.
3. The fixture probe actually demonstrates that a missing host SDK can resolve an
   unverified cached gcloud. The original probe exited 1; the opt-in replay below
   also records its actual result. This must be fixed by the registry owner.

All five candidates remain **defer for full 457 admission**. A passing subset gate
must not close this story as fully qualified. See `../EXISTING-BLOCKERS.md`.

## Candidate-linked source/license evidence

The following hashes and source references are inherited from the existing 456
research record, not independently reverified license admission. A research commit
is not a selected stable release/build pin. No new upstream artifact was downloaded.

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

“Eligible” below is policy eligibility after evidence, **not runtime admission**.
Existing unrestricted passthroughs have not become a per-mode enforcement layer.

| Candidate | Eligible bounded mode | Denied/unadmitted modes | Credential/target and effects gaps |
|---|---|---|---|
| gh | Explicit-host/account API read and bounded pagination | extensions, arbitrary child execution, persistent watch/bridge; mutations unverified | No test account/scopes; no bound bashy cursor, output limits, provider-error redaction or typed provider errors verified; read needs network/process effects |
| rclone | Explicit local/remote listing and bounded transfer; provider-validated dry-run | serve, rcd, mount and background services without Rule 1 evidence; destructive sync/purge unverified | Empty-config local dry-run fixture supplied but skipped; remote scopes/backend pagination, cancellation, integrity and write cleanup unverified; remote preview still has network/auth/read effects |
| curl | Explicit finite HTTP request (fixture GET tested) | service semantics, pagination abstraction, unsupported dry-run treated as mutation prevention | No service account; only manual second-page GET tested, no bashy cursor; arbitrary flags/payloads not redacted; GET has network/process effects; mutation options remain unqualified |
| gcloud | Identified host SDK, explicit project/account, bounded read | SDK provisioning, unverified cache fallback, components update, service provisioning and ambient account use | Host selection/missing diagnostics tested with inert fixture; real executable version/hash and auth/API drift unverified; registry cache fallback fails strict host-only |
| ollama | Read-only request to explicitly configured existing endpoint, or approved signed runtime | upstream download/start, PATH/OLLAMA_BIN fallback; model pull/delete and managed-service qualification outside this read-only audit | No endpoint or approved signed artifact supplied; DefaultURL still has implicit localhost fallback; error bodies read without general redaction/bounds; no request made |

Ollama registry resolver remains disabled, pinned compatibility constant v0.40.1;
its AssetMatch stub returns false. No registry edits, Ollama execution, service
startup, model access or upstream downloads occurred. The checked-out source
submodule pin is `b0d0bbba93ab6382b0a75b1bbba1653cea9cbb1f`, different from the
research license revision; the research hash does not qualify this built runtime.

## Platform/artifact coverage

Actual gate host: Dragon, Go 1.27.2, darwin/arm64. No Linux/Windows native execution
or signed-runner execution occurred. Table-driven OS cases prove name selection
only, never native support. Manager must gate whole touched packages on a claimed
remote host before any push. No push was performed.

| Candidate | Actual artifact evidence | Coverage |
|---|---|---|
| gh | No selected release archive/hash; no client executed | package build/unit fixtures only; all native authenticated modes unverified |
| rclone | No selected release archive/hash; no test executable | package build/unit fixtures; dry-run and remote transfer unverified |
| curl | Explicit `/usr/bin/curl`, executable SHA-256 `5ab042572ea0e068644e3b8f9e8dd1ad197bfcf33d199316615b46ddc4390a41` | loopback fixture execution on darwin/arm64 host; binary reports `curl 8.7.1 (x86_64-apple-darwin25.0)`; do not infer arm64-native binary support |
| gcloud | Inert test executable only, never executed; research SDK 575.0.0 is not this run's verified artifact | host lookup/missing fixture only; real SDK unavailable/unverified for admission |
| ollama | No signed artifact/hash/signer verification | source inspection only; all native/runtime and existing-endpoint checks unverified |

curl reports libcurl 8.7.1, SecureTransport, LibreSSL 3.3.6, zlib 1.2.12 and nghttp2
1.68.1. This is runtime version output, not a transitive dependency/license audit.
The existing Windows fallback is 8.21.0_2 from
`https://curl.se/windows/dl-8.21.0_2/curl-8.21.0_2-win64-mingw.zip`, archive SHA-256
`2a3e951f522be7d9a3a964be57e74ee15d19dd970bcc7b7843445089901a3ed4`;
not downloaded or verified on Windows in this run. No fallback platform is admitted.

## Reuse by other sprints

Sprint 382 create-to-post `5a592739a607` and Sprint 453 object storage should reference
**this note at the submitted candidate SHA**, section “Per-mode disposition and
effects”, rclone row, plus `external/rclone/rclone_test.go` and its raw gate below.
The reference conveys a skipped dry-run and explicit transfer gaps, not successful
publication or storage evidence. Neither workflow nor a storage engine is implemented.

## Raw gate evidence

Commands below run inside the isolated clone. Exit codes are process exit codes,
not inferred from a wrapper tool. The intentional host-only audit failure is retained
separately from passing package gates. No podman/Ollama tests or heavy suites run.

```text
$ go build ./cmd/yoke
(no output)
exit=0
```

```text
$ go build ./external/gh ./external/gcloud ./external/curlbin ./external/rclone
(no output)
exit=0
```

```text
$ go test -short -count=1 -v ./external/gh ./external/gcloud ./external/rclone
=== RUN   TestSpec
--- PASS: TestSpec (0.00s)
=== RUN   TestAssetMatch
--- PASS: TestAssetMatch (0.00s)
PASS
ok  	github.com/qiangli/yoke/external/gh	0.305s
=== RUN   TestInstallHintNonEmptyAndVendorPath
--- PASS: TestInstallHintNonEmptyAndVendorPath (0.00s)
=== RUN   TestResolveDoesNotAutoDownload
--- PASS: TestResolveDoesNotAutoDownload (0.00s)
=== RUN   TestQualificationResolveRefusesEveryVersion
--- PASS: TestQualificationResolveRefusesEveryVersion (0.00s)
=== RUN   TestQualificationProbeCacheMustNotReplaceHostSDK
    qualification_probe_test.go:14: known registry cache fallback; set S457_AUDIT_HOST_ONLY=1 to reproduce admission failure
--- SKIP: TestQualificationProbeCacheMustNotReplaceHostSDK (0.00s)
=== RUN   TestQualificationHostSDKAndMissingDiagnostic
--- PASS: TestQualificationHostSDKAndMissingDiagnostic (0.01s)
PASS
ok  	github.com/qiangli/yoke/external/gcloud	0.445s
=== RUN   TestQualificationAssetSelection
--- PASS: TestQualificationAssetSelection (0.00s)
=== RUN   TestQualificationExplicitVersion
--- PASS: TestQualificationExplicitVersion (0.00s)
=== RUN   TestQualificationExplicitDryRun
    rclone_test.go:48: rclone artifact unavailable; set S457_RCLONE_TEST_BINARY explicitly
--- SKIP: TestQualificationExplicitDryRun (0.00s)
PASS
ok  	github.com/qiangli/yoke/external/rclone	0.291s
exit=0
```

```text
$ S457_CURL_TEST_BINARY=/usr/bin/curl go test -short -count=1 -v ./external/curlbin
=== RUN   TestQualificationMissingHost
--- PASS: TestQualificationMissingHost (0.00s)
=== RUN   TestQualificationExplicitClient
    curlbin_test.go:45: platform=darwin/arm64 executable_sha256=5ab042572ea0e068644e3b8f9e8dd1ad197bfcf33d199316615b46ddc4390a41
    curlbin_test.go:94: read /page/1: {"items":[1],"next":"/page/2"}
    curlbin_test.go:94: read /page/2: {"items":[2],"next":null}
    curlbin_test.go:104: denied: foreign exit=22; --fail suppresses provider error-body canary (not a general redaction guarantee)
    curlbin_test.go:120: cancellation: child returned failure and was reaped
--- PASS: TestQualificationExplicitClient (0.47s)
PASS
ok  	github.com/qiangli/yoke/external/curlbin	1.015s
exit=0
```

```text
$ S457_AUDIT_HOST_ONLY=1 go test -short -count=1 -v ./external/gcloud -run TestQualificationProbeCacheMustNotReplaceHostSDK
=== RUN   TestQualificationProbeCacheMustNotReplaceHostSDK
    qualification_probe_test.go:25: HOST-ONLY GAP: missing PATH SDK resolved unverified cache fixture (gcloud); no executable was run
--- FAIL: TestQualificationProbeCacheMustNotReplaceHostSDK (0.01s)
FAIL
FAIL	github.com/qiangli/yoke/external/gcloud	0.376s
FAIL
exit=1
```
