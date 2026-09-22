---
id: 2d182deefb31
kind: feature
title: 'shared resources on the sprint board: who holds what, visible to a human'
seq: 4
status: todo
priority: p2
labels:
    - coordination
created: 2026-09-22T16:06:48.57307Z
sprint: 251
sprint_id: 40bcd7c0-49e6-5ad5-aa21-30c2647e2e21
sprint_title: 'bashy claim: voluntary exclusive holds on shared resources'
---

A hold nobody can see is a hold nobody can help with. The operator's case: an agent is blocked on a droplet, and the human wants to find that out from the board rather than by asking.

Ships AFTER story 822d45ee (there is nothing to show until holds exist). Read-only projection, no mutations - Sprint's browser surface is read-only by construction (pkg/webconsole/handler.go:369-371 says so in as many words).

TWO SURFACES, and they are different renderers - do not try to share one.

A. THE WEB BOARD - yoke/pkg/board/panels.go
   DefaultPanels() is a registry of panel{id, build}; panel_board.go:251-259 projects EVERY registered panel into boardPanelView automatically. So this is one build function plus one entry in the DefaultPanels() list, and the browser picks it up with no new UI machinery.
   Copy the shape of dagPanel() in the same file:
     id: claims
     Title: Claims
     Collapsed: N shared resource(s) on this host; M held  (the one-line summary a collapsed panel shows)
     Columns: RESOURCE, HOLDER, STATE, INTENT, HELD FOR, MODE

   ON THE TITLE, and this is a real finding rather than a preference. The board ALREADY carries "Host resources" (pkg/board/resources.go:64, CPU/memory/disk telemetry) and "Fleet resources" (panels.go:289, capacity). A third panel called "Shared resources" would be the third title ending in that word, meaning a third unrelated thing, on one screen. Title it for the VERB a reader runs next - Claims - and let the collapsed line carry the words "shared resource". If a reviewer prefers the operator's original wording, the collision is the thing to argue against, not the wording.
   STATE is the role.Liveness word - live, lapsed, unknown - never a bool. INTENT is what the holder typed; it is the whole reason a human can tell blocked-and-fine from blocked-and-stuck at a glance. MODE is attached or lease (story a8404f42; until that lands every row is lease).
   Sourcing: read the coord ledger directly. Do NOT add a cloudbox call, a daemon, or a refresh - the ledger is a directory of small JSON files and coord.List already walks it.

B. THE TERMINAL BOARD - yoke/pkg/weave/weave_story.go, runSprintBoard
   Around line 380 there is already an on the clock: trailer after the kanban columns, with a comment explaining exactly why it is a trailer: the columns scatter, and what is on the clock right now is a question about the SET. Held resources are the same kind of question. Put it directly beside it, same voice:

     shared: do1 held by lintel 42m (sprint 247 leaf replay) · do2 free

   Keep it to one line where it fits. A stale or unknown hold is the row that matters, so mark it the way the lease already marks STALE rather than hiding it.
   Also add the rows to the --json payload at the top of the same function (payload := map[string]any{stories: stories}) so an agent reads the same facts the human sees.

HONESTY, and this is the part most likely to be got wrong
The ledger is HOST-LOCAL. free means no hold on THIS machine - it does not mean the droplet is idle, because a second dev box is invisible to it and the only real exclusion there is the flock on the droplet itself. Word every cell so it cannot be read as a claim about the machine: held on <hostname>, not held here. And unknown is its own state - never render it as free.

TESTS
pkg/board: panel builds with zero holds (empty, not absent); with a live hold; with a lapsed one; with an unknown one. pkg/webconsole: the panel reaches the JSON overview and the page renders it - and per yoke CLAUDE.md a UI change needs the browser test, go test ./pkg/webconsole -tags verifydom, because the byte-level tests cannot see the DOM or a script that throws. pkg/weave: board trailer with and without holds; the --json payload carries them.

NOT IN SCOPE: releasing or forcing a hold from the browser (read-only), notifications, and any history of past holds.
