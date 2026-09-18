# Coordination turn-admission adapter

`pkg/admission` is the store-neutral boundary for automatic coordination input.
It renders already-prepared records under one deterministic byte budget and
returns acknowledgements for exactly the records represented. It is not a
mailbox, cache, spool, or source of truth.

`pkg/bus` applies the primitive to both live-turn and cold-launch preambles. The
combined default is 4,096 UTF-8 bytes. The compatibility callback
`bus.PrepareTurnInbox` remains available, but Bashy's unified inbox should wire
the item adapter below so priority and acknowledgement remain record-scoped:

```go
bus.PrepareTurnItems = func(agent string) ([]admission.Item, error) {
	// Snapshot existing MB, Meet, and role stores without advancing a cursor.
	// Return one Item per authoritative record.
}
```

For every returned `admission.Item`, the Bashy adapter must:

- set a stable `Source` and source-native `ID`; set `Sequence` when the source
  has one;
- copy the exact valid-UTF-8 body and its routing header (`From`, `To`, and
  `Topic`) without textual deduplication;
- assign the shared priority: directed BLOCKED/CONFLICT/stop/security/ownership
  at P1, directed response requests at P2, decisions/baseline changes/terminal
  failures in declared concerns at P3, other directed/subscribed deltas at P4,
  and broadcast information at P5;
- supply `ArtifactRef` as a stable command or workspace-accessible path that
  opens that one complete record by ID. Temporary paths are not sufficient;
- supply `OverflowRef` as a stable source-level unread/list command. The
  versioned overflow manifest groups fully unrepresented records by this
  command and includes source, priority, first/last ID, counts, bytes, and a
  content digest, so a large batch remains discoverable within the same cap;
- make `Acknowledge` idempotently advance only that authoritative record. It
  must not acknowledge a whole snapshot, a later sequence, or any record whose
  body/reference header was not represented;
- return an error for a failed snapshot or invalid record rather than an empty
  successful snapshot.

The host calls `PreparedPreamble.Err` before attempting a turn. It calls
`PreparedPreamble.Commit` only after the model/control channel accepts the
prompt. A render error, launch refusal, budget refusal, or transport failure
therefore advances no source cursor. Header-only items may be acknowledged
because their digest and stable retrieval command were delivered; completely
unrepresented items remain unread.

Overflow is an inline `bashy-coordination-overflow-v1` JSON object containing
aggregate item/byte counts and a SHA-256 digest. Complete bodies stay solely in
their existing authoritative stores. Telemetry may copy only
`AdmissionReport` counts, byte totals, and `ContentDigest`; it must never record
bodies, summaries, sender/recipient names, topics, IDs, or retrieval paths.

Bus records can be retrieved without cursor movement with:

```text
bashy inbox --as AGENT --id bus:SEQUENCE --peek
```

The Bus adapter materializes addressed backlog in its existing pending view so
out-of-order priority acknowledgements can be recorded exactly. An omitted
record prevents the monotonic timeline cursor from advancing past it; no new
message store is created.
