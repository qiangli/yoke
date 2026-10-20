# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [repo/page] CLI stdout ownership must outlive process wait — WHEN streaming events from a short-lived CLI with os/exec, do not let exec.Cmd.Wait own and close StdoutPipe while a separate scanner drains terminal events. Attach an explicitly owned os.Pipe writer to Cmd.Stdout, close the parent's writer after Start, and let the scanner close t  (kb:cli-stdout-ownership-must-outlive-process-wait)
- [repo/page] Agy model-door text lives in step updates — For cligw stdin-stream-json workers, extract only agent_response step_update.text_delta and use result.response only when no deltas arrived. Select a fresh Agy project explicitly; cwd alone can reuse remembered context. Keep raw provider stderr out of HTTP headers. See docs/validation/cligw  (kb:agy-model-door-text-lives-in-step-updates)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
