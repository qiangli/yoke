# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] Windows shell paths: program names and file operands use different conversions — WHEN a Bash or Bash# fixture passes paths into Go on Windows, convert executable program paths with pathconv.NativePath (program only, never argv), but convert filesystem operands such as /tmp state files with pathconv.ToOS so virt  (kb:windows-shell-paths-program-names-and-file-operands-use-differen)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
