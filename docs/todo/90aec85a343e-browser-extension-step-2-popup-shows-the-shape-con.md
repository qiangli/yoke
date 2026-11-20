---
id: 90aec85a343e
kind: feature
title: 'browser extension step 2: popup shows the Shape conversation (chat/meet room)'
seq: 3145
status: todo
priority: p1
created: 2026-11-20T16:47:35.84191Z
sprint: 680
sprint_id: af559546-24b2-5355-8565-b4f41449486e
sprint_title: Human-in-the-loop browser and Shape conversation
---

Follow-up to a79cefd4 (step 1 Shape button merged in yoke f7cf7eaf, extension 0.12.0). Operator design 2026-11-20: the agent joins the same chat/meet conversation that Shape posts into; the popup shows that conversation (last few messages, each with its tab) and Pop out opens the full room in the existing meet web UI (meet serve). No chat UI built inside the extension; reuse meet's room view and event feed. Agent replies are ordinary DM/chat replies. Pairs with the tray Chat item (bashy story on Sprint 680). Acceptance: after a Shape, the agent's reply appears in the popup within seconds on Edge and Chrome; Pop out opens that room; several tabs/browsers feed one ordered conversation.
