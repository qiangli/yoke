# Sprint 453 graph package evidence

Story #2266 (`55284ec86624`). This is package-level evidence for the graph holder; CLI/MCP registration, lifecycle native backup/restore, independent-process and remote gates remain with the sprint manager's coordinated phase.

## Representation and boundary

`pkg/dbgraph` implements typed node and directed edge operations on the supplemental in-memory `MemGraph` and on persistent Bonsai. Persistent typed records use only `bashy_graph_key` (indexed string) and `bashy_graph_data` (base64 of the JSON encoding of `dbstore.Node` or `dbstore.Edge`). Node and edge identities, types, direction, opaque binary properties, and revisions round trip. Isolated nodes export through `GraphTx.Nodes`; incoming and outgoing edges use the same identity. `MemGraph` has no DQL interpreter. Persistent Bonsai retains native parameterized DQL query and mutation for databases with nonportable schemas.

Logical export is a new directory with `manifest.json` and checked `records.jsonl`, format `bashy-db-export-v1` / `1.0`, graph representation `bashy-typed-graph-v1`. The manifest declares the two reserved predicates and SHA-256, byte count and record count. Memory export uses one graph snapshot transaction; persistent export uses one Bonsai query snapshot. A persistent database with another schema, an unexpected portable key, malformed record or unsupported format fails before logical mutation. Arbitrary DQL data uses native backup for fidelity; logical export makes no arbitrary DQL parity claim. Import validates the full manifest, checksum, identities, duplicate keys, revisions and edge endpoints before writing. Memory import is one atomic graph transaction. Persistent import performs one Bonsai mutation after conflict checks; it is limited to the reserved typed subset. `on_conflict=error|skip|replace` and whole-target `replace` are explicit. Typed persistent writes read/check and mutate within one category operation; raw category mutations and schema changes are refused once a typed schema exists. Non-Bashy direct native clients remain cooperative under the adapter's declared exclusion model.

**Owner admission gap for CAS:** `dblifecycle/owner.go` currently admits normal category writes with weight 1 of 1024. Two persistent typed writes can therefore both read the same revision before either mutation. Bonsai's `Upsert` is also implemented as a separate query followed by `Mutate`, without an atomic conditional. The manager has been asked to serialize graph typed writes at the existing per-target owner admission boundary while retaining `AccessWrite`; persistent revision preconditions are not yet accepted as concurrent CAS evidence. This package adds no lock registry.

A portable export is bounded to 100,000 records, 64 MiB of Bonsai query JSON and 8 MiB per JSONL record. Context cancellation is checked during scans, export and import validation. Larger graphs return `limit_exceeded` instead of an incomplete interchange. This bounded representation is the package's current scale limit.

## Local gate

On Dragon with `CGO_ENABLED=0 GOMAXPROCS=4 GOFLAGS=-p=2`:

- `go test -short -count=1 ./pkg/dbgraph` — PASS. Covers typed operations, revisions, process exclusion, memory→persistent→memory round trip with isolated nodes and binary properties, idempotent import, replacement, raw mutation bypass refusal, deletion cascade, and external DQL schema refusal.
- `go test -short -count=1 ./pkg/dbstore ./pkg/dbstore/bonsaidb` — PASS. Covers the existing graph adapter and Bonsai adapter tests after the graph transaction import seam.

The package is not yet mounted in shared CLI/atlas rows. The manager owns that phase and whole-product, independent-process and platform gates. Native Bonsai backup/restore in the shared lifecycle is likewise pending manager coordination; the existing lifecycle only supports SQLite and bbolt there.
