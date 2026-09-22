---
id: 822d45eef46b
kind: feature
title: 'claim a NAME, not only a project: generalize coord.Claim'
seq: 1
status: todo
priority: p1
labels:
    - coordination
created: 2026-09-22T16:03:58.127714Z
sprint: 251
sprint_id: 40bcd7c0-49e6-5ad5-aa21-30c2647e2e21
sprint_title: 'bashy claim: voluntary exclusive holds on shared resources'
---

Today yoke/pkg/policy/coord claims a git PATH SET (Claim.Roots, conflict = Intersects). Make the claimed thing generic so the same ledger holds a named resource - the two DO test droplets are the case that forced it.

KISS: no new verb, no new package, no registry verb, no waiter list, no bus wake, no slots. Reuse what is there.

WHAT CHANGES
- Claim gains a Resource field (uuid + seq + slug + title). A claim is EITHER a path set (today, unchanged) or a named resource. Conflict for a named resource is slug equality; for a path set it stays Intersects.
- The record MINTS on first claim - there is no separate register step. uuid via uuid.NewV7, seq via max+1, slug via a local slugify mirroring kb.Slugify. Refuse a slug whose ref.ShapeOf is not ShapeSlug, so a name can never shadow a seq or a uid. Collision appends the seq.
- claimPath keys by HOLDER today because one agent holds one project. An agent may hold several resources, so named holds get their own file per resource under the same dir - one file, one holder, which is what makes exclusion structural. The two keyings coexist in one directory and List returns both kinds; story 2d182dee reads that listing, so keep List the single way in.
- Liveness switches to role.Seat / role.Liveness (yoke/pkg/role/seat.go) instead of the local Live/Stale pair. vacant | live | lapsed | unknown, and Takeable() excludes unknown. A hold is never seized because a probe was inconclusive - docs/fleet-evidence-invariant.md. Keep TTL at 30m and keep --force recorded.
- Conflict.Error already names holder, intent, since and the remediation - extend it to name the resource instead of the project. Refusal is the documentation.

SURFACE (all inside the existing claim verb - cli.go argues against verb accretion)
  bashy claim <name> [--intent T] [--wait D] [--ttl D] [--force]
  bashy claim release [<name>]
  bashy claim list [--json]        # projects and named holds in one table

--wait retries the WHOLE acquire cycle with backoff (20ms growing to ~2s), and is bounded by the duration given. It is NOT lockfile.AcquireWithin: in this detached mode the exclusion is the RECORD (holder + heartbeat + TTL), and claims.lock is only the mutex serialising the read-decide-write. AcquireWithin would return the instant it got that mutex and still find the hold taken. AcquireWithin is the right primitive for the ATTACHED mode only, where the kernel lock IS the hold - see story a8404f42.
No waiter registry, no push wake.

--ttl overrides the 30m default FOR THIS HOLD. 30m stays the default because a manager works in bursts; a long suite passes a longer one rather than heartbeating through it.

REF WIRING
- one row in yoke/pkg/ref/ref.go var kinds: Resource Kind = resource, plus a cleanID case so a leading hash is tolerated like todo and sprint. TestKindsIsTheVocabulary is the contract.
- coord.RegisterRefs(g) added to wireRefResolvers() in bashy/internal/agentos/refs.go, or TestRefResolversCoverEveryKind fails by name.
- NAMING NOTE for the implementer: the verb bashy resource is already taken by yoke/pkg/resources (host CPU/mem/disk telemetry). The ref KIND resource is a different namespace and does not collide, but say so in the doc comment so the next reader does not think it is a bug.

HONESTY
This ledger is HOST-LOCAL. It excludes agents on this machine and nothing else - the droplet-side flock in bashsharp-tests/tools/upstream-harness/leaf-run.sh is what excludes a second dev box. list and the refusal must say held on <hostname>, never free.

TESTS
exclusion between two distinct principals; a lapsed hold is reclaimable without --force and an unknown one is NOT; the same holder re-claiming is idempotent and preserves AcquiredAt; slug minting refuses a seq-shaped and a uid-shaped name; a path-set claim and a named claim do not conflict with each other; local slugify agrees with kb.Slugify (test-only import).

DONE WHEN, of sprint #251's acceptance, these pass: mint-and-hold, the refusal naming holder/intent/since, release, list showing project claims and named holds together, list showing a declared resource nobody holds, and resource:<name> resolving through bashy define by slug, seq and uuid prefix. The double-dash form is a8404f42; the help and skills are 6317513c; the board and the sprint take print are 2d182dee and 6317513c. This story blocks all three.

THE RECORD IS ALSO AN INVENTORY (operator, 2026-09-22)
The registry is not only a lock table. One agent creates a DO droplet and keeps it for reuse; another registers the local macOS box it runs tests on. Every later sprint manager should be able to SEE that those exist and opt into them. That is a second, independent reason the record is declared rather than a free-form key - and it changes what the record must carry.

So the minted record keeps four descriptive fields beyond uuid/seq/slug. They are what another agent needs to DECIDE whether a resource is the right one, and nothing more:
  Title    what it is in one line - DO test droplet, lon1, s-2vcpu-4gb
  Class    a coarse word for filtering - host | account | device | service
  Host     an optional fleet.Host alias, the reach hint. Addresses live in the
           fleet host catalog under the bashy config dir, NEVER in the record
           and never in a doc (docs/bashy-posix-fleet-implementation-plan.md).
  Notes    free text - what is prepared on it. This is the field that makes a
           registered machine reusable rather than merely visible: which
           toolchain, which trees, which lock the heavy jobs take.
Plus RegisteredBy (a principal.Ref) so a reader knows whom to ask.

Set them on first claim with flags, or later with the same command - no separate register verb:
  bashy claim do1 --intent X --title "DO test droplet 1" --class host --host do1 --note "Go 1.27.1 SDK at /srv/sprint142; heavy jobs take /srv/sprint162/leaf.lock"

A LISTING THAT SHOWS UNHELD RESOURCES
bashy claim list must show a DECLARED resource nobody is holding. A registry that only lists what is busy cannot answer what exists, which is the question this section is about.

THE RECORD OUTLIVES THE HOLD. This is the load-bearing sentence of the whole discovery half, and without it an implementer will delete the record on release and the inventory is empty forever. Release drops the HOLDER, never the resource. Two separate lifetimes in one store:
  the resource   declared once, persists until bashy claim forget <name>
  the hold       taken and released many times over that resource's life
So the inventory is populated as a side effect of ordinary use: the first agent to claim do1 describes it, releases it, and every later manager can see it. There is still no register verb - declaring a machine you are not about to use is claim-then-release, two commands you already have.

forget is the one addition to the surface: bashy claim forget <name> removes a declared resource, and REFUSES while anything holds it.

WHAT THE RECORD DOES NOT AUTHORIZE
Registering a droplet tells other agents it EXISTS. It does not tell them they may destroy it, resize it, or wipe its trees - those cost money and other people's prepared state, and doctl can create but not destroy (docs/vsc-pcts/harness/droplet-on-demand.md). Holding a resource is permission to USE it for the duration, never to end it. Say so in the help text, not only here: a discovery surface that reads as a disposal surface is worse than no discovery surface.

HOST-LOCAL, again
This inventory is per host, like the holds. An agent on another machine sees none of it until the deferred carrier exists. The listing must not imply otherwise.
