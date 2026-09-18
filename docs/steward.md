# The steward seat

`pkg/steward` is the host/user-scoped seat of **authority and continuity**: exactly
one steward per machine-and-account, holding an append-only, evidence-carrying
journal that outlives whoever holds the seat.

It implements the design agreed in the 2026-07-13 three-party meeting. This document
is the self-contained record of what was decided and why; it deliberately does not
depend on any umbrella-private material.

---

## What problem this solves

An agentic host accumulates two kinds of loss, and they are different:

1. **Lost work** — a session dies with an uncommitted diff in the tree. That is
   `pkg/handoff`'s problem, and it is task- and artifact-scoped: capture the working
   tree, restore it into a successor's checkout.

2. **Lost authority and lost truth** — the agent that has been answering for this
   machine vanishes, and with it goes the only account of what was actually done,
   what was decided, and what was merely *claimed*. Nobody can say who is in charge,
   and nobody can distinguish "we verified that" from "an agent said so."

The second is what `pkg/steward` addresses, and it is the harder one, because the
incumbent **never gets to say goodbye**. A steward that crashed, hit a rate limit, or
was simply killed leaves no handoff note. Continuity has to work anyway.

### The two are not the same verb

This distinction is load-bearing and is enforced by a test
(`TestSeatLifecycleTouchesNoRepository`):

|                | `bashy handoff`         | `bashy steward`               |
| -------------- | ----------------------- | ----------------------------- |
| Scope          | one task, one project   | the whole host/user           |
| Moves          | **work** — a diff, a tree | **a mandate** — the seat      |
| Touches a repo | yes, that is the point  | **never**                     |
| Needs the predecessor | yes — it writes the note | **no**                   |

Claiming the steward seat restores no working tree, captures no diff, and touches no
repository. Conflating the two is what made "hand off your work" ambiguous in the
first place: **WORK is a diff, a SEAT is a mandate**, and only one of them should ever
mutate your repo.

---

## The design, and the reasoning behind each part

> **A note on this revision.** An adversarial pass over the first implementation found
> that several of its safety properties were reachable around rather than through. The
> corrections are called out inline as **[revised]**, with the hole each one closes,
> because a design document that quietly presents the fixed version teaches nobody why
> the obvious version was wrong.
>
> **A second pass then found holes in the fixes**, and those are marked **[revised 2]**.
> They are worth reading as a set, because they rhyme: each one is a control whose *shape*
> was right and whose *root* was wrong. An authorization that lives in a file the agent can
> write. A verification that promotes on a sentence the agent can type. An identity that
> comes from an environment variable the agent can export. A repair that is atomic in the
> happy path and torn in the crash path. **The lesson the second pass teaches is that a
> control is only as good as the thing it is rooted in — and that "the agent could just
> write the file" is a sentence you have to say out loud about your own design, not only
> about somebody else's.**
>
> **A third pass found that the second pass's fixes had the same disease**, and those are
> marked **[revised 3]**. The rhyme is now impossible to miss:
>
> - Promotion was moved off the `--method` sentence… onto a **digest the agent chooses** and
>   an **`Approved: true` struct the agent fills in**. Still the agent's own credential for
>   the agent's own claim.
> - The seat was keyed to the OS machine id… with an **environment variable consulted first**,
>   so the agent could rename the machine and get a fresh seat.
> - Each store was bound to its seat… and the agent could open **as many stores as it liked**
>   with `--dir`, each binding itself to that same seat.
> - The repair was made atomic… and then **reported as a failure after it had committed**.
>
> **The lesson of the third pass: it is not enough to ask what a control is rooted in. You
> have to ask who can *write the root* — and, for anything read back off disk, who can write
> the thing you check it against.** A control whose evidence lives in a file the agent owns is
> a control the agent owns, however many layers of indirection sit in between.

### One seat per machine and OS account — and neither comes from the environment **[revised 2]**

The seat is keyed to a **stable machine identity** and a **stable OS account**. Both halves
used to come from the ambient environment, and both were wrong in the same way.

- **The account was `$USER`** (falling back to `$LOGNAME`/`$USERNAME`) — a string the process
  inherits and can overwrite. So `USER=someone-else bashy steward claim` was **a different
  seat**. An agent that wanted a seat another agent held did not need to take it: it could
  export a variable and become the steward of a host that already had one. The singleton was
  sidesteppable **without touching a single file**.
- **The machine was the hostname** — a label a machine wears, not a machine. Two laptops both
  called `localhost` sharing a synced or network home would have **merged their journals**,
  their epoch ladders, and their stewards.

Identity now comes from the OS:

| | Source |
|---|---|
| **account** | the numeric **UID** (unix); the account **SID**, read from this process's token (Windows). Never an env var. |
| **machine** | `/etc/machine-id` (linux), `kern.uuid` via sysctl (darwin), `kern.hostuuid` (freebsd), the `MachineGuid` registry value (Windows) — read by syscall, never by shelling out. |

Where the OS offers no stable machine id — and **only** there — `$BASHY_HOST_ID` names it.
Where the OS *does* answer, the variable is **ignored**.

> **[revised 3]** This used to be backwards: the variable was consulted *first*, so
> `BASHY_HOST_ID=elsewhere bashy steward claim` resolved to a different scope digest, hence a
> different store, a different epoch ladder, and a **fresh vacant seat for the asking**. An
> agent that did not like the steward it found could simply declare itself to be on another
> computer. That is the same class of hole as the `USER=root` trick this section was written
> to close, reintroduced by the escape hatch meant to make it usable. A machine identity a
> process can *choose* is not one.

With neither an OS id nor the fallback, the seat **fails closed** (`ErrNoStableIdentity`).
The tempting alternative — generate an id and persist it under `$HOME` — is exactly wrong: a
shared home is the case machine identity exists to *detect*, and an id stored there would
travel with the home directory to every machine mounting it, handing all of them the same
seat. That is the failure, wearing the costume of the fix. A host that must resolve identity
its own way has a trusted, in-process hook (`WithScopeProvider`) that an agent cannot reach
by exporting anything.

### One seat, one store — and `--dir` does not get a vote **[revised 3]**

The store carries a **binding** to the identity it was born under and refuses to be opened
from another machine or account (`ErrScopeMismatch`). That is `dir → scope`: *this directory
belongs to that seat*.

