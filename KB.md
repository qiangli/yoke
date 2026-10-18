# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [host/page] never pkill on an outpost host — WHEN tempted to kill a stuck process on any outpost-paired machine - never pkill/killall by pattern The outpost daemon is the ONLY remote path to the machine; a pattern kill takes it down with the target and you lose the host. Find the exact pid, confirm its command line, kill that one pid - or better, use the o  (kb:never-pkill-on-an-outpost-host)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
