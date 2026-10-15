# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] Real-engine probe tests need a test-only sibling package — WHEN testing yoke usage-probe seeds end to end (YAML -> fleet -> llmbudget.Quota through the REAL Bash# runner): llmbudget's internal tests snippet.SetRunner(nil) in cleanup and pkg/dag imports llmbudget, so neither pkg/llmbudget nor pkg/fleet tests can import  (kb:real-engine-probe-tests-need-a-test-only-sibling-package)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