It is **half** the singleton, and the missing half was an open door. Nothing enforced
`scope → dir`: *that seat lives in exactly one directory*. So:

```bash
bashy steward --dir /tmp/mine claim          # a fresh store…
BASHY_STEWARD_DIR=/tmp/mine bashy steward claim
```

got a brand-new store, which cheerfully bound **itself** to the very same seat, minted its
own epoch ladder from an empty journal, and handed over the seat. Two stewards on one host,
each holding epoch 1, neither able to see the other, **neither fenced** — because fencing
compares epochs *within one journal* and there were now two. The singleton the whole package
exists to guarantee was defeated by a flag.

So the mapping is made canonical. A **seat registry** (`registry.go`), kept *outside* every
store and keyed by the scope digest, records the ONE directory each seat lives in:

| | |
|---|---|
| **rejects** | any other directory, for `--dir`, `$BASHY_STEWARD_DIR`, and a plain `Open` alike (`ErrScopeDirConflict`) |
| **serializes** | first-bind, under a per-scope lock — two processes racing cannot both win |
| **revalidates** | before **every mutation**, so a handle whose seat was rebound behind its back is refused at its next write rather than journaling into an orphan |
| **isolates** | a shared home: entries are keyed by *machine*, so two boxes mounting one home still get two seats |
| **rooted** | in the **OS account's own home** — the passwd record for the real uid, the access token's profile directory on Windows. Never `$HOME`/`%USERPROFILE%` (see below) |
| **injectable** | `WithRegistryRoot`, for hermetic tests and for an embedder migrating a host's stores. Deliberately **not** an env var or a flag — a registry the agent can redirect is not a registry |
| **compares canonically** | the recorded directory is the *canonical* one — absolute, symlinks resolved — so `./x`, a trailing slash, and `/var/…` vs `/private/var/…` are one directory, not two seats |

#### …and neither does `$HOME` **[revised 4]**

The registry closed the `--dir` door and left its own front door reachable through the same
kind of knob. Its root was `os.UserHomeDir` — which *is* `$HOME` (`%USERPROFILE%` on Windows),
a string the process it governs can set. So the escape survived, at one remove and with one
extra variable:

```bash
HOME=/tmp/other BASHY_STEWARD_DIR=/tmp/other/store bashy steward claim
```

A registry that has never been written is a registry with **no binding in it**, and no binding
means *this seat has no store yet — bind mine*. Fresh store, fresh journal, epoch 1, a second
steward on a host that already had one. The registry did not fail; **it was asked in a
different building**.

So the root is taken from the **OS account**, by the same standard the seat's identity already
meets: the passwd record for the real UID, or the profile directory of this process's access
token on Windows. Both are as unspoofable as the UID and the SID the scope is keyed on, and
neither reads an environment variable.

The *store* directory is still movable — `--dir`, `$BASHY_STEWARD_DIR`, and `$HOME` all still
select where the seat keeps its bytes. That is deliberate, and it is not the same permission:
saying **where** a seat lives is not being allowed to have **two**. Move `$HOME` and you do not
get a fresh seat; you get `ErrScopeDirConflict` from the same canonical registry, naming the
store you already have.

With no account record at all (a container with no passwd entry), the root **fails closed**
(`ErrNoAccountHome`). The fallbacks it declines are `$HOME` — the hole — and a temp directory,
since `os.TempDir` is `$TMPDIR`, which is the same hole wearing a different variable. A host
whose state lives somewhere the OS cannot name says so **in-process**, with `WithRegistryRoot`.

**What it is not worth.** An agent with write access can delete the registry entry, just as
it can delete the journal. Nothing rooted in the filesystem survives an attacker who owns the
filesystem, and this package will not pretend otherwise. What the registry buys is that the
singleton is now *enforced* rather than merely intended: reaching a second store takes a
deliberate, destructive, evidence-leaving act instead of an ordinary flag — and the loser
finds out at its next write.

#### One directory has one spelling **[revised 5]**

The registry decides *same store?* by comparing a directory against the one it recorded, so
the **string** those comparisons run on has to be the same string every time it is derived —
including the one time there is no directory there yet.

That was the gap, and it hit the *first* store a seat ever had. `filepath.EvalSymlinks` cannot
resolve a path that does not exist, and the canonicalizer fell back to the unresolved spelling
when it failed. So a brand-new store under a **symlinked parent** — `/var/…` on macOS, where
`/var` is a symlink to `/private/var`, which is where `$TMPDIR` and many a state dir lives —
bound its seat to `/var/…/store`, and *then* created the directory. One mutation later,
revalidation canonicalized the recorded dir — which now resolved, because the directory was
there — compared `/private/var/…/store` against the `/var/…/store` the open store still held,
and refused the write with `ErrScopeDirConflict`: **the seat's only store, refused against its
own binding**, advising the operator to go and use the store it was already using. It fired on
the first claim a host ever made, which is the one path with no earlier store to paper over it.

So the order is fixed: **create the directory, then canonicalize it**, and take the canonical
form off the directory that now exists (`makeCanonicalDir`). The answer no longer depends on
*when* it is asked, and a store that cannot be resolved to a canonical path is refused rather
than opened under a spelling it would repudiate one write later.

The read-only half (`canonicalDir`) answers for a directory the registry *recorded* — one this
process must not create, and which may since have been deleted. It resolves the deepest
existing ancestor and re-attaches the components below it, so it agrees with the other half on
a live directory and does not change its answer when one goes away: a seat whose store was
removed is not refused against its own entry either. Bindings written by the affected revision
carry the unresolved spelling and keep working, since they resolve to the same canonical
string — there is nothing to migrate.

### One journal. Everything else is a projection.

A single append-only, hash-chained journal (`journal.jsonl`) is the **only** authority.
The board, status, log, conversation, history, and checkpoints are all **read-only
projections** derived by replaying it.

This is not tidiness — it is the fix for the most common way a state machine rots. The
moment a cached view becomes a *writable* second truth, it starts to drift, and the
first time it disagrees with the log nobody can say which one is wrong. A projection
has no state of its own, so it structurally cannot drift.

Concretely: `ProjectBoard(entries)` is a pure function. Same entries → same board →
same digest, on any host, in any process.

### Authority classes

