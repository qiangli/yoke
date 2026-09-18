// Package craft is the living skill graph: what a host has LEARNED from
// running skills, as opposed to which skills it has.
//
// The division of labour with pkg/skills is the point of the package
// existing at all:
//
//	skills   the CATALOG — an Agent Skills-compatible store you list,
//	         show, add, verify, run, promote, and export. It answers
//	         "what procedures do I have, and do they apply here?"
//	craft    what the catalog ACCUMULATES INTO — evidence gathered
//	         across runs, coordinates, and implementations. It answers
//	         "what has actually held, where, and how often?"
//
// One is the shelf; the other is what the practitioner learned working it.
// craft depends on skills, never the reverse.
//
// # Two keys, two questions
//
// Evidence is indexed both ways, because they answer different things.
//
// By NAME, evidence describes one skill: this procedure, this history.
//
// By CAPABILITY — skills.CapabilityKey, a content address over a skill's
// contract and effect cap with its name and steps projected away — evidence
// describes a PROMISE, pooled across every implementation that makes it. Two
// differently-named skills guaranteeing the same postconditions under the
// same effect bound are the same capability, so their runs are evidence about
// the same claim and belong in one pool rather than two thin ones.
//
// That second key is also what stops a catalog degrading as it grows.
// Semantically-overlapping skills competing as peers at selection time is a
// measured failure mode, not a hypothetical one; skills sharing a capability
// key are alternatives under one heading rather than rivals in one list.
//
// # No new store
//
// craft reads the append-only JSONL that pkg/skills already writes on every
// run. It adds no store of its own, and the derived index is rebuildable in
// full from those logs at any time. A tree that already carries several
// disjoint outcome records does not need another one; what it needed was a
// reader, because the receipts were being written and never read back.
//
// # Three content addresses, and what each one is FOR
//
// A composed skill has no authoritative file — it is assembled at query time
// and anything on disk is a cache. That is what makes it living, and it is also
// what would make it unauditable if nothing addressed the result. Three hashes
// do, and they answer three different questions:
//
//	Identity        the IMPLEMENTATION — canonical bytes. Dedups.
//	CapabilityKey   the PROMISE — contract + effect cap, with name and steps
//	                projected away, so two skills making one guarantee are
//	                alternatives rather than rivals at selection time.
//	Stamp           the RENDERING — identity, capability, band, coordinate,
//	                graph version, entity, and every fact and fold applied.
//	                Facts are HASHED, never carried: the stamp must separate
//	                compositions that saw different facts without becoming a
//	                channel for the values themselves.
//
// # The stamp addresses the bytes; the rev addresses the READ
//
// The stamp sees what a composition APPLIED. It cannot see what the composition
// did not read — a fold at another coordinate, a fact about another entity, a
// skill absorbed an hour ago, an attestation since accumulated. So two
// byte-identical compositions can come from materially different stores, and
// "which store produced this" is exactly the question asked when a gate verdict
// lands later and is attributed back to a rendering.
//
// GraphVersion closes that (see rev.go). It is taken BEFORE any store is read,
// so it names the state the composition was derived against rather than the
// state the process ended at. Its two properties are load-bearing: sizes rather
// than record counts, because counting would read every attest ledger on the
// prompt-assembly path; and UNKNOWN IS NOT ZERO, because a store we could not
// read must never be reported as a store with nothing in it.
//
// Verification routes for all of the above — the command that proves each
// claim — are tabulated in
// dhnt/docs/self-improving-loop-and-composition-provenance.md §3.
//
// # It reports; it does not decide
//
// Contribution rates are computed and displayed. Nothing is retired,
// elected, or reranked here. Those are policy, they require an evidence
// floor a single host does not reach alone, and acting on a handful of runs
// would discard a sound skill on noise.
package craft
