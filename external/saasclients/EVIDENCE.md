# Salesforce / Stripe / Twilio / Slack evidence — story c512b342864d (sprint 457, seq 2294)

Yoke-side admission record. Fixture/loopback tests are distinct from live support:
nothing here ran against a real account, credential, tenant or network service.

## Admission (reviewed 2026-10-18; release API digests, tag-ref commits)

| Client | Repo | License | Release | Immutable commit | Managed? |
|---|---|---|---|---|---|
| sf | salesforcecli/cli | Apache-2.0 | 2.156.4 | 54d34873cb87d1c0093b9f4dbd6c9756ff300e4b | host-only (Node/oclif tarball, runtime bundled) |
| stripe | stripe/stripe-cli | Apache-2.0 | v1.54.0 | 7839c312a40ed89ecc48d16fc5fb8bca7dbfac49 | managed GitHub archive, host-first |
| twilio | twilio/twilio-cli | MIT | 7.2.0 | 9c88e3b8e4ef44b5e9f1f11c967cfce8ccea8ca8 | host-only (release ships installers only, no archive) |
| slack | slackapi/slack-cli | Apache-2.0 | v4.10.0 | e7ff104a324c4914e492eebaa7ceb34705a10460 | managed GitHub archive, host-first |

Per-asset SHA-256 (GitHub release digest) for the reviewed platform artifacts is in
`clients.go` (`Digests`). Mode: short-lived client only (supply-chain Rule 2).
Remote services (Salesforce, Stripe, Twilio, Slack) are proprietary hosted services
under the account holder's own agreement; client admission is not service approval.

## Per-client product/API scope (ops.go `Ops`)

A development CLI is not assumed to expose every CRUD verb. Mapping is from vendor
reference documentation and was NOT executed live:
- sf: query + single-record get/create/update/delete via CLI; single-record upsert by
  external id is bulk-CSV only in the CLI, so it uses the documented REST adapter.
- stripe: resource commands cover read/create/update/delete with `--idempotency`.
- twilio: `api:core:*` covers the REST resources; no provider idempotency.
- slack: the CLI is app tooling (manifest/deploy/triggers/datastore); history, channel
  list, message post/delete use the documented Web API adapter (api.go).

## Acceptance evidence

| Item | Evidence |
|---|---|
| Account/project selection explicit, no ambient fallback | `scope.go`; `TestResolveScopeExplicit`, `TestStripeKeyModes` (live key refused) |
| Structured errors | reuses `external/gws` typed codes + loom-v2; `TestAdapterTypedErrorsBoundsAndRedaction` |
| Bounded output | `API.MaxBytes` read-limit with explicit `output-limit` error; same test |
| Idempotency for mutations | `Mutation.Validate`, `DeriveKey`, `Ledger`, per-op `Idem` strategy; `TestMutationValidation`, `TestSlackSandboxPostCleanup` (replay refused), `TestSalesforceUpsertIdempotentWithCleanup` |
| Sandbox read/write cleanup, fixtures only | loopback fakes: Slack post -> delete -> not-found; Salesforce upsert x2 -> one row -> delete |
| Never send real messages/payments | real-world ops (`EffectRealWorld`) refused unless scope is a declared sandbox; Stripe live keys refused; non-sandbox post never reaches provider (test) |
| Webhook forwarding/server separate mode | `modes.go` `ClassifyArgs`/`AdmitMode`; `TestModeAdmission` (stripe listen, twilio serverless:start / webhook URL repoint, slack run, sf apex tail / login web all refused, exit 2) |
| Redaction canaries | `TestAdapterTypedErrorsBoundsAndRedaction` (provider-echoed token, argv) |
| Registry entries | `external/registry/saasclients.go`, `TestSaasClientEntries`; atlas rows in `pkg/atlas/platform.go` |

## Atlas external-count delta

+4 externals (`sf`, `stripe`, `twilio`, `slack`), one registry entry and one
`pkg/atlas/platform.go` row each. No new commands beyond these four front doors.
go.mod/go.sum untouched (no new Go dependencies).

## Named unverified gaps (move to Sprint 465)

- Live read + reversible write with cleanup against a real Salesforce sandbox org,
  Stripe test-mode account, Twilio test account and Slack sandbox workspace; denied /
  expired-auth against the real APIs. No credentials exist here.
- Live confirmation of the CLI command mapping in `Ops` against each pinned binary.
- Vendor telemetry opt-out: only `SF_DISABLE_TELEMETRY`/`SF_AUTOUPDATE_DISABLE` (sf) and
  `STRIPE_CLI_TELEMETRY_OPTOUT` (stripe) are set; Twilio and Slack have no verified
  switch and no no-outbound-connection demonstration was run.
- No sha256 pins in `pkg/binmgr/pins.go`, SBOM or dependency audit of the shipped
  artifacts, bundled-runtime (Node) licenses for sf/twilio. Managed download for
  stripe/slack is opt-in via `$STRIPE_VERSION`/`$SLACK_VERSION` only.
- sf and twilio have no managed archive (host-only).
- Durable cross-process idempotency ledger (the `Ledger` is in-memory); durable webhook
  receivers need their own policy admission and are not admitted here.
- Twilio test-credential/magic-number behavior and Stripe test-clock behavior not exercised.
