# KB — relevant repo and host knowledge (may be stale; verify before relying on it)

Check these before you start — they may save you a failed approach:

- [host/page] Bridge arbitrary Go strings through binary-safe bytes — When a native bridge string may contain serializer bytes, NULs, or invalid UTF-8, do not rely on the JSON string field alone: encoding/json replaces invalid UTF-8 in strings before transport. Populate the existing Bytes field and make string reconstruction prefer it;  (kb:bridge-arbitrary-go-strings-through-binary-safe-bytes)

More: `bashy kb search <query>` (or `bashy kb show <slug>`).

AFTER this issue is done, close the loop: `bashy kb retro <a few words on what you did>`
— validate/update/supersede what you consulted, or add the new lesson (distilled, never a transcript). NOOP is fine when nothing durable was learned.