Not every entry carries the same weight, and pretending otherwise is how a model's
prose gets mistaken for a fact.

| Kind | Authority | Carries |
|---|---|---|
| `effect` / `observation` | **authoritative** | evidence — something happened in the world |
| `decision` | **authoritative** | a rationale — an explicit, durable record of intent |
| `verification` | **authoritative** | an attestation that somebody went and **checked** an earlier entry |
| `transcript` | **non-authoritative** | an optional hash-linked artifact; nothing derives from it |

Seat events, checkpoints, reconciliations and repairs are a fourth thing: **record
facts** (`Kind.RecordFact`). They are made true *by being written* — the entry does not
describe the acquisition, the entry **is** the acquisition — so there is nowhere to send
an observer to check them. Reconciliation therefore grades world claims and leaves record
facts alone, and they are checked in the ways that actually apply to them instead: the
hash chain on every replay, and independent re-derivation for checkpoints.

> **[revised]** Grading record facts as if they were world claims produced nonsense in
> both directions. Every seat claim carries `success` and points at its own epoch, so the
> host was "degraded" from the moment anyone became steward, forever, with no act
> available to anybody that could clear it — and recording a *clean* reconciliation
> appended an entry that the *next* reconciliation read back as an unverified claim, so
> asking "is everything checked?" is what made the answer "no". A health signal that flips
> because you asked it is not a health signal.

**Transcripts are optional by contract.** Delete every transcript artifact on the host
and the board, the status, the history, and every checkpoint must be *bit-identical*.
`TestTranscriptDeletionDoesNotAffectProjections` pins this, precisely so it cannot
quietly stop being true. A decision record is what *binds*; a transcript merely lets a
human go back and see how the room got there.

### Missing evidence yields unknown — never success

The single most load-bearing line in the package:

```go
func (e Entry) EffectiveOutcome() Outcome {
    if e.Outcome == OutcomeSuccess && !e.HasEvidence() {
        return OutcomeUnknown
    }
    return e.Outcome
}
```

An entry claiming success with nothing to point at **does not project as success**. It
projects as `unknown`, on every board, in every checkpoint, forever. The journal still
records the claim faithfully — it is an honest record of what was *asserted* — but no
view will ever promote an unevidenced assertion into a fact.

An LLM writes fluent, confident prose about work it did not do. The only defense that
scales is to refuse to launder an unevidenced claim into a fact.

**Degradation travels one way only.** A *failure* without evidence stays a failure. We
never upgrade toward the happy path: the cost of a false "success" is unbounded, and
the cost of a false "failed" is a second look.

Consequently `closed` and `verified done` remain different facts. A workstream closed
with an unevidenced success is closed **and** unknown, and the board says so.

### A reference is not a verification — the confidence ladder **[revised]**

The first implementation stopped one step short, and the step it skipped is the one that
matters. It treated *evidence attached to a claim* as establishing the claim, so
`--outcome success -e "command:go test ./..."` projected as **verified**.

But `-e command:go test ./...` records only that somebody **said** they ran the tests. It
does not record that the tests were run, and it certainly does not record that they
passed. An agent can attach a plausible command string, commit hash, or URL to work it
never did **exactly as easily** as to work it did — the reference costs the same either
way. So a reference buys **auditability** and nothing else: it tells a skeptic where to
look. It does not tell them what they will find.

The board therefore has four rungs, and only one act climbs the last one:

| Confidence | What actually happened |
|---|---|
| `unknown` | success claimed with **nothing** to point at |
| `degraded` | an outcome self-declared as degraded |
| `asserted` | success claimed **with references nobody has checked** — the ordinary state of honest work |
| `verified` | a `verification` **sealed by a host-injected verifier** that went and looked — and that still recognizes its own seal when the board is projected |
| `refuted` | somebody looked, and the claim was **false** |

A `verification` entry is the only thing that can reach `verified`. It binds to the target's
**hash**, not merely its sequence number — an attestation must name the exact bytes it
vouched for, or it is an attestation of whatever ends up at that seq.

#### Nothing the caller can write promotes a claim **[revised 3]**

Three revisions have now put a lock on this door, and the first two locks could be opened
from the inside. Each time, the agent supplied **both the claim and the credential that
vouched for it**:

| What promoted | Why it was worthless |
|---|---|
| `--method "re-ran the suite"` | An agent that would write `--outcome success` it had not earned will just as happily write a method string it did not run. The sentence costs the same either way. |
| `-e 'file:/tmp/test.log#sha256:…'` | A digest proves **integrity**, not that a check ran: it says some bytes did not change and nothing about whether anybody looked. And since nothing rehashes the evidence at promotion time, **thirty-two arbitrary bytes did just as well** — the file need not exist. |
| `Verification.Adapter` | A *public* `*Attestation` with `Approved` and `Grade` fields, described in its own doc comment as "a trusted verification adapter's attestation." Nothing made it one. The caller filled it in: `Approved: true, Grade: verified`. |

None of those eliminated the trust-me claim. Each **relocated it one field sideways and
promoted it there**.

So promotion now rests on the one thing an agent cannot produce: **an interface it does not
implement.** A `VerificationVerifier`, injected by the *host* (`WithVerificationVerifier`) —
a CI adapter that asks the CI system, a git adapter that looks at the commit, a signing
service the agent holds no key for — goes and establishes the claim, and **seals** it.

And it is asked **twice**. Once at `Attest`, to mint the seal; once again **at projection**,
to re-check the seal that came back off disk. The second ask is the load-bearing one: the
journal is a file on the agent's filesystem, so a seal read out of it is just bytes the agent
could have typed. Only the verifier can say whether the seal is one **it** issued, **for this
claim** — so a hand-written seal fails, and a genuine seal *lifted off another verification*
fails too, because the token commits to the claim.

```
Seal.Approved, Seal.Grade   descriptive. A forger sets these to anything. They decide nothing.
Seal.Token                  opaque, and the whole seal. Only the verifier can produce it,
                            and only the verifier can recognize it.
```

**With no verifier injected, nothing is ever promoted.** Checks are still recorded in full —
the log keeps its value, `--method` and `-e` are there for a human to read and rehash — and
the board reports the strand as `asserted`, which is what an unverified claim *is*. The CLI
injects none (it has nothing that could go and check a claim), so it says so out loud:

