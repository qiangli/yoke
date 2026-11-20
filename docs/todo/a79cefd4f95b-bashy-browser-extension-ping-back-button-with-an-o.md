---
id: a79cefd4f95b
kind: feature
title: 'bashy browser extension: ping-back button with an optional message box'
seq: 3140
status: assigned
priority: p1
created: 2026-11-20T13:44:14.501114Z
weave: 94
assignee: claude-fable5.2
sprint: 680
sprint_id: af559546-24b2-5355-8565-b4f41449486e
sprint_title: 'Human-in-the-loop browser: ping-back and remote headless web-app testing'
---

Operator 2026-11-20: in the live-mode extension popup, a 'Send this tab to the agent' button plus an OPTIONAL text box for an additional message. Sends {tab id, url, title, message} to the agent's inbox (bashy mb), so a running session sees it like a steer and can act with --id (or --active). Needs: which agent (default: the live chat session / last addressed agent, selectable), the extension change, and a store re-submission (CWS/Edge). Pairs with --active.

Why (operator, 2026-11-20): a browser the agent launches (`browser --headed login`, solo mode) is a fresh profile with no password manager, so the human cannot sign in there — passwords live in the password manager of their own Edge/Chrome. The flow is therefore human-first: the human launches their own browser (Edge or Chrome, live-mode extension installed), signs in and chooses the tab, then pings back with the button (+ optional message). The agent then acts on exactly that tab over the live hub (`--id` from the ping, or `--active`). Agent-launched login flows should point the human to this path instead of opening a browser they cannot use.

Acceptance: Edge and Chrome; the ping reaches the addressed agent's inbox with tab id/url/title and the optional message; the agent can read that tab (e.g. cookies-get) without the human typing anything into the terminal.

Scope widened (operator, 2026-11-20) — two-way, many tabs, many browsers:
- Human → agent: from ANY tab in ANY browser (Edge, Chrome; each has its own hub port), "Send to agent" + optional note. Each ping carries {browser, tab id, url, title, note}, so per-tab verdicts stay attached ("Edge superadmin tab: the login is wrong — see the error"; "Chrome guest tab: perfect"). The agent then addresses exactly that tab: --browser <name> --id <tab>.
- Agent → human: the agent can post a request ("open the superadmin login in Edge") that shows as a badge + line in the extension popup; the human answers by pinging the tab (with or without a note). Requests and replies are one thread on the bus.
- The chat/TUI stays a valid channel; this adds tab-anchored messages, it does not replace it.
MVP order: (1) human→agent ping with note, both browsers; (2) agent→human request shown in the popup and answered by a ping.

## Step 1 implementation notes (conductor, 2026-11-20)

Implement STEP 1 ONLY of story a79cefd4 (read the story file docs/todo/a79cefd4f95b-*.md first; step 2, agent->human requests, is out of scope).

Goal: from ANY tab in the human's own Edge or Chrome (live-mode MV3 extension, pkg/browser/live/extension), the human presses "Send to agent" in the popup, optionally types a note, and the agent receives one message per ping on the bashy message board with {browser, tab id, url, title, note, at}. The agent can then act on exactly that tab: `bashy browser --mode live --browser <name> --id <tab>`.

Design constraints:
- Extension -> hub over the existing WebSocket protocol (protocol.go/server.go): a new message type; the hub (`bashy browser hub`, Go side) posts it with bus.PostMessage. The extension writes nowhere else.
- Recipient: a "To" field in the popup (agent or session name, remembered per browser in extension storage). Default when empty: the most recently joined live interactive chat session in the host room (room cards, mode interactive); if none, post to the operator's own inbox and say so in the popup. Topic "browser".
- Browser identity: which browser the hub serves (its --browser / port: chrome 58082, edge 58084) goes in the message, so tabs in different browsers never collide.
- The note is untrusted human text: carry it verbatim as data; no HTML rendering anywhere.
- Keep the extension minimal: no new permissions unless unavoidable; list any new permission in the commit message (store review is affected). Bump the extension version.
- Agent side: a way to list received pings, e.g. `bashy browser pings [--json]` (a thin read over the board, topic browser), and mention the ping in the help for `login` and `--active`.

Tests: Go unit tests for hub message -> bus post (recipient default, browser field, note verbatim); follow the extension's existing test pattern (extension_test.go / extension_e2e_test.go) for the popup/message. Targeted only: `GOWORK=off go test ./pkg/browser/... ./cmds/browser` and `scripts/fmtcheck.sh`. No heavy suites. Root cause before any fix if something pre-existing fails.

Commit with the trailer `Story-ID: a79cefd4f95b`. Do not push; the conductor reviews and merges.
