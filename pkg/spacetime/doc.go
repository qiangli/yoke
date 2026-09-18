// Package spacetime measures where-and-when code is running and answers
// whether a declared predicate holds here-now.
//
// It is the shared coordinate engine: host probes (the "space-time"
// coordinate), a machine-checkable `requires` grammar over them, and a
// content fingerprint (ContextKey) of a probe snapshot. pkg/skills gates
// skill applicability with it; pkg/fleet and pkg/principal gate contact
// methods with it.
//
// The ContextKey fingerprint is byte-compatible with the dhnt skill-CNL
// runtime's ContextKey, so keys can be handed to a dhnt adaptation store
// without translation.
//
// # Volatility
//
// Probes fall into two classes. Static facts (os, arch, libc) are stable
// for the life of a host and are memoized in-process and persisted across
// processes. Volatile facts — network locality above all — change while a
// process runs: a laptop is on the LAN one minute and remote the next.
// A volatile Probe is re-evaluated on every read; a volatile Resolver's
// namespace bypasses the persistent Cache entirely. Serving `net.same_lan`
// from a 24h cache would be worse than not probing it at all. The time.*
// probes and place.id are volatile core probes and therefore re-evaluate on
// every read.
//
// # Place privacy
//
// place.id is an opaque hash of network identity signals such as the default
// gateway's link-layer address and DNS search suffixes. Raw SSIDs, MACs,
// addresses, and real-world location are neither returned nor persisted.
package spacetime
