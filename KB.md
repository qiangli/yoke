# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] CLI stdout ownership must outlive process wait — WHEN streaming events from a short-lived CLI with os/exec, do not let exec.Cmd.Wait own and close StdoutPipe while a separate scanner drains terminal events. Attach an explicitly owned os.Pipe writer to Cmd.Stdout, close the parent's writer after Start, and let the scanner close t  (kb:cli-stdout-ownership-must-outlive-process-wait)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
