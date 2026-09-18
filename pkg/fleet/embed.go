package fleet

import "embed"

// baselineRoot is the prefix every embedded noun directory sits under.
const baselineRoot = "baseline"

// baselineFS is the compiled-in ring-0 fleet: the SEEDS bashy knows about
// with no configuration, no shared catalog, and no cloudbox — a launch
// contract per harness (every one, including the declared-but-not-measured
// ones) plus a default model and agent roster carrying its band pegs and
// reliability ledger. Every higher ring shadows it; nothing ever writes to it.
//
// The seeds are DATA, not the mechanism: rod-not-fish binds the Go (no
// vendor knowledge in code, no model name the runtime special-cases), while
// the roster is an initial default an operator overwrites — a same-named
// file in a shared dir on $BASHY_MODELS_PATH / $BASHY_AGENTS_PATH, an org
// overlay pulled by `sync`, or a `bashy model|agent set` copy-on-write in
// the local store all win over it. Without seeds a process that does not
// inherit the operator's shell exports (a headless worker, a daemon, an ssh
// session, a fresh host) saw an empty fleet, and every band-routed picker —
// `chat --band`, `steward start`, `meet --min-band`, the weave judge floor —
// failed closed. A seed that goes stale is re-pegged by data; an empty ring
// cannot route at all. (Sprint 161 D5 shipped tools only; revised 2026-09-13.)
//
// pkg/fleet/testdata/ring holds the roster the tests resolve against (see
// fleettest.Ring); it is deliberately separate from the shipped seeds so a
// roster edit never rewrites a test expectation.
//
// The tools are the single source of truth for the launch contracts,
// capability priors, and env markers that used to be duplicated across
// pkg/chat, pkg/weave, pkg/capability, and pkg/skills.
//
//go:embed baseline/tools/*.yaml baseline/models/*.yaml baseline/agents/*.yaml
var baselineFS embed.FS