```
$ bashy steward verify --seq 7 --result success --method "re-ran the suite" -e file:/tmp/t.log#sha256:9f2c…
verification recorded: seq 8 attests to seq 7 (success)
  NOT PROMOTED: the board still reports this strand as ASSERTED, not verified.
  No trusted verification verifier is injected on this surface, so nothing here could
  establish that the claim came true — your method and evidence are recorded and
  auditable, and they promote nothing. A host wires one with WithVerificationVerifier.
```

That is not a disappointment; it is a host with nothing that can check a claim, declining to
produce a green row about one.

**Refuting needs no credential at all.** `--result failed` moves a claim backwards with
nothing attached, and the board believes it. Degradation travels one way: we demand evidence
to become more confident and never to become less, because the cost of a false `verified` is
unbounded and the cost of a false `refuted` is a second look. (A verifier that goes and looks
and finds the claim **false** blocks recording it as a *success* — `ErrRefuted`. A refuted
claim must not enter the log wearing a success label.)

Both locks are real — `Attest` mints the seal, and `ProjectBoard` re-checks it. The
projection is a pure function of the log, so it must be able to grade a record it did not
write, including one that reached the journal without ever passing through `Attest`.

Most healthy hosts sit at `asserted`, and `steward reconcile` grades that **degraded**
rather than `ok`. That is not pessimism — it is the whole point. Calling a pile of
unchecked references a clean bill of health is precisely how "an agent said so" becomes
"we verified it".

### Authority vs. liveness — and why the split is the whole trick

```
AUTHORITY (who holds the seat, at which epoch)  ← derived from the JOURNAL
LIVENESS  (is the holder still breathing)       ← from seat.json's heartbeat
```

Authority is **recoverable by replay alone**. Delete `seat.json` entirely and the holder
and epoch survive; only liveness is lost, and it honestly degrades to `unknown` rather
than inventing a death. This is what makes crash recovery work with no handoff note, no
goodbye, and no cooperation from the incumbent.

### A stale heartbeat proves only a liveness lapse

This is the signal every lease system misreads. "The heartbeat is old" gets treated as
"the holder is dead" — and then a returning incumbent, which was merely throttled or
mid-thought, silently corrupts the record.

This package never makes that claim. A lapse is a **lapse**: the holder may be
mid-thought, rate-limited, paused at a human prompt, or on a bad network, and may come
back at any moment.

That is *precisely why the epoch exists*.

### The fencing epoch

A successor claiming an expired seat bumps a **monotonically increasing epoch**. The
returning incumbent — still holding the *old* epoch — is **fenced**: its mutations are
rejected, loudly (`ErrFenced`), instead of silently interleaving with the new steward's.

So a lapsed incumbent coming back is not a bug. It is *expected*, and the fence is what
makes it harmless.

Two details that are easy to get wrong, and are pinned by tests:

- **The epoch is checked before identity.** A returning zombie is, by then, no longer
  the holder — so an identity check would reject it as a mere stranger (`ErrNotHolder`)
  and never tell it the one thing it needs to know: *your tenure ended, the world moved
  on, re-read the journal.* Both errors refuse the write, so safety is identical — but
  only one of them explains a zombie to itself, and an agent that misreads "you are not
  the holder" as "I should just claim the seat again" will happily overwrite the steward
  that replaced it.

- **The epoch never descends.** A release does not reset it. An epoch that could go
  backwards would let a fenced holder un-fence itself simply by waiting.

### Every authoritative write presents a fencing epoch — and zero is not a value **[revised]**

The first implementation let a write omit the epoch, treating `0` as "whatever tenure I
currently hold". That convenience **was the hole.** An agent that does not know its tenure
ended is *precisely* the agent that will not mention an epoch — so the one caller the
fence exists to stop was the one caller it waved through.

There is now no such shortcut (`ErrNoEpoch`). Every authoritative write presents the epoch
it believes it holds, via `--epoch` or the `$BASHY_STEWARD_EPOCH` that `steward claim`
exports. The gate checks, in this order: the journal is readable, the seat is held, an
epoch was **presented**, that epoch is **current**, and only then, that the actor is the
holder. Checking the epoch *before* identity is what fences the **same logical principal**
holding a stale token: being yourself is not a credential, and a token from a tenure that
ended is stale no matter whose hand it is in.

### Claim vs. takeover

- **`claim`** takes a seat that is **vacant**, or one whose **trustworthy** heartbeat says
  the holder has **lapsed**. The ordinary path. It never negotiates with the incumbent and
  never requires a handoff note: read the journal, decide, write — all under one lock, so
  two agents racing for an empty seat cannot both win.

- **`takeover`** seizes any other seat — live, or unreadable. The recovery path, and
  deliberately the loud one.

**Both are authorized.** See below; that is the second revision's largest change.

#### Claiming does not renew a seat you already hold **[revised 2]**

Re-claiming a live seat you hold was quietly treated as an idempotent heartbeat: refresh
the cache, no epoch presented, no journal entry, no complaint. It reads as a harmless
convenience.

It is **a way to refresh a held tenure without presenting the epoch** — and the epoch is
the only thing in the system that can tell a steward its tenure ended while it was away.
The agent that most wants this shortcut is precisely the one that must not have it.

A live holder now renews through `steward heartbeat --epoch N`, which **presents the
token** and is fenced like every other authoritative act. Claiming a live seat is `ErrHeld`
— including when the live holder is you, which gets its own message pointing at
`heartbeat` rather than sending you off to look for a takeover you do not need.

Being yourself is not a credential. The token is.

#### An unreadable liveness record is NOT claimable **[revised]**

The first implementation treated an *unknown* liveness — no `seat.json`, corrupt file,
wrong holder, wrong epoch — as claimable, reasoning that a missing seat file is what a
crash leaves behind.

It is. But it is also what `rm seat.json` leaves behind, and **from inside the process the
two are indistinguishable.** Deleting one file must not be enough to take a healthy
steward's seat away. Every way of producing an unreadable liveness record is also a way to
produce one *deliberately*.

So the distinction the package now draws is between two very different sentences:

- *"I looked, and the incumbent is late"* — a fact about **the incumbent**. Claimable.
- *"I cannot find or trust the record"* — a fact about **the record**. Not claimable.

