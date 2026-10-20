# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:


- [repo/page] resource watch: install and operate the host resource watcher — How to install, tune and read bashy resource watch — use when a host needs disk/memory/CPU/GPU/network threshold alarms or when auto-registered host resources appear in resource list `bashy resource watch` samples this host's registered resources and  (kb:resource-watch-install-and-operate-the-host-resource-watcher)
- [repo/page] Agy model-door text lives in step updates — For cligw stdin-stream-json workers, extract only agent_response step_update.text_delta and use result.response only when no deltas arrived. Select a fresh Agy project explicitly; cwd alone can reuse remembered context. Keep raw provider stderr out of HTTP headers. See docs/validation/cligw  (kb:agy-model-door-text-lives-in-step-updates)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
