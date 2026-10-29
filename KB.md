# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] Account observation hook: where it can and cannot live — WHEN wiring an automatic account-collection hook (pkg/accounts) into a provider or budget path in yoke — pkg/provider is a stdlib-only nested module and pkg/llmbudget is imported BY accounts, so neither can call the hook directly pkg/accounts.ObserveSuccess cannot  (kb:account-observation-hook-where-it-can-and-cannot-live)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
