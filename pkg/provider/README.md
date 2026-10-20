# Provider packages: manifest and protocol reference (S472.14)

Version: `provider/v1` (the only protocol version this module speaks).
Module: `github.com/qiangli/yoke/pkg/provider` — a nested stdlib-only
module importable by Go 1.26. New code under `pkg/provider` imports only
the standard library and other `pkg/provider` packages.

A provider package provisions host/VM infrastructure. It has one of two
forms. Both speak the same `Request`/`Result` schema; they differ only in
where the behavior runs.

- **declarative**: data only (endpoints, request templates, lifecycle
  mapping), interpreted by the built-in engine. The master runs it with
  exec disabled. Authoring and validation need no local machine.
- **executable**: an argv-invoked program. The `Request` goes as one JSON
  document on stdin, one `Result` comes back on stdout, diagnostics go to
  stderr. It runs only on user-owned compute (local bashy or an existing
  satellite). The master never executes user code.

`kind` is always `infra` (fleet kind and bashy verb of record, S472.0).

## Manifest

| field | declarative | executable | notes |
|---|---|---|---|
| `kind` | `infra` | `infra` | required |
| `name` | string | string | required, unique in the ring |
| `form` | `declarative` | `executable` | required |
| `version` | string | string | optional, informational |
| `protocol_version` | `provider/v1` | `provider/v1` | required, must match exactly |
| `summary` | string | string | optional |
| `capabilities` | op list | op list | required; one capability names one operation |
| `argv` | absent | command line | exec'd directly, never through a shell |
| `declarative` | document below | absent | schema owned by the engine |
| `builtin` | marker | n/a | loader-level only (fleet): a compiled-in reference entry dispatched to the linked-in Go adapter, never the engine |

Capabilities name lifecycle operations: `validate`, `capacity`, `plan`,
`create`, `inspect`, `list`, `start`, `stop`, `destroy`, plus the optional
`reboot`, `resize` and the feature flags `volumes`, `spot`. An operation
outside the manifest is refused with `unsupported_capability` before any
provider call. Every `Op` is also a capability; `plan` is rendered locally
and takes no endpoint.

Credentials are never manifest fields. They are references resolved by
the caller at open (once, held in memory, scrubbed from journals and
logs), placed on the wire only as the manifest's `auth` declares.

## Declarative document

```yaml
base_url: https://api.example.com
auth: {in: header, name: Authorization, value: "Bearer ${credential}"}
headers: {X-Owner: "${owner_tag}"}
resource: {id: id, name: name, status: status, region: region,
  size: size, tags: tags, public_ips: ips, created_at: created_at,
  retained: retained}
states: {new: pending, running: running, stopped: stopped, deleted: deleted}
operations:
  validate: {method: GET, path: /api/account}
  capacity: {method: GET, path: /api/regions}
  create: {method: POST, path: /api/machines, result: machine,
    body: {name: "${name}", region: "${spec.region}",
      size: "${spec.size}", image: "${spec.image}", tags: "${tags}"}}
  inspect: {method: GET, path: "/api/machines/${resource_id}", result: machine}
  list: {method: GET, path: /api/machines, result: machines}
  start: {method: POST, path: "/api/machines/${resource_id}/power",
    body: {action: start}, result: machine}
  stop: {method: POST, path: "/api/machines/${resource_id}/power",
    body: {action: stop}, result: machine}
  destroy: {method: DELETE, path: "/api/machines/${resource_id}", result: machine}
```

Rules the engine enforces at registration (`ValidateDeclarative` — the
same function the master calls, so registration and the engine agree):

- `base_url` is http(s); `auth.in` is `header` or `query`;
  `auth.value` must place `${credential}` (default `"${credential}"`).
- `${credential}` appears nowhere else: not in headers, paths, query,
  `result` or bodies.
- `resource.id`, `resource.status` and `resource.tags` are required;
  paths are dot-separated keys (array indexes are digits), no functions.
- Every declared capability except `plan` has an endpoint with a valid
  method and a `/`-rooted path; `create` needs `list` (idempotent create
  looks the resource up by its tag). Unknown fields are rejected.
- Templates are pure substitution (`${field}` from a closed
  placeholder set: `credential`, `resource_id`, `idempotency_key`,
  `idempotency_tag`, `owner_tag`, `name`, `tags`, `spec.name`,
  `spec.region`, `spec.size`, `spec.image`, `spec.user_data`). An unknown
  placeholder is an error. Nothing is evaluated: no functions, no shell.

