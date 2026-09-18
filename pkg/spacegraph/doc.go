// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package spacegraph is the SPACE plane: what this machine has learned about
// its environment, as a graph of entities and the relations observed between
// them.
//
// # The gap it fills
//
// craft already records FACTS: `ssh -p 2222 -l xuser remote-host` teaches that
// `host:remote-host` answers on 2222 as xuser. That is an ATTRIBUTE bound to one
// entity, and it is genuinely useful — a later scp against the same machine can
// use it.
//
// What no store held was the RELATION. Nothing recorded that THIS host reached
// that one, as that account, at that time. A graph of attributes with no edges
// cannot answer "what can I get to from here", "who did I connect as", or "what
// stopped working since Tuesday" — and those are the questions an agent landing
// on an unfamiliar machine actually has.
//
// So one successful `ssh -p 2222 user@remote.host` on `dragon` yields:
//
//	nodes  host:dragon              (the self node — the local machine)
//	       host:remote.host
//	       endpoint:remote.host:2222
//	       account:user@remote.host
//	edge   host:dragon --reached--> endpoint:remote.host:2222
//	         via=account:user@remote.host  n=7 ok=7  first=… last=…
//
// # Facts, so no export path — and that absence IS the enforcement
//
// Every node here names something real: a hostname, a login, an address. That
// is identity by construction, which makes this a fact store rather than a
// knowledge store, and facts never leave the host. There is no Export, no Sync
// and no marshaller that emits the set, exactly as in craft. A store that
// COULD be shipped eventually would be.
//
// What generalises — "on this network locality ssh needs an explicit port" — is
// a fold: coordinate-keyed, identity-free, and it lives elsewhere behind the
// scrubber. Promotion out of here is a deliberate, gated step, never a dump.
//
// # Failure teaches nothing
//
// Edges are recorded on success, and on a payload failure (the session worked;
// what it carried did not). A TRANSPORT failure records nothing, because it is
// unattributed: `ssh` can fail on a wrong port, a wrong login, a rebooting
// host, or a dropped VPN, and in three of those the graph was right. Deleting
// an edge on failure would erode the store every time the network hiccuped.
// Correction happens by supersession on positive evidence — when a different
// value later succeeds, the old edge is closed.
//
// # Time is an attribute, never a key
//
// An edge is keyed by (source, relation, target) and nothing else. Counts,
// first/last timestamps and validity intervals hang off it. Putting a
// timestamp in the key is the failure mode this package is arranged to avoid,
// and it is silent: every observation lands at a fresh address, the store fills
// with n=1 singletons, nothing errors, and the graph quietly learns nothing.
// spacetime.IsUnkeyableProbe bans a clock probe from a coordinate for the same
// reason.
package spacegraph
