// Copyright (c) 2025 qiangli
// See LICENSE for licensing information

// Package execlog is the TIME plane: the agentic replacement for shell history.
//
// The bash `history` builtin is interactive-only. It records nothing on the
// script path, nothing on `-c`, and nothing on the ExecHandler path an agent
// drives — so the one process that sees every command an agent runs keeps no
// usable record of it. This package is that record: every dispatched command,
// in order, with its outcome, its duration, and where in space-time it ran.
//
// # What this is NOT
//
// It is not the audit log. `pkg/policy/audit` is a hash chain, and a hash chain
// cannot be pruned without breaking its own verification. This store MUST be
// prunable — it grows at roughly 7 MB/day under a working agent — so the two
// cannot be the same file. Audit proves what ran; execlog is what the host
// learns from.
//
// It is also not knowledge. A record here is an EPISODE: the rawest, most
// perishable stage. Knowledge is what survives distillation into a coordinate-
// keyed claim, and that lives elsewhere.
//
// # Redaction is structural, not conventional
//
// A record cannot be appended without one, because Append will not accept
// anything but a Scrubbed, and only Scrub can construct one. A writer that
// forgets to redact does not compile. That is deliberate: this store holds
// full argv for every command on the machine, so "we remember to call the
// scrubber" is not a good enough guarantee.
//
// # The template is the point
//
// Raw argv is stored scrubbed, but the LEARNING key is Template — a canonical,
// identity-free rendering in which `git commit -m "fix a"` and
// `git commit -m "fix b"` are the same command. Without that collapse every
// invocation is its own singleton and there is nothing to count.
//
// Note the consequence, because it cannot be undone later: raw argv is never
// stored, so a canonicaliser change cannot be applied retroactively. CanonVer
// is part of every record and a bump is a corpus reset boundary, not a
// migration. Get it right before turning capture on.
//
// # Day partitions, and why nothing is ever renamed
//
// Files are `<root>/<YYYY-MM-DD>/<episode>.jsonl`, opened O_APPEND and never
// rewritten. Retention deletes whole past-day directories. The alternative —
// rotate-by-rename, as the telemetry spool does — has two failure modes that
// are both silent: on unix the rename lands on a file other processes still
// hold open, and their writes vanish into an unlinked inode; on Windows the
// rename fails outright, the cap never binds, and nothing reports either.
//
// Sharding is by EPISODE rather than PID because BASHY_EPISODE is inherited,
// so nested bashy, subshells and `dag -j8` children co-locate — and that
// co-location IS the sequence this package exists to record. `pid`/`ppid`/`seq`
// disambiguate concurrent siblings so a reader never mistakes two interleaved
// shells for one causal chain.
package execlog
