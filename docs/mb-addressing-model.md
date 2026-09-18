# The message-board addressing & delivery model

`bashy mb` / `bashy ping` post to one public, append-only board (`pkg/bus`,
`board.go`). A **send** produces a **receipt**, and the one rule the receipt must
obey is Yoke's: *claim only what the store can prove*. A confirmation that reads
like a delivery when nothing was delivered is the same class of defect as an exit
code reporting a success nothing verified.

This document is the canonical mapping the code implements.

## Resolution happens at SEND time

Before a post is written, the target a sender typed is resolved. Precedence,
most specific first (`ResolveSendTarget`):

| kind     | what it matches                                        | address used            |
|----------|--------------------------------------------------------|-------------------------|
| `role`   | a seat on this host (`steward`, `conductor:22`)        | the seat's stable topic |
| `agent`  | a name in the roster (`bashy agents list`)             | the canonical fleet name |
| `reader` | a name with a cursor — it has read the board ≥ once     | itself                  |

A target matching **none** of the three resolves to nothing. The send then
writes NOTHING and fails, naming near misses (`NearMisses`, ranked by edit
distance across roles + roster + readers) plus the broadcast escape hatch. This
is Yoke's requirement that an unresolvable identity "fail with choices instead of
guessing".

> The defect this closes: `bashy ping --as X profile-b "msg"` used to be accepted,
> posted with a literal `to: profile-b`, and reported *"waiting on the board for:
> profile-b"* — a receipt indistinguishable from a real delivery, to a name no
> reader answered.

## The six provable Delivery states

`Delivery.State` (`board.go`) is one of six, from most contact to least. The
classifier is `deliveryState(to, seq, steered, perReader)`.

| state        | proof                                                                      |
|--------------|----------------------------------------------------------------------------|
| `delivered`  | pushed into a live session — `SteerLive` succeeded                          |
| `read`       | the recipient's cursor is **at or past** the post's sequence               |
| `queued`     | appended, and the recipient's cursor is **behind** that sequence           |
| `unverified` | appended, but the recipient has **no cursor at all** — it has never read    |
| `accepted`   | well-formed and the target resolved, with **no single reader cursor** to judge (a role seat, a selector group, a broadcast) |
| `failed`     | the target resolved to no role, agent or reader — **nothing was written**   |

`unverified` is the important one, and the state the old wording erased. A reader
that has never read is **not merely behind**: reporting `queued` there claims more
than the evidence supports. `CursorSeq` draws the line — it returns `ok=false`
for a name with no cursor, which is distinct from a cursor of zero.

`perReader` is false for a role/selector/broadcast (no single cursor to judge),
so those prove at most `accepted`; it is true for an agent or an existing reader,
which have a cursor of their own and so can reach `queued` / `read` / `unverified`.

## A concern is a first-class ROUTE, not a fifth Address kind

The board also routes by SUBJECT. `Post.Topic` names what a post is about;
`Subscription.Topics` (the same field the bus sidecar has always matched on)
names what a reader cares about. When the two match — the bus's
segment-anchored matching, so a declared `cert.*` covers a post tagged
`cert.posix` — the post lands in that reader's view **uncapped**, alongside
the directed tier `Unseen` never truncates.

**The defect this closes:** `Unseen` capped everything that did not name the
reader to `-n` (default 5). On a busy board every capability-selector post and
every broadcast could scroll out of the default view, and the reader got a
COUNT (`older`) instead of the message — and shared-baseline announcements are
exactly that class.

**The decision, and why it is a route.** Yoke's `Address` kinds — principal,
agent, tool, role seat, person, host, group, external — are all IDENTITIES: a
*who* that resolves at send time, holds a cursor, and can be reported on in a
receipt. A concern is a PREDICATE OVER MESSAGES — a *what* — with no cursor, no
seat and no inbox. Making it a fifth Address kind would fork the model:
`ResolveSendTarget` would resolve a subject to nothing it can prove delivery
against, and every receipt state below `accepted` would be unreachable. So a
concern is Yoke's **Route**: a policy-controlled mapping from ingress (posts
whose topic matches) to a principal (each reader whose own subscription
declares it). The declaration is per-reader policy, resolution happens at READ
time, and the send path is untouched — `mb send shared-baseline` still fails
with choices, because a subject is not somebody you can send to.

Two boundaries keep the route from re-creating the defect it fixes:

- An **undeclared** concern is never silently promoted. The tag alone lifts no
  cap, or tagging would be a megaphone and the cap would be gone.
- A **work offer** (`--any`) is never concern-routed. It belongs to its pool
  and its claim rule; a declarer outside the pool must not be handed — much
  less claim — work that was not offered to it.

### The convention: well-known concerns

The mechanism is general (any topic can be declared, with
`bashy bus subscribe --topic <concern>`), but a vocabulary nobody shares routes
nothing. The documented set:

| concern           | what rides it                                                        |
|-------------------|----------------------------------------------------------------------|
| `shared-baseline` | changes to state every agent builds on — a frozen dir, a moved gate  |
| `posix-cert`      | the certification campaign                                           |
| `harness`         | agent harness/tooling changes — a rebuilt bashy, new or renamed verbs |
| `announce`        | the board's `wall`: EVERY reader is subscribed by default            |

Two rules keep it honest, enforced by review rather than code: **declaring a
concern is an obligation to read it** — the declaration is what lifts the cap —
and **tagging `announce` to skip everyone's cap is the same defection as
marking every email urgent**, visible to everyone on a public board.

## Consistency with the Yoke communication contract

- **Addresses stay identities.** `ResolveSendTarget` resolves role / agent /
  reader and nothing else; an unresolvable target still fails with choices. No
  subject ever became addressable.
- **Routes are policy, held by the routed-to party.** The declaration is the
  reader's own subscription — nobody can lift another reader's cap, and a
  sender's tag is a claim about the subject, not a delivery instruction.
- **Receipts claim only what the store can prove.** A concern post proves at
  most `accepted` at send time (no single cursor to judge). After the fact,
  each concern read records a VIEW — the same counted receipt `ModeAll`
  announcements keep — so "did everyone concerned read it" is answered by the
  views measured against the concern's declarers (`ConcernDeclarers`), not by
  a new delivery state.
- **Demote, never drop.** The undeclared tier is still trimmed, not deleted:
  the cap reports exactly what it hid (`older`), and `--all` retains the whole
  board.
