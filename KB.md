# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] Seed effects must cover the atlas effects of every command they run — A usage-probe seed's Effects: line becomes advice.WithCap in the snippet runner, and every command the body dispatches is classified by atlas.Lookup — fetch is read+net, so a [net]-only balance seed died 126 (effect cap denied). WHEN authori  (kb:seed-effects-must-cover-the-atlas-effects-of-every-command-they)
- [repo/page] Real-engine probe tests need a test-only sibling package — WHEN testing yoke usage-probe seeds end to end (YAML -> fleet -> llmbudget.Quota through the REAL Bash# runner): llmbudget's internal tests snippet.SetRunner(nil) in cleanup and pkg/dag imports llmbudget, so neither pkg/llmbudget nor pkg/fleet tests can import  (kb:real-engine-probe-tests-need-a-test-only-sibling-package)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
