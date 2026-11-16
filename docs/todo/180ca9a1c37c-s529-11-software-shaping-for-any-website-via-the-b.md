---
id: 180ca9a1c37c
kind: feature
title: S529.11 software shaping for any website via the bashy live extension
seq: 3123
status: todo
priority: p2
labels:
    - browser
created: 2026-11-16T18:22:36.467797Z
sprint: 529
sprint_id: 61593fcd-15ea-578f-b8ca-527620119de2
sprint_title: 'bashy browser extension: store distribution + cross-browser (Chrome Web Store, Edge/Windows, Safari)'
---

Operator request 2026-11-16: let a user shape any website they use (layout, hidden clutter, extra buttons, filled defaults, small workflows) through the extension, driven by bashy's shaping flow (bashy shape wish -> an agent drafts the change -> the user previews and keeps it), per site, owned by the user, regenerate-not-merge. Store-policy constraint: MV3 forbids executing code not shipped in the package, so agent-written JS must go through chrome.userScripts (Chrome 120+; needs the userScripts permission AND the user enabling 'Allow User Scripts' for the extension). Adding a permission after publication is the costly update (deeper review; existing users must re-accept, extension disabled until they do), so decide before 0.11.x is widely installed: either add userScripts in the next store update while the install base is ~0, or ship CSS-only shaping first (scripting.insertCSS needs no new permission) and add userScripts later. Spike first: capability matrix (CSS vs userScripts vs DOM edits through existing actions), the preview/keep/undo UX, storage (local-only, per-site, exportable), and how shapes sync with the bashy shaping engine. Cross-browser: userScripts exists in Chrome/Edge; Firefox has userScripts; Safari differs.


Operator 2026-11-16, two ways around the permission cost: (a) add userScripts later as an OPTIONAL permission, requested at runtime when the user turns shaping on (an optional permission added in an update does not disable the extension; verify userScripts may be optional in current Chrome); (b) preferred: shape in a COMPANION TAB, not inside the site: the shaped UI is a hub-served page (same mechanism as copy sheets, opened beside the site in split view) that drives the site through the existing extension actions (extract/type/click/select) plus CSS via scripting.insertCSS. No agent-written code runs in the site, so no remote-code policy issue and no new permission.

Operator 2026-11-16: the companion tab is a general building block, not only for shaping: e.g. a summary (or translation, notes) view beside the original page in split view. Candidate verb: `bashy browser beside FILE.html|.md` — the agent writes the page, the hub serves it (sheet dir, token URL) and open_beside puts it next to the driven tab (shipped with the copy-sheet split view, yoke 8cc18c9c).
Also (operator): a visual, poster-style graphic summary beside long, detailed prose — the companion tab carries any agent-made view (summary, poster, translation, notes) next to the source.

Operator 2026-11-16: make the shaping state visible. Distinguish producer-side shaping only from consumer-side shaping active, and show whether the user is signed in to dhnt.io (the cloudbox account), so users know which mode applies. Surface it in the extension (badge or popup line, beside the S529.10 hub badge) and in the tray (status line/tooltip), from one shared state the hub reports.