Recovering from the second is a **takeover**, which is authorized and recorded. (The
holder's own way out is simply to prove liveness: `steward heartbeat` rebuilds the record
from the journal, which still knows they hold the seat.)

#### Every acquisition of authority is authorized — claim *and* takeover **[revised 2]**

`--authorized-by <name>` was a **string**. An agent that wanted the seat could type one.
The first revision replaced it with a **grant** — single-use, expiring, epoch-bound — and
gated only the *takeover* with it, on the theory that claiming an empty chair takes nothing
from anybody.

That theory is true of a vacant seat on a fresh host and false everywhere it matters:

- **A lapsed seat has an incumbent.** "Lapsed" proves a heartbeat gap and *nothing more* —
  the holder may be mid-thought, rate-limited, or paused at a human prompt — and claiming
  it **fences** them. An unattended agent that could claim a lapsed seat could simply
  **wait out the TTL** and depose a working steward. That is the takeover it was forbidden
  to perform, spelled differently and with no record of authorization.
- **A vacant seat is still the seat of authority for the whole machine.** Becoming the
  steward is what lets an agent write the host's authoritative record. *"Anyone may become
  the authority as long as they get there first"* is a race, not an access-control policy.

So both consume a grant, and the two are **different actions**: a capability minted to
`claim` an empty seat cannot be spent on `takeover`.

| Bound to | Why |
|---|---|
| a **nonce** | the seat event that spends it names it **in the journal**, so replay refuses a second use — even if the grant file is restored from a backup |
| an **action** | claim and takeover are different acts with different victims |
| a **grantee** | not a coupon somebody else can pick up |
| **one epoch** | it authorizes acting on the seat **as it stands now**; if the seat moves on, the grant is dead |
| an **expiry** | 15 minutes by default; a capability that outlives its situation is a backdoor with a nice name |

#### …and the capability is not the authority **[revised 2]**

Here is the previous revision's own confession, and it was accurate:

> This package runs as you, on your machine, with your filesystem. Anything else running
> as you — *including the agent it is meant to restrain* — can write these same files.

It then shipped a control that lived entirely in those files. **A control an agent can
satisfy by writing a file is not a control; it is a log entry.** Every bound in the table
above is a check on bytes the agent could have authored — including the "external receipt",
whose digest proves the bytes did not change and says *nothing whatever* about who wrote
them. An agent can write a file, hash it, and hand it over as an approval; the digest
verifies perfectly. **A digest is integrity, never authenticity.** So the rule "an
unattended seizure needs a receipt" reduced, exactly, to *"the agent must write a file
first"* — on the one path that most needed a control, since that is where the cron jobs, CI
runners and runaway agent loops live.

A capability rooted in store state can never be better than the store. So the root of trust
now lives where the agent's filesystem access does not reach: **in the host process, as
injected code.**

```go
type Verifier interface {
    Name() string
    VerifyCapability(ctx context.Context, c Capability) (Attestation, error)
}

s, _ := steward.Open(dir, steward.WithVerifier(hostVerifier))
```

- **No verifier, no authority.** Every transition fails closed (`ErrNoVerifier`). Not "with
  a warning", not "recorded as unverified" — refused. Reads (status, board, log, reconcile)
  keep working, so a store you cannot take authority from is still one you can inspect.
- **It is asked when the capability is SPENT, not only when it is minted.** A mint-time
  attestation is a record *in the store*, and a record in the store is precisely what an
  agent can fabricate. Re-asking the injected verifier at the moment authority moves is what
  makes the check unforgeable from disk. A grant is a **bound** on an authority, never the
  source of one.
- **Grades are honest.** `GradeVerified` means a trusted verifier rooted outside this store
  established it. `GradeAudit` means a human-channel act this process **cannot
  authenticate**.

**An unattended transition requires `GradeVerified`.** An audit-grade attestation is
accepted only where a human plausibly is present — with nobody there, a human-channel
confirmation attests to nothing at all.

#### What the CLI can honestly offer: audit-grade **[revised 2]**

The CLI wires a typed-terminal confirmation (`ptyVerifier`). You type the epoch back; it
cannot be answered by a reflexive "y", and it forces you to look at who currently holds the
seat before taking it from them.

It is **audit-grade, not security-grade**, and the label is accuracy rather than modesty: a
pty an agent allocated produces exactly the bytes a keyboard does, and there is no second
party to ask. It attests that the act was **deliberate and attended**. It attests to nothing
about **who**.

- **There is no `--yes`.** It existed. A flag that skips the confirmation is a flag every
  unattended agent on the machine will pass, and an operator "assertion" nobody asserted is
  not an assertion — it is the whole control handed back in one word.
- **With no terminal, the CLI cannot acquire the seat at all.** Not a downgrade, a refusal.
  Note what that means for this repo's own test suite: the CLI tests cannot take the seat
  and must seed it through a host-injected verifier. A control the test suite could shrug
  off would be a control an agent could shrug off.
- **The integration hook is `WithVerifier`.** A host with a channel the agent cannot write
  into — `bashy meet`, a desktop confirmation, an approval service, a signature it can check
  against a key the agent does not hold — implements `Verifier`, returns `GradeVerified`,
  and the unattended path opens. Nothing else in the package changes: the enforcement point
  already exists and already fails closed.

An agent **cannot self-authorize** — and this document will say that only because a trusted
verifier is what establishes the transition, not because a grant file exists.

> **[revised]** "Is anyone at the terminal?" was answered with the usual
> `os.ModeCharDevice` shortcut — **which is true for `/dev/null`**, and `/dev/null` is
> exactly how a daemon, a cron job, a CI runner, and a headless agent get their stdin.
> The most unattended processes on the host were being judged the most attended. The check
> now asks the OS whether the descriptor is a terminal (`golang.org/x/term`), and
> `TestDevNullIsNotATerminal` pins it.

Takeover never asks the incumbent — an incumbent that could be asked would not need to be
taken over — and an unexplained seizure of authority is indistinguishable from a hijack, so
the capability *and the attestation that established it* live in the hash-chained journal,
not in a status file a crash could take with it.

### Checkpoints are caches with receipts

A checkpoint carries the **watermark** it projects and the **chain digest** at that
watermark. That makes it *verifiable* rather than merely trusted: re-project the journal
at the same watermark and compare.

