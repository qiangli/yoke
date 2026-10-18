# Conductor run supervision with lens

Sprint #446, Story #2134 (13778c0d7a83).

Use one checkpoint per locator and output format. The shell must register its
owning-store sprint/run resolvers in lens.AgentWorldRefs (the same ref.Resolver
used by define). Story snapshots and deltas use StoryResolver, falling back to
the todo resolver; claims use ClaimResolver or the local coord ledger.

## Poll a run

For an existing run ID, capture the initial state and checkpoint:

```sh
bashy lens -t jsonl run:ID --since=start > run.delta.jsonl
bashy jq -c 'select(.t == "cursor") | .cursor' run.delta.jsonl > run.cursor.json
bashy jq -c 'select(.t != "cursor")' run.delta.jsonl
```

On subsequent polls:

```sh
bashy lens -t jsonl run:ID --since=@run.cursor.json > run.next.jsonl
bashy jq -c 'select(.t != "cursor")' run.next.jsonl
# After downstream processing succeeds:
bashy jq -c 'select(.t == "cursor") | .cursor' run.next.jsonl > run.cursor.json
```

Only process a batch if lens succeeded. Persist checkpoints atomically in a
production loop. Do not advance them before downstream processing succeeds.
An unchanged run emits only a cursor. A changed resolver status, for example
running → done, emits the new record followed by a cursor:

```json
{"schema_version":"bashy-node-v1","ref":"run:ID","path":"ID","name":"Worker","kind":"record","type":"run","attrs":{"kind":"run","id":"ID","title":"Worker","status":"done"}}
```

The actual final record has `t:"cursor"` and a `bashy-cursor-v1` envelope,
bound to provider `run`, target `run:ID`, and query `delta:jsonl`.
Its entity-v1 hash token is opaque; retain the emitted envelope unchanged.
A different run, scheme, or format cannot reuse it. Missing records/store
errors fail; they are not empty successful polls or deletion events.

## Collect concrete evidence alongside the run state

In the run's workspace, record its initial commit as BASE, then compare against
that fixed baseline (these are Git snapshots, not lens change-stream records):

```sh
git diff --name-status BASE
git log --oneline BASE..HEAD
```

Example evidence: `M cmds/lens/entity_delta.go` and
`abc1234 lens: wire run deltas`. Include uncommitted changes when reviewing
progress; a terminal run status alone does not prove a delivery commit exists.

For a known append-only worker log path:

```sh
bashy lens -t txt /absolute/path/worker.log --since=start --cursor-out=log.cursor.json
bashy lens -t txt /absolute/path/worker.log --since=@log.cursor.json --cursor-out=log.cursor.json
```

Example delta: `ok github.com/qiangli/yoke/cmds/lens 0.4s`.
The file cursor advances over complete lines only. Rotation/truncation requires
an explicit restart. With --cursor-out the bound envelope is saved separately
after stdout succeeds; without it JSONL streams end with a cursor record.

## Selector contract and remaining integration

All ten agent-world schemes support start and bound resume envelopes when their
resolvers are wired. Entity deltas detect changes to the visible record, not
every intermediate transition. Entity sequence IDs are identities, not update
revisions, so sprint/run/story/agent/claim/mb/meet/kb/host reject numeric, timestamp,
and duration selectors. Their resolvers expose no authoritative update time.

bus:events (and event-kind/sequence targets) supports sequence positions and
time/duration predicates against event timestamps, even out of append order.
Time queries after retention has removed history fail rather than claim a
complete answer. bus:timeline supports bound cursors only. File streams support
start and file envelopes only.

The proposed `lens -t listing run:ID --since=10m` remains unsupported:
--since uses jsonl/txt, and ref.Node has no run artifact or log history.
This recipe uses separate evidence sources explicitly. Completing that original
acceptance requires an owning run-store history API plus conductor skill
integration in the embedding shell repository; this repository has no conductor
skill source. No file/commit/log events are invented from a run status.
