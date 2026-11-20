# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] Run llmgw gates from its nested module — WHEN a yoke gate names ./pkg/llmgw/..., the root Go module rejects that pattern because pkg/llmgw has its own go.mod. Run go test ./... (and go vet ./...) with cwd pkg/llmgw; keep the root cligw, broker, and fleet gates separate so the nested-module setup error cannot mask their results.  (kb:run-llmgw-gates-from-its-nested-module)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