- Same entries, same watermark → same board, always. No clock, no randomness, no ambient
  state leaks into the projection.
- Appending to the journal does **not** invalidate an old checkpoint — the watermark pins
  the history it projected.
- If a checkpoint stops re-deriving, the journal beneath it changed. Given the hash chain,
  that means *someone rewrote history*, which is worth finding out about.
- Delete every checkpoint file and you have lost nothing but the recompute time. The
  *file* is the cache; the journal entry recording that a checkpoint was taken is the
  memory.

The tempting alternative — a checkpoint you can *edit*, that accumulates state the journal
never saw — produces an artifact that is faster to read and impossible to trust. This
package structurally cannot do that.

### Corrupt tails: tolerated on read, refused on write

A crash mid-append can leave a torn final line. Replay walks the journal and returns the
**valid prefix**, plus an honest account of what it could not read.

- **Reads carry on.** A journal whose last 40 bytes are garbage still has a perfectly good
  history before the tear, and that history is exactly what a successor needs. Refusing to
  read it would turn a survivable crash into total amnesia — the precise failure this
  subsystem exists to prevent.
- **Writes refuse** (`ErrCorruptTail`) rather than forking the chain around the damage. The
  error states how many valid entries survive, so an operator learns immediately that a
  repair costs them nothing but the torn tail.
- **Repair is explicit and human-invoked** (`steward repair`, `--plan` to see it first).

Repair is deliberately not automatic: a log that silently healed itself would be worthless,
since "it repaired itself" and "someone tampered with it" would look identical.

#### What a repair may touch, and what it must refuse **[revised]**

`steward repair` fixes exactly **one** kind of damage: a **torn final append**. That is what
a crash actually leaves — the process died partway through the last line, so the file ends
with an incomplete fragment and **no terminating newline**. Nothing completed is in those
bytes, by definition, since a completed append is fsynced *with* its newline.

**Everything else fails closed** (`ErrNotRepairable`), and the two refusals are the point:

- **Mid-log damage.** If complete lines *follow* the unreadable region, whatever is after it
  was fully written. Truncating from the damage point would destroy completed records.
  (Detected by a newline in the discarded suffix.)
- **A complete record that does not chain.** A parseable entry whose hash, `prev_hash`, seq,
  or epoch is wrong is *not* a torn write. It is a record that was **altered**, or one
  written around a record that was **removed** — the signature of tampering. A tool that
  silently truncated that away would be the attacker's best friend: it would delete the
  evidence and call it a repair.

> *A repair that can only ever remove garbage is a repair. A repair that can remove data is
> a data-loss tool with a reassuring name.*

#### The repair is atomic **[revised 2]**

