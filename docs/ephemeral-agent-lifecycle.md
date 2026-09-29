# Ephemeral agent ownership

Weave's `--clone` workers record their queue directory and run number in
`agent.lifecycle` at creation. Managed sprint sessions pass `BASHY_MINT_SPRINT`
(the sprint UUID) to newly minted agent definitions. Both `agent add` and
`agent clone` also accept `--sprint <uuid>` for explicitly creating a seat owned
by that sprint. Ownership sets `ephemeral: true`; cloning never inherits the
parent's lifecycle ownership. When a manager-created clone is first assigned
to a weave run, ownership narrows to that run while retaining sprint provenance;
seats created with `agent add` remain sprint-owned.

A durable weave or sprint transition reconciles owned definitions. A run ends
ownership when merged (`done`) or abandoned, and a sprint when its column is
`done`. Submitted and failed runs retain their workers for review or recovery.
Existing permanent agents and unowned legacy clones are never inferred to be
disposable from their names.

Before removing an owned local definition, reconciliation checks room members,
all known weave queues, outstanding resource reservations, and sprint leases.
Even a stale lease must first be released. Concurrent queue writes and room
claims defer cleanup. A later queue or sprint transition retries deferred work,
including after lease release. Unknown or unreadable evidence retains the agent.

Definitions are archived as private YAML files in the local agents directory's
`archive/` subdirectory before removal. Queue histories, sprint histories,
capability evidence and other ledger records are retained. Archived definitions
are absent from `bashy agent list --all` as well as the default view. An archive
failure leaves the original definition in place. Lower-ring identities and
local shadows of them are never automatically removed.
