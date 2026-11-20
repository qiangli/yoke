---
id: a79cefd4f95b
kind: feature
title: 'bashy browser extension: ping-back button with an optional message box'
seq: 3140
status: done
priority: p1
created: 2026-11-20T13:44:14.501114Z
weave: 95
assignee: claude-fable5.2
sprint: 680
sprint_id: af559546-24b2-5355-8565-b4f41449486e
sprint_title: 'Human-in-the-loop browser: ping-back and remote headless web-app testing'
closed: 2026-11-20T15:39:40.793865Z
resolution: fixed
closed_by: claude-fable5.2-q
---

## FINAL SPEC — step 1 (operator decisions 2026-11-20; supersedes everything below)

1. Popup: just ADD a note textarea, ONE button labelled "Shape", and a one-line sent/error status to the existing popup. No restyling, no reordering, no other verbs, no multi-tab selection.
2. Shape sends the CURRENT tab only: {browser (the hub's chrome|edge), tab id, url, title} + the optional note (multi-line, up to ~8 KB; tell the human if truncated).
3. Transport = the SAME bashy chat/meet infrastructure, no new transport or store:
   - if the addressed agent has a live chat session: deliver as a steer through the existing chat steer path (pkg/chat), like `bashy chat steer`;
   - otherwise: post into the existing meet DM room between the human and that agent (pkg/meet), which notifies via the board like meet does today.
   - The tab fields travel in the message; long notes use meet's existing long-body/attachment path (check before adding anything).
   - Recipient: a "To" field in the popup (remembered per browser); empty = the most recently joined live interactive chat session, else the operator's own inbox (say so in the status line).
4. No `bashy browser ping`/`pings` verb and no separate ping store; the agent reads it with `bashy inbox` / its chat session like any message.
5. Extension icon: the Shape icon (Flow mark from cloudbox ui/src/features/desktop-settings/shape-button.tsx FlowMark: the FLOW path twice, second rotate(180 128 128), 256x256, no voice dot) as icons/shape.svg; regenerate icons/icon-{16,32,48,128}.png from it with a deterministic local rasterizer (record the command); manifest paths unchanged; note in the commit that store listing icons follow.
6. Help: `bashy browser --help` describes the tab's Shape button; login help keeps pointing to the human's own browser + --active.
7. Tests: Go unit tests for hub Shape → chat steer vs meet DM routing, recipient default, tab fields, note verbatim and the size cap; the extension's existing popup test pattern; targeted only (GOWORK=off go test ./pkg/browser/... ./cmds/browser; scripts/fmtcheck.sh). Existing popup tests keep passing.
8. Commit trailers: Sprint: #680 / Story: #3140 / Story-ID: a79cefd4f95b. Do not push.

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


Problem statement (operator, 2026-11-20) — what this story must solve:
a) the human does not know which browser/tab the agent opened — tabs an agent drives must be visibly marked (the tab list already tracks `driven`; surface it in the browser, e.g. badge/title marker) — next step after step 1;
b) the agent's browser has no saved passwords — solved by the human choosing the browser/tab and pinging it;
c) associating an error or page content with the agent's request is awkward — the ping's note carries pasted error/content for that tab.
Step 1 = ONE tab per ping + optional note (multi-line, up to ~8 KB; full note stored in a ping file, short board post with a reference since posts cap at 1024 bytes). Multi-tab selection: dropped as overkill.

Popup (operator decision, 2026-11-20): ONE "Shape" button covers all cases — ping-back (look at this tab; here is the error/content) and shaping the content, for producers and consumers alike; the receiving agent interprets the note. No separate verbs, no multi-tab selection.

Extension icon (operator decision, 2026-11-20): the Shape icon (Flow mark, cloudbox shape-button.tsx FlowMark) becomes the extension icon — one mark for the family; store listing icons follow.

Popup layout (operator decision, 2026-11-20): keep the existing popup content (hub port, Connect this tab / Disconnect, console mirror, Pop out, Clear, log/trace); add the Shape section on top, styled like the consumer Shape panel (Flow mark, note box, one Shape button, feedback line).

Popup (operator, final, 2026-11-20): just add a note textarea + one Shape button (+ one status line) to the existing popup; no restyling or reordering.

Design (operator, 2026-11-20): the extension is just another chat input to bashy. Shape = an ordinary board message to the agent (as bashy mb send) with the tab attached (browser, tab id, url, title); read with bashy inbox; no separate ping store or listing verb. Step 2 replies are ordinary messages back, shown in the popup.

Infra (operator, 2026-11-20): Shape uses the same bashy infrastructure as chat/meet — a live chat session gets it as a steer (pkg/chat), otherwise it goes into the meet DM with that agent (pkg/meet); replies are ordinary DM/chat replies. No new transport.

Step 2 (operator, 2026-11-20): the agent simply joins the same chat/meet conversation the extension posts into; the popup shows that conversation (last few messages) and Pop out opens the full room in the existing meet web UI (meet serve). No chat UI built inside the extension; every message keeps its tab context, so several tabs/browsers feed one ordered conversation.
