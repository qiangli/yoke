# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] A service unit's expose.app with no port routes AT the primary listener — WHEN a bashy/outpost service unit fronts its real server with a translating proxy (loom/gitea + X-WEBAUTH-*, or any auth-header gateway): expose.app / expose.mesh with no 'port:' resolve to the unit's PRIMARY listener, so the proxy is sil  (kb:a-service-unit-s-expose-app-with-no-port-routes-at-the-primary-l)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
