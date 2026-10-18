# dataclients evidence — story 9f087594f442 (sprint 457, seq 2295)

Admission and verification record for the remote database/warehouse clients
psql, mongosh, valkey-cli and snow. Source inspection, licence admission,
offline fixture tests and live support are distinct rows; a registered
command, a discovered host binary or a passing fixture is NOT live support.
No live support is claimed or inferred for any client.

## Admission

| Client | Client licence | Mode | Provisioning | Remote engine (usually) | Remote licence — SEPARATE, not granted by the client |
|---|---|---|---|---|---|
| psql | PostgreSQL | short-lived client | host-only (PreferHost) | PostgreSQL server or wire-compatible | PostgreSQL (permissive); forks/managed services carry their own terms |
| mongosh | Apache-2.0 | short-lived client | host-only | MongoDB server or wire-compatible | SSPL-1.0 for MongoDB Community Server (not OSI-permissive) |
| valkey-cli | BSD-3-Clause | short-lived client | host-only | Valkey, or Redis-protocol server | BSD-3-Clause for Valkey; Redis 7.4+ is RSALv2/SSPLv1 (8.0+ also AGPLv3) |
| snow | Apache-2.0 | short-lived client | host-only | Snowflake SaaS | proprietary hosted service under the account holder's agreement |

- Mode: "external user-managed endpoint" of the client admission contract.
  Servers are never installed, started, restarted or provisioned; no listener
  or resident child exists. `MONITOR`/`SUBSCRIBE`/blocking commands are
  refused (long-lived, would need service Rule 1).
- Sprint 453: these are remote clients. They do NOT satisfy the embedded
  in-memory/persistent engine, shared-owner or exclusive-claim contracts.
- Provisioning: no sha256 pin for any of the four exists in
  `pkg/binmgr/pins.go`, so `Resolve` returns an install hint and nothing is
  downloaded (same posture as gcloud). Rule 2 pinned releases are a named gap.
- Source/commit/artifact hash/SBOM/transitive-licence review of each client
  binary: NOT performed (no network, no artifacts in this environment).
  Named gap, owned by a later admission pass; licences above are the
  upstream-declared ones, unreviewed against the shipped artifacts.

## Consumed shared contracts (no private ABI)

- Errors: `external/gws` typed codes over the loom-v2 envelope
  (`weavecli.EmitError`, single `schema_version`); exits 2 invalid/unsupported,
  124 timeout, 130 cancelled. No 457 error ABI added.
- Redaction: `gws.RedactArgs` (`policy/audit.Redact`) + literal masking.
- Schema: `dataclients.Schema()` is a `fleet.CommandSchema`; CLI and MCP both
  hand bound values to the single `Execute`.
- Credentials: explicit `<PREFIX>_PASSWORD` / `_PASSWORD_FILE` (as materialised
  by `bashy secret`); no new store. Carried to the child by env only, never
  argv or stdin; ambient `PG*`/`REDISCLI*`/`MONGODB*`/`SNOWFLAKE*` variables are
  scrubbed from the child env, so there is no ambient fallback.
- No new Go module dependencies.

## Behaviour and where it is tested (`dataclients_test.go`, offline)

| Requirement | Evidence |
|---|---|
| Explicit endpoint only, no default localhost/ambient | `TestResolveEndpoint`, `TestEnvScrubbedOfAmbientClientVars` |
| TLS verify by default; plaintext only to loopback; snow HTTPS-only | `TestResolveEndpoint`, `TestTLSArgs` |
| Typed data fidelity (declared per client; big ints/decimals/null preserved) | `TestRunRecordedFixtures` (`testdata/`), `TestParseRejectsGarbageJSON` |
| Bounded reads (rows, bytes enforced while reading; child killed at bound; explicit partial) | `TestBoundedReads` (incl. an unbounded producer) |
| Cancellation / deadline kill the child; typed 130/124 | `TestCancellationAndTimeout` |
| Transaction rollback by default; commit only explicit; unknown outcome on interrupted commit | `TestTransactionRollbackByDefault`, `TestCancellationAndTimeout` |
| Credential redaction with canary (argv, stdin, scope, explain, provider error body, envelope) | `TestBuildSecretsNeverInArgvOrStdin`, `TestRedactionCanary` |
| CLI/MCP parity and pre-execution rejection; `--explain` never executes | `TestExecuteParityAndExplain`, `TestBuildRefusals` |
| Registry entries, host-only refusal, separate remote licence text | `external/registry/dataclients_test.go` |
| Atlas platform rows | `pkg/atlas/platform.go`; `TestEveryExternalDeclaresPlatforms` |

Fixtures are HAND-AUTHORED from the clients' documented output formats, not
captured from a live server (none is allowed here). They prove the parsing,
bounds and redaction logic, not that a real client emits exactly these bytes.

## Declared limits

- Output representation: psql is text (NULL via a per-invocation sentinel →
  JSON null; one result set per statement); mongosh canonical EJSON; valkey
  `--quoted-json`; snow provider JSON. No resume cursor (no snapshot/continuation
  token exists): partial results say so and the caller narrows the statement.
- Read-only is a client-side transaction plus psql `default_transaction_read_only`;
  statements are not SQL/JS-parsed, so a hostile statement can try to escape it
  (e.g. `ROLLBACK; BEGIN READ WRITE`). Use a least-privilege role; this is
  defence in depth, not an authorization boundary. psql backslash meta-commands
  and valkey admin/blocking commands are refused. mongosh statements are
  operator-supplied JavaScript by nature.
- Cancelling locally cannot guarantee remote rollback: an interrupted committing
  write is reported `partial-unknown-mutation`. Rollback relies on the server
  discarding an uncommitted transaction on disconnect (unverified live).
- Wiring the typed op as a registered, schema-bound command (CLI) and an MCP
  tool is consumer-side (bashy `interp.BindCommandSchema` / MCP bridge) and
  calls `dataclients.Execute`; the four `bashy <client>` front-doors added here
  are raw passthrough and enforce none of the above.

## Named gaps — moved to Sprint 465 (NOT attempted; no account, credential or remote database)

- Live connections to a real PostgreSQL / MongoDB / Valkey / Snowflake
  endpoint over TLS (certificate and hostname verification, expired and denied
  auth, rate limits).
- Capturing real wire/output fixtures from each pinned client version and
  confirming flag spellings (`snow sql -i --temporary-connection …`,
  `valkey-cli --quoted-json`, `mongosh --nodb` + `connect(uri, user, pass)`,
  psql `-P null=`/`-R`) against them.
- A reversible write with cleanup, and verification that a killed client
  leaves no committed change.
- Rule 2 sha256 pins / SBOM / vulnerability audit and Windows evidence per
  client (valkey-cli has no official Windows build).

## Atlas consumer

Commands added to the documented external count (bashy `docs/command-atlas.md`,
`TestDocumentedExternalCount`): `psql`, `mongosh`, `valkey-cli`, `snow` (+4).
