---
id: 6317513cfac6
kind: feature
title: 'sprint take + help + skills: make shared resources KNOWN to a manager'
seq: 3
status: todo
priority: p2
labels:
    - coordination
created: 2026-09-22T16:05:08.516712Z
sprint: 251
sprint_id: 40bcd7c0-49e6-5ad5-aa21-30c2647e2e21
sprint_title: 'bashy claim: voluntary exclusive holds on shared resources'
---

A mechanism nobody is told about is a mechanism nobody uses.

DEPENDS ON 822d45ee, not on a8404f42. Every touch point here - the help, the skills, the refusal, and the sprint take inventory - needs only the named hold and the declared record from story 1. The ONE thing that waits for a8404f42 is any example spelling the double-dash form; write those examples last, or write them with the detached form. Help that names a command which does not work yet is worse than silence.

The gap today: a sprint manager is told to coordinate about WORK (mb/Meet/chat/ping before touching another owner's changes) and nothing at all about MACHINES. Venue assignment lives in prose in a handoff doc, and the only real exclusion is a flock on the droplet that the dev box cannot see.

FOUR TOUCH POINTS

1. yoke/pkg/weave/weave_story.go, NewSprintCmd Long - THE TICK, step 3 STAFF (around line 481). Staffing is where a manager commits a machine, so the rule belongs in the step, not in a footnote. One clause: a story that needs a shared host takes the hold FIRST, because two managers staffing the same droplet is the collision, not the merge.

2. bashy/internal/agentos/sprint.go, const ownerAccountabilityHelp (line 125). It already has PRESERVE + clean up and COORDINATE OWNERSHIP. Add a sibling bullet in the same voice - something like: CLAIM SHARED RESOURCES before use. A test host, a droplet, a paired device or an account is held with bashy claim NAME --intent, and released when done. The hold is ADVISORY and HOST-LOCAL: it excludes the agents on this machine and nothing else. Never start a heavy run on a shared host without checking bashy claim list first, and never force another holder off without contacting them.
   WARNING for the implementer: bashy/internal/agentos/commands_e2e_test.go:262 pins the sentences of this constant. Update that test in the same commit or the e2e dispatch gate fails.

3. bashy/skills/sprint/SKILL.md and bashy/skills/conductor/SKILL.md - the procedure skills a manager actually reads. Same rule, one line each, in the staffing/setup step. Both are compiled in via skills/embed.go; the directive needs no change since neither file is new.

4. The refusal is the documentation (the pattern coord.Conflict.Error already uses): when a claim is refused, name the holder, their intent, how long they have had it, and the remediation. An agent that read nothing learns the rule the first time it tries to break it.

KEEP IT SHORT. This is help text a manager reads every turn - the TICK is already long. A clause and a bullet, not a section.

NOT IN SCOPE: wiring held resources into bashy sprint tick output, and any enforcement that refuses an unheld run. Both are follow-ups once the hold exists and is used.

DONE WHEN all four are updated, cd bashy and go test ./internal/agentos is green (including the pinned-sentence e2e test), and bashy sprint --help shows the new bullet.

5. yoke/pkg/weave/weave_story.go, sprint take and sprint start - SHOW THE INVENTORY WHEN A MANAGER TAKES THE SEAT (operator, 2026-09-22).

   The moment a manager takes a sprint is the moment it decides where the work will run, and it is the only moment it is guaranteed to be reading output. Print the declared shared resources there, after the lease is taken:

     shared resources on this host (bashy claim <name> to hold one):
       do1   host   free           DO test droplet 1 — Go 1.27.1 SDK at /srv/sprint142
       do2   host   held by lintel 42m (sprint 247 leaf replay)
       mb1   host   free           local macOS test box — podman machine, darwin go test

   WHY THIS IS WORTH A PRINT AND NOT A FOOTNOTE. Resources here are created by agents, for agents: one spins up a droplet and keeps it rather than destroying it, another registers the macOS box it already runs tests on. The value is compounding only if the NEXT manager finds out. Today it finds out by reading a handoff doc, which is why venue assignment keeps being restated in prose in every sprint plan.

   Keep it SHORT and keep it QUIET when there is nothing to say: no declared resources means no section, not an empty heading. Honour --quiet and --json like the rest of the verb.

   It is an OFFER, not an instruction. The manager opts in as appropriate - some sprints are dev-box-only and telling one it must pick a droplet would be wrong. The line above the list should read that way.

   SAME HONESTY RULE as everywhere else in this sprint: free means nothing holds it ON THIS HOST. And per story 822d45ee, seeing a resource is not permission to destroy it - the print must not read as an invitation to reclaim somebody's prepared machine.

   Story 2d182dee shows the same facts on the BOARD, which is where a human looks. This one is where an AGENT looks. Build the projection once and call it from both; do not grow two renderers of the same rows.
