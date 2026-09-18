# Shared model usage and admission

`CollectReport` reports configured models and agents, including unused/unsupported
entries, grouped by explicitly bound provider/account/pool/billing lane. Model
filters retain the full matched pool and competing active run attribution. A nil
metric value is unknown; source, classification and observation/window/reset
timestamps qualify every number. Local observations and organization aggregates
can overlap and must never be added together. Host filtering belongs to the
caller’s projection and cannot reduce shared account demand.

`Preview` reads state without consuming counters, rate buckets or reservations.
A successful preview grants no capacity. Immediately before work, acquire an
`OwnerLease` and call `Reserve` with a stable operation ID. Only `Action == Allow`
**and a non-nil Reservation** authorize that operation. `RouteAlt`/`Downgrade`
require a new reservation for the selected target. Premium permission cannot
bypass explicit hard policy. Reserve separate launch-concurrency and inference
usage axes to avoid counting launch capacity on every model call.

Renew while work is active, settle exactly once with actual usage, or release
when no usage occurred. Input includes cached input; cached tokens are a subset,
not another charge. Matching settlement/release retries are idempotent;
conflicting operation reuse fails. If actual cost is absent, its estimate remains
conservatively charged and current hard spend policy blocks on uncertainty.
`ReconcileTerminated` requires caller-verified owner/run/host identity and
termination evidence. It releases capacity while retaining estimated consumption
for work that may have used the model. Supervisor death and TTL expiry alone do
not reclaim anything: detached work may still be running.

## Policy

`Config.Policy` supports embedding and tests. Otherwise resolution is
`Config.PolicyPath`, `BASHY_LLM_BUDGET_POLICY`, then `llm-budget-policy.json` beside
the existing meter (`BASHY_LLM_BUDGET_STATE`, normally `~/.bashy/llm-budget.json`).
An explicitly selected missing file is an error. The absent default policy keeps
legacy unconstrained behavior only until a hard policy has been used. Thereafter
its disappearance fails closed. To intentionally remove constraints, publish a
valid explicit version-1 policy with an empty constraints array.

```json
{
  "version": 1,
  "bindings": [
    {"model":"model-one","provider":"openai","account":"org-example","pool":"org-api","lane":"api-key"},
    {"model":"model-two","provider":"openai","account":"org-example","pool":"org-api","lane":"api-key"}
  ],
  "constraints": [
    {"provider":"openai","account":"org-example","pool":"org-api","daily_tokens":1000000,"daily_spend_micro_usd":5000000,"concurrency":2},
    {"host":"worker-a","host_slots":2,"memory_bytes":8589934592}
  ],
  "routes": [],
  "sources": []
}
```

Model names must be canonical catalog names. Account/pool values are explicitly
configured opaque identities, never credentials. Subscription and API lanes are
separate. Omitted limits are unknown/unconfigured; zero is a real enforced cap.
Daily/Monday-week policy windows use the gate clock’s location. Host constraints
are allocations on the state authority’s host. Machines with separate meters
must receive conservative explicit allocations; this package does not claim a
global distributed quota. Source observations are reporting evidence, while
configured local policy is the admission authority; unknown external consumption
prevents this from guaranteeing a provider-wide quota.

## Documented sources

Both source kinds require `enabled:true`, explicit provider/account/pool/lane and
unique `id`. Refreshes share a disk cache and a per-source kernel lock; retries
honor cadence, deadlines and backoff even under explicit refresh. No source makes
an inference call, searches harness credential stores or enumerates accounts.

