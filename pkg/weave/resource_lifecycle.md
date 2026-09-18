# Sprint 138 resource lifecycle

`weave start` is the persistent wrapper. Detached launchers start that command;
they do not acquire a reservation that is released when the launcher returns.
The wrapper takes a nonblocking per-run lifecycle lock and reserves shared
account/host capacity **before** clone, hydration, workspace setup or child
launch. It persists reservation ID/owner and the exact native wrapper birth ID
in each queue claim. A conflicting lifecycle or admission decision returns an
explicit refusal with the existing pending run untouched.

Outer hosts install `WithWeaveResources` for native identity lookup and a shared
cached host-pressure check. Bashy uses `resources.LookupProcessIdentity` and
`resources.ObserveHost`; weave never imports resources and adds no sampler.
Standalone weave fails closed if `BASHY_HOST_ADMISSION_POLICY` is configured but
no host check is installed. Without a native identity hook a launch may proceed
under the ordinary budget policy but its wrapper identity is unknown and cannot
be used for process control.

The durable llmbudget authority atomically enforces account/model concurrency
and configured host slot/memory ceilings. Unknown external harness token/spend
totals and undeclared memory demand are explicit unknowns; applicable hard
ceilings queue them. An advisory route/downgrade is not permission to launch the
original model. The wrapper reserves only an explicit Allow with a reservation.
Renewal runs every 30 seconds for a two-minute TTL. A renewal failure cancels
owned work; expiry or wrapper death never silently refunds capacity.

Normal completion requires synchronous Wait plus proof that the owned Unix
child process group is gone before verified termination settlement. A surviving
grandchild retains the claim, even if its immediate parent exited. Failed spawn
releases unused capacity. Crash, uncertain child survival and the currently
unsupported Windows job-object proof retain capacity for explicit verified
reconciliation. The process-group proof assumes cooperative descendants retain
their assigned group; a hostile child can escape with setsid. This is lifecycle
accounting, not a security sandbox. The queue records `resource_terminated`
only on verified normal termination so cleanup cannot reclaim an uncertain
orphan's workspace.

`weave pause` requires explicit acting conductor identity, durable ownership,
and exact native wrapper birth identity. A legacy PID or reused PID grants no
control. It records pause intent before sending SIGTERM to that wrapper. The
wrapper stops its child, preserves progress with the existing terminal evidence
and auto-commit path, then acknowledges `paused`. A timeout reports pending
progress preservation rather than escalating to a kill and claiming success.
Resume selects only owned paused runs and returns through actual start admission;
it neither waives pressure nor releases an old uncertain claim.

`bashy sprint prune` reports expected apparent file bytes. `--apply` independently
checks the sprint run's birth generation and only reclaims that linked run's
terminal, integrated, inactive artifacts. Unknown/recycled sprint links are
retained. Repository-wide stale worktrees and branches remain observations;
prune does not infer this sprint owns them. Run lifecycle exclusion covers the
entire cleanup; a fresh queue-locked eligibility check and integration proof
precede atomic rename to a private reclaim path. Long deletion happens outside
queue.lock. Symlink traversal below the queue, shared run references, dirty,
untracked and ignored files, live wrappers and uncertain child termination all
prevent deletion. A second dirt check after rename protects edits during the
scan. External processes that deliberately mutate an owned directory after it
is claimed are outside the cooperating-writer protocol. Failed deletion retains
its private reclaim path and reports the failure. Scans stop at two seconds or
100,000 entries and never describe partial bytes as complete. Actual bytes are
apparent regular-file bytes removed, not a promise about physical free space.

Bashy hard host pressure policy is an explicit JSON file selected by
`BASHY_HOST_ADMISSION_POLICY`, with `version: 1`, optional `max_cpu_percent`,
`min_available_memory_bytes`, `min_free_disk_bytes`, and `disk_path`. Actual,
fresh observations are required; `allow_estimated_cpu` and
`allow_estimated_memory` must explicitly authorize platform estimates. No
monitor threshold silently becomes admission policy. A sampled pressure floor
is a preflight check; aggregate reserved demand is bounded by the separate
llmbudget hard HostSlots/MemoryBytes constraints. Configure those shared ceilings
when concurrent launches must have atomic capacity limits. Memory reservations
represent declared launch demand, not a kernel allocation guarantee.

Focused verification:

```
GOMAXPROCS=2 GOFLAGS=-p=2 go test -race ./pkg/weave -run '^TestWeaveResource' -count=1 -timeout=90s
# in bashy
GOMAXPROCS=2 GOFLAGS=-p=2 go test ./internal/agentos -run '^TestHostAdmission' -count=1 -timeout=60s
```

Tests use private HOME/USERPROFILE and clear inherited Bashy/Weave overrides.
Separate OS processes prove lifecycle exclusion and retained claims after owner
crash. Actual command tests prove reservation before provisioning, persistent
wrapper ownership, synchronous settlement and owned pause preserving progress.

Safety follow-up: a paused acknowledgment requires verified child termination;
an unverified prior reservation blocks every restart path even when other
capacity is available. Cleanup accepts only the run's conventional workspace,
log and socket paths and repeats the integration proof on the renamed workspace
immediately before removal. A new clean commit is valuable work too. A hard
Bashy memory floor requires a declared nonzero launch memory demand.
