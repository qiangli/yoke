# Permitted capacity and Sprint 138 demo

`bashy sprint monitor 138 --json` shows the selected sprint alongside shared host
workloads and complete matching account pools. `--watch --json --duration 60s`
emits bounded changes and heartbeats. `models usage`, `models limits`, and
`models budget` expose source classifications and windows without consuming
admission. `models budget configure --file policy.json` validates; `--apply`
explicitly publishes the policy. Account filters preserve shared competitors.

For a deterministic host-pressure plus low-quota demonstration, the Bashy
`TestSprintMonitorAlertsSustainRecoverAndSurviveHandoff`,
`TestSprintMonitorQuotaWindowsRemainDistinct`, and
`TestSprintMonitorAlertPublicationReplaysWithoutConsumingInbox` fixtures show
attribution, sustained warning, shared-pool windows, owner handoff, and recovery.
A warning requires 15 seconds of fresh high samples; recovery requires 30 seconds
below the lower threshold. Stale data never becomes a zero or clears pressure.
Mitigation is explicit: inspect the named competing work, change admission
policy, pause owned work, or inspect cleanup eligibility. Observation never
pauses, prunes, dispatches a job, or acknowledges the recipient's inbox.
`weave` cleanup remains guarded by the ownership/dirty/active/shared checks in
[resource lifecycle](../weave/resource_lifecycle.md).

Remote setup is explicit on both the sender and receiver. Register a reach alias
using the existing fleet host catalog. Create `remote-capacity-policy.json` in
the fleet state directory, or select a file with
`BASHY_REMOTE_CAPACITY_POLICY`. A minimal shape is:

```json
{"version":1,"local_worker":"builder-1","targets":[{"name":"my-builder","worker":"builder-1","observe":true,"dispatch":true,"data_classes":["public-fixture"],"workspace":"/absolute/existing/clean/checkout","revision":"EXACT_GIT_COMMIT","toolchain":"EXACT_GO_RUNTIME/GOOS/GOARCH","slots":1,"memory_bytes":1073741824}]}
```

The receiver's `local_worker` must match the requested logical worker. Reach
addresses stay in the existing fleet alias. Observation and dispatch are separate
permissions; dispatch also requires an allowed data class. No permission is
inferred from registration. Toolchain means the Bashy embedded Go runtime and
OS/architecture used by its shell executor, not an attestation of arbitrary
external compilers. External tasks additionally require matching `executables`
arrays in the request and target policy, each entry `{"path":"/absolute/resolved/compiler","sha256":"EXPECTED_SHA256"}`. The receiver verifies up to eight regular executables before and after the run (256MiB each, 512MiB total, three-second deadline). Parsed static direct calls are pinned to those absolute paths, preventing PATH shadowing; undeclared or dynamic direct calls stay queued. Returned provenance contains the verified inventory. This proves declared executable bytes, not arbitrary descendant executables, package dependencies, libraries, or sandbox isolation.

The receiver independently checks its checkout (including
untracked/ignored files), runtime, and native resource sample before execution.
Estimated or stale CPU/memory headroom—including Darwin sources when estimated—
is refused. Guarded execution is supported on Linux/macOS; Windows remains
explicitly queued because its process-group/boot-identity proof is unavailable.
Authorized observation works independently. Capacity is atomically reserved on the receiving host, clamped to
both policy and observed headroom. A sender's local file lock has no cross-host
authority.

`bashy sprint monitor --host my-builder --json` invokes the permitted observation
endpoint. To execute, write a request with version 1, unique `id`, `target`,
`data_class`, exact `revision`/`toolchain`, a bounded shell `body`, and existing
`TaskSpec` fields (`schema_version:1`, task, venue `userland`, distribution
`single`, explicit `cpu_per_task`/`mem_per_task`, and `timeout_ns` <=10 minutes).

```sh
bashy dag capacity plan --request request.json
bashy dag capacity dispatch --request request.json
bashy dag capacity queued
bashy dag capacity reconcile --target my-builder --run REQUEST_ID
```

Plan is read-only. Refused dispatch persists a local pending request; listing it
never retries or launches it. Only the explicit dispatch action sends the body.
No checkout, dataset, or cloud resource is provisioned. Returned output is a
bounded 64KiB artifact with SHA256, logical worker, exact revision/runtime,
request digest, and the existing RunRecord. Provenance mismatch remains an
explicit unverified result, even if the body exited successfully.

A parsed constrained executor (static printf/echo/true/false/sleep/:, no external
fallback) and a process-group guard allow proven child cleanup. Arbitrary
external commands conservatively retain capacity because portable process groups
cannot prove that descendants never escaped. `capacity_held` and a reconciliation
action are returned. Reconciliation checks the target's durable receipt and
native identity; external descendants require a verified receiver boot change.
It never blindly frees demand on wrapper death or TTL. Durable admission,
reconciliation, and inbox-idempotency receipts are authority records, retained
for replay protection rather than rotating observation history.

`go test -race ./pkg/dag -run '^TestCapacity'` exercises the real SSHTransport with
a permitted local subprocess endpoint: allowed/denied/unregistered/unreachable
placement, incompatible platform/runtime, racing receiver claims, output and
revision mismatch, child escape retention, verified-boot reconciliation replay,
timeout records, and durable local fallback. These are deterministic fixtures,
not live vendor telemetry, an actual remote-host reboot, or a real remote
provisioning/dispatch claim.
