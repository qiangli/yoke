# aws evidence — story bedc320542eb (sprint 457)

Yoke-side admission and verification record for the AWS CLI v2
integration. Source/documentation inspection, build/license admission,
fixture tests, and live verified support are distinct rows below; a
registered command, a discovered binary, or a passing mock is NOT live
support.

## Admission (reviewed)

- Candidate: aws — Amazon Web Services CLI v2, Apache-2.0
  (`https://aws.amazon.com/cli/`).
- Mode: short-lived client only (supply-chain Rule 2). No server,
  daemon, or resident mode is admitted (`ClientRuntime`).
- Provisioning: host-first (PreferHost, gcloud posture). The CLI is a
  large vendor distribution whose installer is the trusted, self-updating
  path, so `Resolve` refuses every version with an actionable
  vendor-install hint instead of TOFU-downloading. No silent
  auto-provisioning.
- Notices: a host binary is the vendor's build, not a bashy-verified
  artifact; release selection plus committed pins are a named Sprint 465
  gap.

## Consumed shared contracts (no private ABI)

- Native result/error envelope: loom-v2 (`coreutils/pkg/weavecli`,
  `schema_version: loom-v2`). The ONE discriminator spelling everywhere is
  `schema_version`. No 457 error/cursor/envelope ABI is introduced.
- Pagination: opaque provider tokens bound to (target, query, revision);
  cross-binding reuse fails typed (`cursor-mismatch`) with an explicit
  restart/resync choice.
- Credential selection reuses explicit env + file checks; no new secret
  store (credential material lives in `bashy secret` / explicit env files
  owned outside this package). Redaction validated with canary secrets
  (`errors_effect_redact_test.go`, `fixture_test.go`: argv, provider error
  bodies, fixture failures).
- Login never implies provisioning permission: `ProvisioningPermitted`
  refuses offline even with valid credentials (tested).

## Deferred / unclaimed (not absent)

- `iam:write` — UNCLAIMED until tested by a separate authorization story;
  identity/policy writes are never inferred from login.
- `account:set` — UNCLAIMED until tested; account-level provisioning is
  never inferred from login.

## Verification status

- Fixture tests (this repo, no network, no credentials): PASS —
  `go test -short -count=1 ./external/aws/ ./external/registry/`
  (see commit message for exact exit codes). Covers refused
  provisioning, explicit profile selection and ambiguity refusal, redacted
  scope, opaque-token pagination incl. cross-binding/foreign-token
  rejection, JSON/NDJSON envelopes with the single `schema_version`
  discriminator, EC2/S3 provider-validated dry-run previews plus IAM
  refusal, typed-error exits, canary-secret redaction, short-lived
  runtime, fake-binary read (two recorded pages), create/delete
  round-trip with cleanup, failure redaction, cancellation/reaping, and
  the adapt dry-run mapping (`--dry-run` / `--dryrun`) and refusal.
- Registry gate: `TestNoDefaultRegistryEntryLacksPin` unaffected (aws is
  host-first `PreferHost`, same posture as gcloud); atlas platform
  declaration added (`pkg/atlas/platform.go`, one row) so
  `TestEveryExternalDeclaresPlatforms` stays green.
- LIVE AWS CHECKS — MOVED TO SPRINT 465 (NOT attempted here): no real
  account or credential exists in this environment, so the one read + one
  reversible write with cleanup, real pagination, denied/expired-auth
  against the real API, release selection, and committed sha256 pins were
  NOT performed. No live support is claimed or inferred.
- Go-side dependencies: this story adds NO new module dependencies
  (`go.mod`/`go.sum` untouched — the package reuses `binmgr`,
  `coreutils/pkg/weavecli`, `policy/audit`, `notrack` only).

## Acceptance evidence (story bedc320542eb, aws third)

| Acceptance item | Evidence (test / file) |
|---|---|
| aws via existing external registry | `external/registry/aws.go`, `TestAwsEntry` / `TestAwsResolveRefuses` |
| Apache-2.0 source + host-first, no unverified download | `TestAdmissionConsts`, `TestResolveRefusesEveryVersion`, `TestPathMissingIsActionable` (`aws_test.go`) |
| Explicit profile credentials, no ambient fallback | `auth.go`, `TestResolve*` + `TestRedactedScope` (`auth_test.go`) |
| JSON pagination, single `schema_version` discriminator, bounded queries | `page.go`, `TestPaginateFirstAndResume` / `TestTokenCrossBindingRejected` / `TestForeignAndRangedTokens` / envelope tests (`page_test.go`) |
| Dry-run where supported (`--dry-run` EC2, `--dryrun` S3), refusal elsewhere | `effect.go`, `TestDryRun`; adapt rows `aws.yaml` (unsupported) + `aws-ec2.yaml` / `aws-s3.yaml` (provider-validated-preview), `TestFixtureAdaptDryRun` |
| 446 adapter contract (loom-v2 envelope, preview levels) | `errors.go` (typed codes on the shared exit mapping) |
| 449 discovery (cloud tier taxonomy) | registry `Tier: 6`, atlas row `pkg/atlas/platform.go` |
| Error handling and redaction (offline half) | `redact.go`, `TestRedactArgsCanary` / `TestRedactTextCanary` / `TestFixtureFailureRedaction` |
| Per-client runtime (short-lived only) | `ClientRuntime`, `TestClientRuntimeIsShortLived` |
| Supported OS/arch | host-first on all three OSes (atlas row); executed darwin/arm64 only, other platforms vendor-claimed, not executed |
| Artifact verification | fail-closed: no pin, no download; host identity recorded at qualification time; pins owned by Sprint 465 |
| Read and reversible-write evidence (offline fixtures) | `TestFixtureReadAndPagination`, `TestFixtureReversibleWrite` (`fixture_test.go`); live goes to Sprint 465 |
| Never infer provisioning permission from login | `ProvisioningPermitted`, `TestProvisioningNeverInferredFromLogin` |

Moved to Sprint 465 (NOT attempted here): read and reversible write with
a real account, cleanup, real pagination, denied/expired-auth error
handling against the real API, release selection, and committed sha256
pins.