* `claude-statusline`: reads `path`, an explicitly installed local bridge file:
  `{"schema_version":"bashy-claude-statusline-v1","observed_at":"2026-09-08T12:00:00Z","account":"configured-account","payload":{...}}`.
  `payload` is the documented [Claude statusline stdin object](https://code.claude.com/docs/en/statusline).
  An operator-owned bridge atomically writes the envelope from statusline stdin;
  the adapter does not install or execute one. It checks account and timestamp,
  reads quota percentages/resets, current-context usage and estimated session
  cost. Current-context token counts are never treated as cumulative usage.
* `openai-organization`: opt-in GET-only [completion usage](https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/completions)
  and [organization costs](https://developers.openai.com/api/reference/resources/admin/subresources/organization/subresources/usage/methods/costs).
  Set provider `openai`, lane `api-key`, explicit organization-wide account/pool,
  optional `organization` header and `credential_ref`. Use `env:YOUR_ADMIN_KEY_VAR`
  or an embedding application's configured `CredentialResolver`; a scoped admin
  credential with access to these endpoints is required. Organization totals
  include covered external usage; costs are billing observations, not a quota or
  balance. Pagination is bounded and incomplete totals are withheld. Credentials
  only go to the fixed official origin, with redirects disabled.

Other source kinds and unavailable vendors remain explicit unsupported/unknown
rows. A bridge/API parser being implemented does not mean the operator has
configured live account access. Tests use official-schema fixtures and fake
HTTP transports, never real account calls.

## Persistence and recovery

Every mutation takes a stable kernel lock, reloads current state, evaluates all
account/host axes, and atomically replaces a mode-0600 meter. Legacy `Record`
uses the same transaction; legacy `Check` remains a rate-consuming compatibility
API. Original model/provider/plan counters migrate without inventing account or
input/cache attribution. The versioned meter includes its own content digest.
A version-only `.initialized` marker is published first; losing an initialized
meter is an error. A crash during first initialization may require restoring the
meter from backup rather than guessing that no work ran.

Completed operation receipts stay bounded in the meter (64–128) and are archived
under `<meter>.receipts/<hash-prefix>/<operation-hash>.json`. Only previously
committed completions are archived before compaction, so interrupted archival
leaves duplicate receipts without skipping usage settlement. Receipt lookup is
one deterministic bounded file read; active reservations are capped at 8192.
Archive disk usage grows with completed operations to retain indefinite replay
protection. Keep the meter, marker, receipt tree and stable lock paths together
when backing up; deleting receipts forfeits their replay history. Do not replace
or delete a lock path while any process is using the authority.

Unreadable, malformed, oversized, changed-version or digest-invalid state fails
closed. Restore a coherent backup and verify ended work through the lifecycle
owner; never delete state as a budget reset. Unix publication fsyncs files and
parent directories; Windows uses file sync plus atomic replacement because Go
does not expose directory fsync there. Lock waits honor cancellation and have a
five-second maximum. Legacy `Record` has no error return and logs failed writes;
new callers should use the error-returning settlement API.

`ConfigurePolicy(ctx, file, apply)` returns a count-only `PolicyChange`. With
`apply=false` it validates without writing. Apply publishes the strict, bounded
JSON policy under the admission lock; malformed/unknown fields and unsafe header
text are rejected before replacement. `Request.UnknownTokens` and
`Request.UnknownMemory` preserve opaque demand without large numeric sentinels.
Matching hard constraints refuse unknown demand, including outstanding claims;
verified termination with unknown token usage retains that uncertainty for the
current policy window.

Chat Invoke, PTY/ACP sessions and steers reserve before dispatch. They observe
opaque harness lifetimes, not individual provider requests. Successful harness
completion settles local text/average-price observations with
`Actual.TokensEstimated=true`; account reports label split tokens estimated and
retain unknown token/spend coverage for hard policy. Errors/cancellation retain
claims until an existing lifecycle authority verifies termination; a caller's
cancel signal is not proof. Ordinary successful command completion bounds the
supervised work, with deliberately detached descendants outside the portable
process ownership guarantee. No real provider API is called by these tests.

Scheduler jobs opt into the same authority with `schedule add --expensive` and
optional `--budget-model`/`--budget-memory-bytes`. Unmarked jobs, including ordinary
POSIX at/batch jobs, keep their existing behavior. `Job.WorkBudget` and
`FireJobWithAdmission` expose the same seam to embeddings. Expensive model jobs
have unknown token demand; a memory allocation is explicit or unknown. Capacity
is held until successful command completion; failures retain the claim for
verified reconciliation. Scheduler/model subprocess completion does not yield
provider-authoritative usage.

Concrete chat/PTY/ACP and scheduler subprocess paths verify the inherited owned
process group has disappeared after waiting, before releasing capacity. Direct
parent exit alone is insufficient. Missing group proof and launched Windows work
retain claims for verified reconciliation; no job-object/daemon-escape guarantee
is invented. A confirmed never-started command releases its unused claim.
Injected `Runner` implementations retain the explicit synchronous contract that
a successful Run returns only after their owned work ends. Estimated completion
settlement remains conservative even when cancellation/error has ended a verified
group; the original command error still reaches its caller.
