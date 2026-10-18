# Sprint 453 graph category evidence — story #2266

Candidate: `pkg/dbgraph` on the issue-451 holder. The graph category registers
with `dblifecycle.CategoryOp`; all calls use the existing lifecycle open/hold,
owner admission, epoch fencing and request replay. The shared `cmds/db` mount
has not been edited pending the manager's phase-2 coordination.

## Local gate

Host: dragon, 2026-10-18. Command (direct exit status, no pipe):

```sh
CGO_ENABLED=0 GOMAXPROCS=2 GOFLAGS=-p=2 go test -short -count=1 ./pkg/dbgraph
```

Exit 0; `ok github.com/qiangli/yoke/pkg/dbgraph 7.907s` on the first named-test
run. Tests cover memory typed node/edge writes and reads, binary properties,
revision conflict, incoming edge traversal and explicit DQL refusal; persistent
Bonsai schema, parameterized DQL, mutation, and one-native-handle-per-process
exclusion. The adapter's separate process exclusion test lives in
`pkg/dbstore/bonsaidb` and is intentionally skipped in short mode.

## Current contract boundary

- Persistent: native Bonsai parameterized DQL, N-Quad mutation, schema show/apply.
- Memory: supplemental `dbstore.MemGraph` typed nodes/edges with revisions.
  Memory mode has no DQL parser or claimed DQL parity.
- Native persistent backup/restore and database drop are inherited from the
  accepted `dblifecycle` and Bonsai adapter, not reimplemented here.
- Lossless logical export/import is **not delivered**. `GraphTx` can look up a
  node and traverse edges, but cannot enumerate all nodes; exporting through
  it would omit isolated nodes. The package refuses export/import and declares
  no mode support for those operations. The manager was asked to coordinate
  the smallest shared adapter enumeration seam before phase 2.
- CLI/MCP mount, cross-mode round trips, independent-process owner tests and
  full story acceptance remain outstanding. Remote fixtures are not live
  host evidence.
