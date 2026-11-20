---
id: a79cefd4f95b
kind: feature
title: 'bashy browser extension: ping-back button with an optional message box'
seq: 3140
status: todo
priority: p2
created: 2026-11-20T13:44:14.501114Z
---

Operator 2026-11-20: in the live-mode extension popup, a 'Send this tab to the agent' button plus an OPTIONAL text box for an additional message. Sends {tab id, url, title, message} to the agent's inbox (bashy mb), so a running session sees it like a steer and can act with --id (or --active). Needs: which agent (default: the live chat session / last addressed agent, selectable), the extension change, and a store re-submission (CWS/Edge). Pairs with --active.
