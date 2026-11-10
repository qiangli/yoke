# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] Prune dry runs must use read-only room membership — WHEN building a cleanup preview, room.Members reconciles stale cards and creates the members directory, so it mutates state. Use room.PeekMembers for candidate scans and recheck under the member-claims guard before applying deletion.  (kb:prune-dry-runs-must-use-read-only-room-membership)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
