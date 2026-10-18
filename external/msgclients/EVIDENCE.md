# msgclients evidence — story 518107b4dfc7 (sprint 457, messaging)

Admission and verification record for the messaging clients mosquitto
(mosquitto_pub/mosquitto_sub), rabbitmqadmin v2 and kcat. Source
inspection, licence admission, offline fixture tests and live support are
distinct rows; a registered command, a discovered host binary or a passing
fixture is NOT live support. No live support is claimed or inferred for
any client.

## Admission

| Client | Client licence (SELECTED option pinned) | Mode | Provisioning | Broker (usually) | Broker licence — SEPARATE, not granted by the client |
|---|---|---|---|---|---|
| mosquitto (mosquitto_pub/sub) | BSD-3-Clause | short-lived client | host-only (PreferHost) | Eclipse Mosquitto broker, or any MQTT v3.1/v3.1.1/v5 broker | EPL-2.0 OR EDL-1.0 for the Eclipse Mosquitto broker; other brokers/managed services carry their own terms |
| rabbitmqadmin (v2) | MIT (selected of MIT OR Apache-2.0) | short-lived client | host-only (PreferHost) | RabbitMQ server with the management plugin (or compatible) | MPL-2.0 for the RabbitMQ server |
| kcat | BSD-2-Clause | short-lived client | host-only (PreferHost) | Apache Kafka cluster (or wire-compatible) | Apache-2.0 for Apache Kafka |

- Mode: "external user-managed endpoint" of the client admission
  contract. Brokers are never installed, started, restarted or
  provisioned; no listener or resident child exists. Every subscription
  is bounded (message, byte and time limits with finite defaults and
  caps); there is no spelling for an unbounded listener.
- Broker startup is NOT admitted: the Mosquitto selection covers the
  mosquitto_pub/mosquitto_sub clients only. Starting `mosquitto` (the
  broker) would be a managed server under service Rule 1 and stays
  disabled.
- Persistent subscribers are classified as long-lived services: they
  require service Rule 1 (permissive source, Bashy-built signed
  artifact) and are not admitted by this story. A timeout does not turn a
  subscriber into a client; bounded reads here always terminate.
- Provisioning: no sha256 pin for any of the three exists in
  `pkg/binmgr/pins.go`, so `Resolve` returns an install hint and nothing
  is downloaded (same posture as gcloud/dataclients). Rule 2 pinned
  releases are a named gap.
- Source/commit/artifact hash/SBOM/transitive-licence review of each
  client binary: NOT performed (no network, no artifacts in this
  environment). Named gap, owned by a later admission pass; licences
  above are the story-pinned selections, unreviewed against the shipped
  artifacts.
- Runtime dependencies (declared, unverified): mosquitto clients link
  libmosquitto (same BSD-3-Clause selection) plus libc; rabbitmqadmin v2
  ships as a standalone install per vendor docs; kcat bundles
  librdkafka (BSD-2-Clause). Dependency/notice review is a named gap.

## Consumed shared contracts (no private ABI)

- Errors: `external/gws` typed codes over the loom-v2 envelope
  (`weavecli.EmitError`, single `schema_version`); exits 2
  invalid/unsupported, 124 timeout, 130 cancelled, 1 provider/auth
  failures and partial-unknown publish outcomes. No 457 error ABI added.
- Redaction: `gws.RedactArgs` (`policy/audit.Redact`) + literal masking.
- Schema: `msgclients.Schema()` is a `fleet.CommandSchema`; CLI and MCP
  both hand bound values to the single `Execute`.
- Credentials: explicit `<PREFIX>_PASSWORD` / `_PASSWORD_FILE` (as
  materialised by `bashy secret`); no new store. Carried to the child by
  env (rabbitmqadmin), a 0600 staged config file (kcat `-F`), or the
  first stdin line consumed as the interactive password prompt
  (mosquitto `-u`); never argv. Ambient `MQTT_*`/`MOSQUITTO_*`,
  `RABBITMQ_*`, `KAFKA_*`/`KCAT_*`/`RD_KAFKA_*` variables are scrubbed
  from the child env, so there is no ambient fallback.
- Credential policy: mosquitto and kcat allow anonymous access; a USER
  without a PASSWORD is refused (the operation cannot prompt), as is a
  PASSWORD without a USER. rabbitmqadmin requires both (the management
  API has no anonymous mode).
- No new Go module dependencies.

## Behaviour and where it is tested (`msgclients_test.go`, offline)

