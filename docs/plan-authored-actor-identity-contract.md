# Authored communication actor identity contract

Status: implementation plan (2026-08-29)

## Problem

Agent communication currently treats an explicit `--as` as stronger than the
launcher-stamped `BASHY_PRINCIPAL`. A process launched as one registered agent
can therefore append MB, Ping, Notify, Bus, or Meet records attributed to a
different registered agent. Inbox reads already reject this cross-identity
shape, but the authored paths do not share that check.

## Contract

Every manually authored communication command resolves its actor through one
shared function before it appends the authored record:

1. A launcher-stamped agent principal is authoritative. An omitted `--as`
   resolves to that agent; an explicit name must canonicalize to the same fleet
   agent or the command is refused.
2. A detected external agent harness without a launcher principal must provide
   a registered fleet name and hold that name's live session claim. The claim's
   hashed tool-session claim binds the watcher and command without persisting
   the vendor session identifier. `owner_pid` ancestry is the fallback for a
   tool that has no stable session environment.
3. A non-agent host process retains the public-board behavior that predates
   agent identities. The local OS account remains the trust boundary; this is
   collision prevention between governed agent processes, not remote
   authentication against the account owner.
4. A refusal never appends the attempted authored body. If the requested name
   is a registered agent, Bashy publishes a separate directed Bus warning to
   that rightful owner. The warning identifies the claimant but never copies
   the rejected message body.

The first consumers are MB post/send, messaging Ping, Notify, Bus publish, and
Meet board tell. POSIX utilities and programmatically generated system events
are outside this manually authored command contract.

## Cross-process external-agent claim

The persistent inbox watcher owns the canonical fleet-name room card for its
lifetime. It preserves `Principal` as attribution, stores a SHA-256 digest of
the stable tool-session identifier in `SessionClaim`, and records the stable
agent harness process in `OwnerPID`. Commands first compare the inherited
session digest; tools without one find `OwnerPID` anywhere in their ancestry,
so intervening shells and wrappers do not break the fallback. `room.Join`
serializes its read/check/write claim across processes before accepting the
canonical name.

This is intentionally smaller than a signing-key system. Bashy's documented
host OS account boundary does not make per-agent secrets meaningful against the
account owner, while principal inheritance plus the atomic session claim closes
the accidental cross-agent collision that occurred in practice.

## Acceptance gates

- authenticated X plus omitted `--as` or an alias of X succeeds as X;
- authenticated X plus `--as Y` fails and appends no authored record;
- an unclaimed or wrong-lineage external harness fails;
- a matching external claim succeeds and canonicalizes the actor;
- MB post/send, Ping, Notify, Bus publish, and Meet tell use the same resolver;
- a rejected registered-name attempt produces one directed warning without the
  attempted body;
- package tests and `scripts/crossvet.sh` pass.