The engine keeps the reference discipline: every created resource
carries the `bashy-lease` tags (lease marker, owner, hashed idempotency
key, temporary marker when authorized); a replayed create finds its
resource by tag instead of allocating another; an existing resource is
acted on only when it carries this owner's tags.

## Executable protocol

`argv[0]` is exec'd directly with the JSON `Request` on stdin:

```json
{"protocol_version": "provider/v1", "op": "create",
 "idempotency_key": "boot-1", "spec": {"name": "sat", "region": "r1"}}
```

and answers one JSON `Result` on stdout:

```json
{"protocol_version": "provider/v1", "op": "create",
 "resource": {"id": "mach-1", "name": "sat", "state": "running"}}
```

Every hook — `validate`, `capacity`, `plan`, `create`, `inspect`,
`list`, `start`, `stop`, `destroy` — follows this shape. Failures are
normalized `{"error": {"code": "...", "message": "..."}}` results (see
below). A result whose `op` differs from the request's is refused with
`protocol_mismatch`, so a plugin cannot substitute one operation for
another. Mutating ops require an idempotency key; `inspect`, `start`,
`stop`, `destroy` (and `reboot`/`resize`) require a resource ID.

## Errors, identity, authorization, billing

Normalized error codes: `unsupported_capability`,
`confirmation_required`, `not_owned`, `not_found`, `invalid_request`,
`invalid_credentials`, `protocol_mismatch`, `rate_limited`, `timeout`,
`provider_error`, `needs_manual_reconcile`, `exec_disabled`. Every
adapter reports a failure in `Result.Error` and returns it as the Go
error, so callers may use either channel.

Operation identity: one idempotency key names one operation on one
resource. Replaying a key answers from the journal without acting
again; carrying a key to another resource is refused. A create whose
answer is lost is reconciled by idempotency tag before any retry, so at
most one resource results.

Destroy authorization (amendment A6): destroying a persistent resource
needs a per-operation confirmation bound to exactly that resource ID
and that idempotency key; without it the destroy is refused before any
provider call and the resource keeps its ongoing cost. A confirmation
is single use by construction. A temporary resource (TTL or work-bound
authorization recorded at create and tagged on the resource) is
destroyed under that authorization with no confirmation; a temporary
authorization on a resource not created as temporary is refused.

Stop versus destroy: stop powers the resource off — state `stopped`,
data retained, billing ongoing until destroyed. Stop never deletes; a
stop that reports `deleted` is a destroy on the stop path and fails
conformance. Destroy reports `deleted` plus what is retained (and still
billed).

## Paths: cloud-only and executable

Both paths are described only in terms of the public dhnt.io protocol:
access registration first, then the dhnt.io satellite provisioning
workflow. The two-stage example: create the host with the provider, then
the satellite installs its proprietary artifact from dhnt.io.

- **Cloud-only (declarative):** author a declarative manifest (no local
  machine needed), register it at dhnt.io, provision there. The
  manifest is validated at registration by `ValidateDeclarative` and
  runs on the built-in engine with exec disabled. Validation status is
  shown; dhnt certification is not a gate. Sharing via Git or a local
  directory needs no hosted registry.
- **Executable:** register locally, run from local bashy or an existing
  satellite (user-owned compute only). The master never runs it.

## Authoring and conformance

- `infra init --form declarative|executable --name NAME --dir DIR`
  writes the scaffold: manifest (+ executable skeleton implementing
  every standard hook for the executable form), `config.example.json`
  and deterministic fixtures.
- `infra validate [DIR]` checks schema, capabilities, structured
  errors, timeout handling and operation identity without allocating
  anything. The declarative check is `ValidateDeclarative`, the same
  function the master calls at registration.
- `pkg/provider/conformance` is the reusable suite both forms must
  pass: states and transitions, idempotency and reconciliation,
  normalized errors, unsupported operations, stop-versus-destroy
  billing and data semantics, and A6 destroy confirmation.
  Fixture-only by default (`WithLive(auth)` enables the allocating
  lifecycle); never touches the network beyond httptest.

## Future work (blocks nothing)

Maintained Lambda/AWS/GCP/Azure adapters and Runpod container-worker
semantics are future work. The DigitalOcean adapter is the reference
example; dhnt certification of third-party providers is not a gate.