| Requirement | Evidence |
|---|---|
| Explicit broker only, no default localhost/ambient | `TestResolveEndpoint`, `TestEnvScrubbedOfAmbientVars` |
| TLS verify by default; plaintext only to loopback; explicit CA file where the client needs one | `TestResolveEndpoint`, `TestTLSArgs` |
| Structured payload preservation (framed text, JSON numbers verbatim, multiline, nulls) | `TestRunRecordedFixtures` (`testdata/`), `TestParseRejectsGarbageJSON` |
| Bounded subscription (server count flags plus wrapper message/byte/deadline limits; explicit partial; no persistent mode) | `TestSubscribeAlwaysBounded`, `TestBoundedReads` (incl. an unbounded producer) |
| Cancellation / deadline kill the child; typed 130/124 | `TestCancellationAndTimeout` |
| Explicit publish effects (publish reports `published`; subscribe `none`; interrupted publish is partial-unknown) | `TestPublishEffectsExplicit`, `TestCancellationAndTimeout` |
| Credential redaction with canary (argv, stdin framing, staged files, scope, explain, provider error body, envelope) | `TestBuildSecretsNeverInArgv`, `TestRedactionCanary`, `TestAuxFilesStagedAndRemoved` |
| CLI/MCP parity and pre-execution rejection; `--explain` never executes | `TestExecuteParityAndExplain`, `TestBuildRefusals`, `TestPublishBinSelection` |
| Registry entries, host-only refusal, separate broker licence text | `external/registry/msgclients_test.go` |
| Atlas platform rows | `pkg/atlas/platform.go`; `TestEveryExternalDeclaresPlatforms` |

Fixtures are HAND-AUTHORED from the clients' documented output formats,
not captured from a live broker (none is allowed here). They prove the
parsing, bounds and redaction logic, not that a real client emits exactly
these bytes.

## Declared limits

- Output representation: mosquitto records are `{topic, payload}` from
  `-F '%t\x1f%p\x1e'` framing; kcat records are the `-J` JSON objects
  verbatim; rabbitmqadmin records are the provider JSON values verbatim
  (a lone array is flattened). Invalid UTF-8 in payloads is replaced on
  JSON encoding (declared lossy). No resume cursor (no
  snapshot/continuation token exists): partial results say so and the
  caller narrows the topic or the bounds.
- rabbitmqadmin `get` uses `ack_requeue_true`, so a bounded subscribe is
  a peek, not a destructive read. An interrupted publish is reported
  `partial-unknown-mutation`: cancelling locally cannot guarantee broker
  state.
- QoS (`--qos 0..2`) exists for mosquitto only and is refused for the
  other two; `--exchange` exists for rabbitmqadmin publish only (empty =
  the AMQP default exchange) and is refused elsewhere and on subscribe.
  Kafka keys/headers, MQTT v5 properties, retained-message control and
  mutual-TLS client certificates are unclaimed.
- Wiring the typed op as a registered, schema-bound command (CLI) and an
  MCP tool is consumer-side (bashy `interp.BindCommandSchema` / MCP
  bridge) and calls `msgclients.Execute`; the three `bashy <client>`
  front-doors added here are raw passthrough and enforce none of the
  above. The mosquitto front-door covers two binaries: the typed op
  dispatches pub/sub by mode; the raw front-door resolves host binaries
  and refuses with an actionable hint when they are absent.

## Named gaps — moved to Sprint 465 (NOT attempted; no account, credential, broker or tenant)

- Live connections to a real MQTT / RabbitMQ-management / Kafka
  endpoint over TLS (certificate and hostname verification, expired and
  denied auth, rate limits).
- Capturing real output fixtures from each client version and confirming
  flag spellings against them: mosquitto `-s` whole-stdin publish,
  `-F` framing, `-C` count, `-u` username; rabbitmqadmin v2 `publish`
  (`--exchange`, `--routing-key`, `--payload-file`) and `get`
  (`--queue`, `--count`, `--ackmode`, `--format`) shapes; kcat `-P -D`,
  `-C -o beginning -e -c`, `-J`, `-F` shapes.
- Credential handoffs MUST be confirmed before any non-test credential
  is used: the mosquitto first-stdin-line password framing (if the real
  binary does not consume it as the password prompt, the password would
  be published as message content), kcat `-F` config acceptance, and
  whether rabbitmqadmin v2 honors `RABBITMQADMIN_PASSWORD` (else move to
  its config-file mechanism).
- A reversible publish with cleanup, and verification that a killed
  client leaves no committed broker change.
- Rule 2 sha256 pins / SBOM / vulnerability audit and Windows evidence
  per client.
- Mutual-TLS client certificates, Kafka keys/headers, MQTT v5
  properties, and retained-message semantics.

## Atlas consumer

Commands added to the documented external count (bashy
`docs/command-atlas.md`, `TestDocumentedExternalCount`, which lives in
bashy — not this repo): `mosquitto`, `rabbitmqadmin`, `kcat` (+3).