Every repair **authorizes** first (the holder, at the current epoch — a damaged journal is
not a licence for a stranger to truncate the host's record), then **quarantines** the exact
discarded bytes by digest, durably, before anything else moves. "The tool ate it" is not an
answer to "what was in those bytes?"

The receipt is where two revisions went wrong, and the second failure hid behind the fix for
the first.

- The first implementation wrote it with `_, _ = s.Record(…)`, so a failed receipt left a
  silently shortened journal with nothing in it saying so. Obviously wrong; obviously fixed.
- The second **reported the error loudly** — and kept the *shape*: truncate the file, then
  append the receipt. Those are **two separate durable writes**, and a crash, a kill, or a
  full disk in the window between them leaves *exactly* the state the receipt exists to
  prevent. Loudly reporting an error you only reach if you did **not** crash is no protection
  at all: in the crash case there is nobody to report to.

A journal that quietly healed itself is **bit-for-bit indistinguishable** from a journal
somebody edited to remove a record they did not like.

So the repair is now **one atomic write**: the valid prefix and the fully-formed,
already-authorized degraded receipt are assembled in a temp file, fsynced, renamed over the
journal, and the directory fsynced. The observable journal is therefore either the
**original corrupt bytes** or the **repaired-and-receipted bytes** — at every instant, for
every observer. There is no third state.

`TestRepairIsAtomicAtEveryCrashPoint` kills the repair at each named failpoint
(`repair.after-quarantine`, `repair.before-replace`, `repair.after-replace`) and asserts
precisely that. A durability property only asserted in a comment is one nobody has ever
checked.

### Reconcile is allowed to say "I don't know"

`steward reconcile` is the verb a successor runs **first**, before touching anything.

| Verdict | Meaning |
|---|---|
| `ok` | the journal is intact and every world claim in it has been **checked** |
| `degraded` | the record is readable, but something in it could not be established |
| `unknown` | the **record itself** is damaged; what survives is valid, what came after cannot be spoken for |

There is deliberately **no `failed`**. The subsystem never reports success in the face of
missing evidence — and it never invents a failure it cannot prove either.

Reconcile also **still reports without the seat**, and that is deliberate: it is the verb
you run *before* you hold anything. Failing to journal the report (no seat, no epoch, a
fenced epoch) prints the report anyway and says why it was not written down. Refusing to
print the truth because you could not also record it would break the one command a cold
successor needs, in exactly the situation it exists for.

#### It does not claim to have checked reality **[revised]**

The first implementation's reconcile reported that it had *"compared the journal against
reality"* while comparing the journal against **nothing but itself**: it re-read its own
entries, noticed which ones lacked evidence, and called that a reality check. That is not a
reality check. It is a **spellcheck**.

The core package is generic and knows nothing about git, CI, GitHub, or any other world an
entry might make claims about — it *cannot*, since a host-scoped journal spans every project
on the machine, and baking in every checker would make this package the union of every tool
it records. So it takes **adapters** (`Observer`): a host supplies things that know how to go
and look — *did that commit actually land on main? did that CI run go green? is that service
actually up?* — and reconciliation reports what they **found**.

With no adapter, `RealityCompared` is `false` and the report says so, in prose, out loud:

> *NOTHING was compared against reality: no observation adapter was supplied. Every claim in
> this report stands exactly as the agent that made it left it.*

An observation is what an adapter **found**; it becomes part of the record only when a
steward records it as a verification. A reconciliation that always produced a clean verdict
would be worthless. The only useful thing it can do is tell you precisely where the record
runs out — the difference between inheriting a *system* and inheriting a *story about* a
system.

---

## Durability and concurrency

- **Atomic, durable writes.** Temp file → `fsync` the data → `rename` → `fsync` the
  directory. A rename that lands while the contents are still in the page cache can leave a
  correctly-named *empty* file after a crash, so the data fsync is not optional. Journal
  appends are `O_APPEND` + `fsync`: the journal is the only authority there is, and if it can
  lose a write, everything derived from it is a guess.

- **Serialized read/decide/write.** Every acquisition runs the whole read → decide → write
  cycle under one exclusive file lock. This is essential exactly here: `Claim` must *read*
  the journal, decide the seat is free, and *write* its claim — and if two agents interleave
  those three steps, both conclude the seat is vacant and both take it. That is the race the
  singleton contract exists to forbid, reproduced inside the mechanism meant to enforce it.

- **Real locks on every shipped platform.** `flock` on Linux/macOS, **`LockFileEx` on
  Windows**. The older claim registry (`pkg/policy/coord`) documents an honest Windows gap —
  no lock, so two agents can both conclude a project is free — and the steward seat cannot
  afford to inherit that apology: *a singleton enforced by a racy acquisition is not a
  singleton*. `pkg/policy/audit` had already proved `LockFileEx` works for this.

- **A platform with no locking fails every mutation CLOSED.** **[revised]** The first
  implementation shipped a **no-op lock** on other platforms (`lock_other.go`), with an
  apology in the comment — and the apology is what gives it away. *A lock that silently does
  nothing is worse than no lock at all*, because the caller believes the read/decide/write
  cycle is serialized. It is not: two agents interleave, both replay a vacant seat, both
  append a claim, and the host now has two stewards that each believe they are the only one
  — the exact failure the singleton exists to prevent, produced by the mechanism meant to
  enforce it.

  The fencing epoch does **not** save this, either, which is what the old note got wrong:
  both claims mint their epoch from the same replayed head, so they **collide** rather than
  supersede. Neither steward is fenced, because neither one's token is stale.

  So the seat fails closed (`ErrLockUnsupported`): a platform that cannot serialize cannot
  host a steward, and saying so is the only honest option. **Reads still work** — they never
  take the lock. `TestUnsupportedLockFailsEveryMutationClosed` pins that every mutation
  refuses.

  **[revised 2]** `aix` was in the `flock` build tag, on the reasonable-sounding grounds
  that aix is a unix. But listing a platform there is a *claim that `unix.Flock` works on
  it*, and a platform that compiles the call but cannot honour it gets exactly the outcome
  above. **A platform earns its place in that tag by being tested, not by being a unix.**
  aix now falls through to the fail-closed implementation, and `GOOS=aix go build` is in the
  cross-compile check.

- **A committed operation reports that it committed. [revised 2]** The journal is the
  authority; `seat.json` is derived. When the append **lands** and the cache write then
  fails, returning a bare error tells the caller *"your claim failed"* — and a caller that
  believes that **retries**. The retry replays against a journal that already holds the
  claim and appends a **second** seat event, minting a second epoch that fences the tenure
  the first call successfully acquired. The operation that "failed" thereby destroys the
  thing it supposedly did not do.

  So `Claim`, `Takeover`, `Record`, `Attest` and `Release` return `ErrCommitted`, carrying
  the **committed seq and epoch** and saying plainly: do not retry. Recovery is an
  **idempotent** `steward heartbeat --epoch N`, which rebuilds the cache from the journal.
  Fault-injection tests (`seat.write`, `seat.remove` failpoints) pin every one.

- **…and so does a committed *repair*. [revised 3]** The rule above was stated for the seat
  and then not applied to the one operation with the most to lose. **The atomic rename is the
  commit**: once `writeBytesAtomic` returns, the repaired-and-receipted journal *is* the
  journal, for every reader, at every instant, including one arriving after a power cut.

  But the failpoint and the read-back that follow it returned **bare errors with an empty
  `RepairResult`**. A caller handed `RepairResult{}, err` reasonably concludes the repair did
  not happen — and retries. The retry replays against a journal that is **already repaired**,
  finds it intact, and reports *"nothing to repair."* So the operator is told, in sequence,
  that the repair failed and that there was never anything wrong with the journal. Both are
  false, and the second is the kind that **ends an investigation**.

  Now the result is populated at the instant of commit, and every later failure returns
  `ErrCommitted{Op: "repair"}` with the seq, the epoch, and the cause. The **remedy is
  operation-specific**, because the fixed sentence the previous revision printed
  (*"run `steward heartbeat`"*) was confident, specific, and wrong for a repair: a repair is
  not rebuilt by a heartbeat — it is already whole. What failed is the step that *confirms*
  it, so the advice is to go and **look**: `steward reconcile`, a pure read.

- **Schema-versioned artifacts.** Every journal entry, seat file, grant, checkpoint, board,
  and reconciliation carries `bashy-steward-v1`. A **mismatch is never tolerated** on the
  seat cache or a grant: a record this package cannot fully understand is not one it may act
  on.

---

## Layout

```
$BASHY_STEWARD_DIR (default ~/.bashy/steward)
├── journal.jsonl        the ONLY authority — append-only, hash-chained
├── seat.json            liveness cache ONLY (delete it; authority survives)
├── steward.lock         the acquisition lock
├── checkpoints/         materialized projections (caches with receipts)
├── grants/              takeover capabilities (the JOURNAL records their consumption)
├── receipts/            external approval artifacts, pinned by digest
├── quarantine/          bytes a repair discarded — kept, never destroyed
└── transcripts/         optional, non-authoritative artifacts
```

Host-wide and **cwd-independent**, deliberately. A steward is not a property of a checkout —
it is the human's continuous point of contact across every project on the machine. Keying it
to a repository would produce one steward per clone, which is exactly what the singleton is
meant to prevent.

---

## CLI

```
READ (no seat required — reconcile is what you run BEFORE you hold anything)
steward status                 who holds the seat, are they alive, what does the board say
steward scope                  which host/user seat this is, and where it lives
steward board [workstream]     the workstreams, and which outcomes are actually established
steward log [--degraded] [--kind K] [--workstream W] [--actor A] [--since T] [--follow]
                               the journal itself, chronologically
steward conversation           the decisions, and how the room got there
steward history                how the seat changed hands; checkpoints along the way
steward grants                 the capabilities, and whether they can still be used
steward reconcile [--record]   what can and cannot be established
steward repair [--plan]        truncate a TORN FINAL APPEND — and nothing else

SEAT
steward claim | take [--intent W] [--export]
                               acquire a VACANT or LAPSED seat (atomic); exports the epoch
steward authorize --actor <who> [--reason W] [--receipt F --receipt-issuer S] [--ttl D]
                               mint a single-use, expiring takeover capability
steward takeover --grant <id>  seize the seat; bumps the epoch, fences the prior holder
steward release [--note W]     vacate cleanly (captures NO repository state)
steward heartbeat              refresh liveness (writes no journal entry)

WRITE (all fenced: --epoch, or the $BASHY_STEWARD_EPOCH that `claim` exports)
steward record -m <what> [--outcome …] [-e kind:ref …] [--observation]
steward decide -m <what> --rationale <why>
steward verify --seq N --method <how> [--result success|failed|unknown]
steward checkpoint [--note W] | [--verify ID] | [--list]
steward transcript -m <what>                 (non-authoritative; nothing derives from it)
steward workstream open|update|close <name>
```

`--json` is available on every verb and carries the schema version.

`--degraded` is the query a successor needs first: *what do I not actually know?*

### The shape of a recovery

```console
$ bashy steward status
seat: claude-a1b2 (epoch 3) — lapsed
  heartbeat: 2026-07-13T09:14:22Z (2h11m ago — LAPSED, which proves a lapse and nothing more:
             they may be mid-thought, throttled, or coming back. Claiming FENCES them, safely.)
  journal:   47 entries, intact

$ bashy steward reconcile
reconciliation: DEGRADED

seat:     claude-a1b2 (epoch 3) — lapsed
journal:  47 entries, intact (head sha256:9f…)
board:    4 workstream(s)

reality:  NOTHING was compared against reality: no observation adapter was supplied.
          Every claim in this report stands exactly as the agent that made it left it.

UNPROVEN — 2 claim(s) you must not take on faith:
  seq 39   [api] claimed success, effective unknown
           migrated the schema and verified the cutover
           why: claimed success with no evidence — a claim nobody can check is not a fact

ASSERTED, NEVER CHECKED — 3 claim(s) resting on references nobody verified:
  seq 41   [web] shipped the rollout
           verify it: `steward verify --seq 41 --result <success|failed> --method <how>`
  …

$ bashy steward claim --intent "picking up after a lapse"
claimed the steward seat: claude-c3d4 at epoch 4
  the lapsed seat was held by claude-a1b2 — they are now FENCED at the old epoch

  export BASHY_STEWARD_EPOCH=4
```

If `claude-a1b2` comes back, its next write is rejected with `ErrFenced` — loudly, naming
the epoch it presented and the epoch the world is at, and telling it to re-read the journal
before it does anything else.

Had the heartbeat been *missing* rather than *late*, `claim` would have refused
(`ErrLivenessUnknown`) and pointed at `steward authorize` → `steward takeover`: a seat you
cannot read is not a seat you may quietly take.

---

## Relationship to the rest of the AgentOS hub

### The steward room is a permanent role address

The steward's Meet room is named `steward` and belongs to the role, not to the
process or agent currently holding it. A handoff therefore reuses the same
room identity and transcript; `steward stop` releases the authoritative seat
but does not destroy its address. It is reachable as `bashy meet observe
steward` (or `@steward`) and is one of Meet's configured permanent rooms.

This changes the old cleanup rule intentionally. An ordinary conductor room is
bounded work and still closes on release. The steward room is the host's front
desk: closing it during every shift change created a series of dead addresses
and discarded exactly the continuity the steward role exists to preserve.
Current ownership and liveness still come from the steward record, never from
the Meet roster.

- **`pkg/handoff`** stays exactly as it is: task/artifact-scoped, repository-touching. The
  steward seat does not replace it, does not restore working trees, and does not capture
  diffs. They compose — a steward may well hand a task off with `bashy handoff`.
- **`pkg/policy/coord`** remains the per-*project* claim registry (one agent per repo). The
  steward seat is per-*host*, and reuses coord's identity rule (`coord.Self`) so a steward, a
  claim, and an audit record all name the same agent the same way. It does **not** reuse
  coord's lock, because coord's Windows no-op is a gap a singleton cannot tolerate.
- **`pkg/principal`** supplies the identity (`principal.Ref`). Holders are compared by
  episode-or-(name, host), never by PID: one logical agent runs many processes (a shell, a
  subagent, a hook), and none of them should be told it is colliding with itself.

---

## Known gaps

These are gaps, stated as gaps. None of them is quietly papered over in the code.

- **Authorization is not cryptographic.** The largest limitation, and it is structural rather
  than an oversight: this package runs as the user, so anything else running as the user —
  including the agent it restrains — can write its files directly. A grant is durable,
  replay-protected, expiring and auditable; it is **not** proof a human was in the room, and
  it is labelled `operator-assertion` precisely so nobody downstream mistakes it for one.
  Closing this properly needs a signer the agent does not control (an OS keychain, a hardware
  token, a second machine) — a real design, not a flag.
- **`Interactive` is the host's assertion.** A caller can set it. It buys the unattended
  path's receipt requirement, not a proof of attendance.
- **No `Observer` adapters ship here.** The core is generic by necessity, so out of the box
  `steward reconcile` compares the journal against *nothing* — and says so, in prose, rather
  than implying otherwise. The git/CI adapters belong in the hosts that know those worlds.
- **`--follow` is polling**, not `inotify`. Deliberate: it keeps follow a pure function of the
  journal (replay, skip below the watermark) rather than a second, event-driven code path that
  could disagree with a plain `steward log` — and it makes follow testable without sleeping
  around a filesystem race.
- **No `steward transfer` verb.** Release + claim already covers a planned handover of the seat,
  and the seat is recoverable without either. A dedicated `transfer` would only add a
  cooperative path to a system whose entire premise is that the incumbent may never cooperate.
- **Platforms with no file locking cannot host a seat at all** (`ErrLockUnsupported`). Mutations
  fail closed; reads still work. This is a deliberate *reduction* in capability from the first
  implementation, which pretended to lock and did not — see the durability section for why the
  fencing epoch does not rescue that case.
