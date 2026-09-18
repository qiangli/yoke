// Package fleet is the declarative registry of the things an agentic
// host runs with: tools, models, and agents.
//
//	tool   an agentic CLI harness (claude, codex, opencode, aider, agy)
//	model  an inference backend (a subscription seat, a metered API, a
//	       pooled local model)
//	agent  a tool bound to a model — written tool:model — under one or
//	       more nicknames
//
// A bare tool is not an agent and a bare model is not an agent: an agent
// always names both. Roles are an orthogonal axis and do not belong to
// the binding.
//
// The same store also carries people, hosts and — since Sprint 179 —
// registered COMMANDS (`bashy commands add`, commands.go). fleet is the
// host ASSET store; a registered command is a host asset whose facet is an
// action (see docs/bashy-action-model.md in the umbrella), and it is the one
// noun with no embedded ring: bashy ships the mechanism, never a catalog of
// commands.
//
// # Nicknames
//
// An agent's identity is its tool:model binding; its names are aliases.
// `007` and `smarty` may both name claude:fable. Any number of nicknames
// collapse to one capability-matrix row, because that matrix is keyed by
// the binding, never by the nickname.
//
// # Two identities, and they answer different questions
//
// The paragraph above is about CAPABILITY: what an agent can do is its
// binding, so aliasing never fragments the matrix.
//
// At RUNTIME the identity is the agent's NAME, and that is a different
// question with a different answer. A running agent has one conversation
// store, one kb attribution, one bus cursor and one API key, and all four
// hang off its name. So a name is a singleton on a host: `bashy chat
// --agent X` while X is live is refused, not duplicated. Handing one
// identity two live tasks mixes their context, and an agent answering
// about one task from the other's history is confidently, plausibly
// wrong — the failure mode that is worse than a crash because nothing
// reports it.
//
// The two views meet at the useful conclusion: two NAMES on one BINDING
// are one capability and two identities. That is a legal, ordinary thing
// to write here — claimName only guards names — and it is the answer to
// "can I run two of these at once". You do not run one agent twice; you
// give the second one a name. `bashy agent clone` is that write, plus
// the parent's context as of now (see CloneAgent), plus provenance.
//
// An agent minted for a single task carries Ephemeral, which keeps it out
// of the roster and marks it for removal when its task closes.
//
// # Rings, and where the truth lives
//
// Entries are merged over pkg/assetring's rings — embedded baseline,
// shared catalog dirs, an optional org overlay, and the host-local store,
// in that precedence order. Every local entry is one file whose bytes are
// exactly the Content blob an org catalog would serve, so a definition
// round-trips in both directions without a transform.
//
// The package is standalone-first and effect-free: it reads and writes
// YAML and probes the host, but it never spawns an agent. Launching is
// the launcher's job; fleet only says how.
package fleet
