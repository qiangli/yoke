# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] Run llmgw gates from its nested module — WHEN a yoke gate names ./pkg/llmgw/..., the root Go module rejects that pattern because pkg/llmgw has its own go.mod. Run go test ./... (and go vet ./...) with cwd pkg/llmgw; keep the root cligw, broker, and fleet gates separate so the nested-module setup error cannot mask their results.  (kb:run-llmgw-gates-from-its-nested-module)
- [repo/page] Gitea's 401 on a push mirror is an address refusal, not auth — WHEN a Gitea push mirror to a LAN/loopback replica answers 401 Permission denied (POST /repos/{o}/{r}/push_mirrors): it is not a credential problem — HandleRemoteAddressError maps ErrInvalidCloneAddr{IsPermissionDenied} to 401 and a correct all-scoped  (kb:gitea-s-401-on-a-push-mirror-is-an-address-refusal-not-auth)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
