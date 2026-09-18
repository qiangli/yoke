# Cell A and Cell B: decided (sprint #111, story #184 / ddd2176106b6)

Source measurement: `docs/agent-comms-matrix-2026-09-03.md` in the dhnt
umbrella (not part of this repo). It names two INTEGRATION cells broken in
the four-cell comms model (`mb`/`ping`/`meet`/`chat`, all of which work and
were left untouched):

- **Cell A** — `todo`'s owner (the assignee) was inert in both directions: assigning
  tells nobody, and `whois todo:<id>` names nothing.
- **Cell B** — `weave --help` names no `inbox`/`mail`/`message`/`notify` verb.

Both are decided below. Neither is left ambiguous.

## Cell A — HOOKED UP

`todo add --owner X` and `todo edit --owner X` now notify X over the
**existing** `bashy notify` front door (`pkg/bus/notify.go`'s `NotifyEvent`,
the same channel `bashy inbox` already drains) — see `pkg/todo/notify.go`.
No new verb, no todo-specific channel, per "prefer reusing an existing
surface over adding a verb."

An assigned item requires a canonical agent from `bashy agents list`; an
unknown name is rejected before the item is written. The caller also gets an
honest `AssignmentNotice` printed on `add`/`edit` immediately:

```
$ bashy todo add "fix the thing" --owner bob
added a1b2c3d4 [assigned] — fix the thing
  notified bob (bashy inbox --as bob)

$ bashy todo add "herd the cats" --owner nobody-registered-anywhere
todo: assignee "nobody-registered-anywhere" is not a registered agent
```

This closes both measured directions at once:

1. **"nothing tells bob"** — bob's inbox now gets a real entry
   (`bus.UnreadNotifications`, the same read path `bashy inbox` uses).
2. **"cannot reach the assignee through the item"** — the operator learns
   reachability at the moment of assignment, rather than needing a `whois
   todo:<id>` address form. Teaching `whois`/`pkg/principal` a `todo:`
   kind was considered and rejected as the wrong-sized hook-up: it is a
   bigger cross-cutting change to a package this story does not own, and
   the answer it would give ("is `bob` a reachable principal") is exactly
   what assignment-time notification already answers, one step earlier and
   without a new address grammar.

`todo edit --owner` reassignment notifies the same way; an edit that only
touches other fields does not re-notify.

Assigning an owner promotes the item to `assigned`; clearing the owner returns
an item still in that state to `todo`.

Tests: `pkg/todo/notify_test.go` (delivery, unreachable-assignee reporting,
and the `edit` reassignment path — all asserting the property via
`bus.UnreadNotifications`, never the CLI's own echo). e2e: `script/
e2e-agent-inbox.sh` checks **C8** (promoted from `planned` to `must-pass`)
and **C8b** (new, `must-pass`).

## Cell B — OUT OF SCOPE for this story, verified NOT actually blocked

`pkg/weave` and `pkg/chat` are both on this story's DO-NOT-TOUCH list, and
the measurement's own "Correction, same day" section names the real fix as a
durable `chat.Start` session wired through `pkg/chat/inbox_relay.go` — a
larger, separate story, not a two-file hook-up. So Cell A's playbook
("smallest hook-up, reuse a surface") does not translate into a coreutils
change here: there is no smallest hook-up available inside the files this
story owns.

What was checked before deferring, empirically rather than by reading
source: **does a worker already have a way to reach its conductor, today,
without any of the forbidden packages changing?**

Yes. `bashy sprint ping <sprint> --body "…"` (`pkg/weave/
weave_story_contact.go`, shipped separately alongside the P0-1
owner-reachability work already ahead of this branch on `main`) already
delivers into the conductor's inbox. Verified against a freshly built
`bashy` binary in a hermetic scratch `HOME`:　a message sent via `sprint
ping` with no `--as`, no registered identity and no live session claim
landed in the conductor's inbox unmodified. That is the exact capability
the matrix asked for ("a worker reporting a blocker to its conductor needs
no turn boundary… it needs a session claim") — except it turns out it does
not even need the claim, because `sprint ping` does not route through
`bus.ResolveAuthoredActor` the way `bashy ping`/`bashy mb` do. It is
therefore unaffected by the separate worker-cannot-AUTHOR P0 that still
blocks the `ping conductor:<sprint>` path (`script/e2e-agent-inbox.sh`
checks C3/C4, both still `planned`, untouched by this story).

The literal probe (`weave --help` names no comms verb) is still true, and
is **by design, not a gap**: `bashy weave` is the per-machine execution
queue (a workspace and a branch); `bashy sprint` is the ownership surface
over the same package, and a worker's channel to its owner belongs there —
the same way `todo`'s channel above belongs under `bus notify` rather than
growing its own verb. Nothing needed adding.

e2e: `script/e2e-agent-inbox.sh` adds **D1** (`must-pass`), which asserts
delivery through `sprint ping` directly — not a proxy, not the command's own
"pinged …" echo. Verified GATE GREEN against a `bashy` binary built from
this branch (16 pass, 0 fail, 13 still planned).

## A hermeticity bug found and fixed along the way

Running `script/e2e-agent-inbox.sh` from an actual agent shell (this one)
failed roughly half its checks — not on the behavior under test, but on
`"authenticated agent … cannot read/author as …"` and `"running under claude
with no claimed agent identity"`. The script exported a scratch `$HOME` but
never cleared `BASHY_PRINCIPAL`/`BASHY_AGENT_ID`/`WEAVE_AGENT` or the
harness markers `pkg/fleet.DetectTool` reads (`CLAUDECODE`, `CODEX_SANDBOX`,
…), so an ambient agent identity outranked every fixture agent the script
minted. Fixed by clearing the full marker set at the top of the script,
right after `export HOME`. This is the same class of bug the story's
DISCIPLINE section warns about — a check can pass or fail for a reason that
has nothing to do with the property it claims to test — just caught on the
harness side instead of the assertion side.
