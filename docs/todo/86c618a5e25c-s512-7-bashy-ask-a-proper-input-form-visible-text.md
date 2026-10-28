---
id: 86c618a5e25c
kind: feature
title: 'S512.7 bashy ask: a proper input form — visible text fields, secret fields with a reveal toggle, choice buttons (provide / I''ll type it / skip / stop); native macOS form replaces the buggy display-dialog'
seq: 2823
status: done
priority: p0
labels:
    - ask
    - macos
    - security
created: 2026-10-28T20:29:45.034007Z
weave: 661
assignee: codex-gpt6-sol
sprint: 512
sprint_id: 9f31a532-c25d-5504-8bd8-163f39bd871d
sprint_title: 'bashy browser site skills: the Bash# skill/script duality applied to web pages (thin extension core + per-site skills, self-healing)'
closed: 2026-10-28T22:34:44.674651Z
resolution: fixed
closed_by: codex-gpt6-sol-k
---

Operator 2026-10-28: the built-in ask form is buggy on macOS (AppleScript display dialog: single hidden field, no reveal, small, cancel/timeout/focus quirks) and cannot carry the S512.5 choices. Changes: (1) field kinds: --kind text (plain visible input: SSN, username, IDs) and --kind secret (masked, with a Show/reveal toggle to verify before submit); (2) choice buttons so one dialog carries the S512.5 decision: 'Enter it here' (value -> 0600 sink), 'I'll type it in the page myself', 'Skip this question', 'Stop'; no answer / timeout = Stop; (3) macOS: replace display dialog with a native AppKit form via osascript -l JavaScript (NSAlert + NSTextField/NSSecureTextField + reveal checkbox, multi-line prompt, brought to front, positive-evidence result as today); Windows: extend the existing WinForms script the same way; Linux: zenity --forms/--entry (visible) or --password; (4) the human channel never tells the operator to run a command (drop/replace the print-a-command rendezvous as a human-facing path). S512.5 integration: 'Enter it here' fills the guarded field by REFERENCE (bashy browser type --from-ask <id>): value goes sink -> hub -> extension, never to agent stdout, receipts or logs, and the guard admits it only for that ask id + field, once. pkg/ask is protected set: this change is operator-approved 2026-10-28. Tests: unit for kinds/choices/timeout, macOS dialog script golden + a live check on Dragon (operator present).

== SHARED CONTRACT v2 (operator 13:4x PDT; SUPERSEDES the human-path choices above) ==
Choices in ONE dialog: "Enter it here" | "I'll type it in the page myself" | "Skip this question" | "Stop". No answer or timeout = Stop. The agent only ever learns the CHOICE.
Go seam (no import cycle; the manager wires the adapter at integration):
- S512.5 (pkg/browser) defines and consumes:
    type GuardAsk struct { Site, Question, Reason string; Secret bool }   // Secret: masked field (passwords/OTP) vs plain visible (SSN, username, IDs)
    type GuardChoice string // "provide" | "self" | "skip" | "stop"
    type HumanAsker interface { AskGuarded(ctx context.Context, a GuardAsk) (choice GuardChoice, askID string, err error) } // err or timeout => "stop"
  with a fake for tests, and a package var the host sets.
- S512.7 (pkg/ask) provides:
    ask.Form(ctx, ask.FormSpec{Title, Prompt string; Field ask.FieldSpec{Name string; Kind "text"|"secret"}; Choices []string; Timeout time.Duration}) (ask.FormResult{Choice string; ID string}, error)
    — on "Enter it here" the value goes only to the existing 0600 sink under ID; ask.TakeValue(id string) ([]byte, error) reads it ONCE and deletes it.
Outcomes of a guarded `bashy browser type` (live; probe/solo through the same gate):
    provide -> the core fills the field BY REFERENCE (sink -> hub -> extension), admitted once for that ask id + that field; content "guard:filled-by-human" exit 0
    self    -> the core focuses the field and waits for the human's "done"; content "guard:human-typed" exit 0
    skip    -> content "guard:skipped" exit 4
    stop    -> content "guard:stopped" exit 5 (also: no human channel, error, timeout)
  Also `bashy browser type --from-ask <id> <target>`: fill by reference from an existing ask id, admitted once for that id + field; never prints the value.
The value never reaches agent stdout/stderr, receipts, logs, extract output or the ask record. Test seam: BASHY_BROWSER_GUARD_ASK=fake:provide=<testvalue>|fake:self|fake:skip|fake:stop|fake:timeout (test builds or a test-only switch only).
For typeform-fill (S512.6): exit 0 with guard:filled-by-human or guard:human-typed -> wait for the question to advance (the HUMAN advances on self; on provide the skill may press Enter ONLY after the fill was verified by the core, never for "self"); 4 -> next question; 5 -> stop the form, exit non-zero.

== WORKER BRIEF S512.7 (codex-gpt6-sol, repo yoke, pkg/ask + pkg/ctty only) ==
Implement the body above + SHARED CONTRACT v2 (the ask.Form / ask.TakeValue side). Keep every existing bashy ask / approval path working (protected set; operator-approved change 2026-10-28). macOS: osascript -l JavaScript AppKit form (NSAlert accessory view: NSTextField for text, NSSecureTextField + "Show" checkbox for secret, multi-line prompt, the four choice buttons, activated to the front, positive-evidence result). Windows: extend the existing WinForms script the same way. Linux: zenity --forms/--entry/--password. Drop the human-facing "run this command" rendezvous text. Timeout => "Stop".
Tests: unit for kinds/choices/timeout/one-shot TakeValue; golden tests for the generated macOS/Windows scripts; no live dialog in tests (the manager runs the live macOS check with the operator).
RULES: Dragon = focused tests only (go test ./pkg/ask/... ./pkg/ctty/...). Don't touch pkg/browser. Commit on your branch, do NOT push. Trailers: Sprint: #512 / Story: #2823 / Story-ID: 86c618a5e25c. Report in sprint submit.


Operator add 2026-10-28: a fifth choice "Use clipboard" — read pasteboard only on click, validate shape against the field kind (mismatch refused, content never shown), clear right after reading only if unchanged, same by-reference path as "Enter it here", dialog notes clipboard-history/Universal Clipboard retention. S512.5 masks sensitive values in clipboard_read output.

== OPERATOR ADD (2026-10-28): 5th choice "Use clipboard" (S512.7 implements; S512.5 unchanged except clipboard_read masking) ==
(1) read the system pasteboard ONLY on that click; (2) validate the value against the field kind (SSN/EIN/card/routing/IBAN shape etc., same validators as S512.5's value check: put the shared validators where both can use them without an import cycle, e.g. a small leaf package, and tell the manager which); a mismatch = refuse with a reason and NEVER display the content; (3) clear the pasteboard right after reading, but ONLY if it still holds the same value (never clobber a newer copy); (4) the value takes the same path as "Enter it here": 0600 sink under the ask ID, filled by reference (browser type --from-ask <id>), admitted once for that ask id + field, never in agent stdout/receipts/logs. ask.Form returns Choice "clipboard" with the ID; the pkg/browser HumanAsker adapter maps it to GuardChoice "provide" (no new browser outcome); (5) the dialog states that clipboard-history apps / Universal Clipboard may keep a copy.
S512.5 also: the guard MASKS sensitive values in `browser clipboard_read` output (agents can read the clipboard through the extension today).
Tests: shape-mismatch refusal (no content in the error), conditional clear (same value cleared; a newer copy left alone), no value in any output.
Operator refinement 2026-10-28: "Use clipboard" is disabled (with a reason) when the clipboard is empty or does not match the field kind; the dialog peeks on open (in memory only, discarded) and re-checks on pasteboard change; when in doubt disable — "Enter it here" + Cmd/Ctrl+V remains. Click re-reads and re-validates.

== OPERATOR REFINEMENT (2026-10-28) to "Use clipboard" ==
The button is DISABLED (greyed, with a short reason such as "clipboard is empty" / "clipboard doesn't look like an SSN") when the clipboard is empty or its content does not match the field kind. To decide, the dialog PEEKS at the pasteboard when it opens: shape check in memory only, never stored/shown/logged, discarded immediately; re-check whenever the pasteboard changes while the dialog is open (macOS NSPasteboard changeCount; Windows/zenity equivalent or click-time only). When in doubt, DISABLE (the operator still has "Enter it here" + Cmd/Ctrl+V). On click: re-read and re-validate before use (the clipboard may have changed). The manager checks on Dragon whether macOS shows a pasteboard-access privacy prompt for the peek; if it does, the fallback is: enable the button and validate only at click time (implement both behind one switch so the manager can flip it after the live check).
Tests: empty -> disabled; wrong kind -> disabled; valid -> enabled; changed between open and click -> re-validated (and refused if it no longer matches).

== REWORK INSTRUCTIONS (run 657 delivery caf19e46 lacked the clipboard choice) ==
Your previous delivery is preserved in the yoke repo at refs/s512/run-657 (= caf19e4637d2d06ee10e63fcd094262146e7e971, branch agent/weave-issue-657). Start by bringing it into your new workspace unchanged (git fetch <yoke repo> refs/s512/run-657 && git cherry-pick caf19e46, resolving only rebase conflicts), then ADD "Use clipboard" per OPERATOR ADD + OPERATOR REFINEMENT on top as new commits. Shared validators: a small leaf package (e.g. pkg/sensitive) importable by pkg/ask and pkg/browser, no cycle; mb claude-opus5.5-f (S512.5) its name and API so the guard reuses it. Same rules and trailers as above (Sprint: #512 / Story: #2823 / Story-ID: 86c618a5e25c). Do NOT push.
Operator live findings 2026-10-28 (bugs, fixed in this story): (1) open dialog must watch the pasteboard (changeCount timer) and enable/disable "Use clipboard" live; (2) paste must work in the input fields: install a standard Edit menu on the osascript-hosted NSApp (Cmd+V/C/X/A), also accept Ctrl+V and right-click Paste.

== REWORK 2 (operator live findings 2026-10-28 ~14:40 PDT on Dragon) ==
Base: start from the yoke repo ref refs/s512/run-659 (S512.5 rework tip 20e9903e, which already contains your S512.7 commits re-picked as 665b1b51 + 4e7e2273, then S512.5's 9cc932ff, 94313af0, 20e9903e). In your new workspace: fetch refs/s512/run-659 and cherry-pick 665b1b51..20e9903e (5 commits) UNCHANGED, then add NEW commits for:
BUG 1: the open dialog does not react to a new copy. Implement a pasteboard watch loop in the native macOS form (NSTimer ~250-500 ms comparing NSPasteboard.generalPasteboard.changeCount; on change re-run the in-memory shape check and enable/disable "Use clipboard" + update its reason live; never store/show/log the content). Same on Windows (WinForms Timer or clipboard listener) and on zenity where feasible (else click-time only, documented).
BUG 2: paste does not work in the input box: the osascript/JXA NSApp has no main menu, so Cmd+V/C/X/A never reach NSTextField/NSSecureTextField. Install a standard Edit menu (Undo/Redo/Cut/Copy/Paste/Select All with the usual key equivalents) on NSApp.mainMenu while the form runs; also accept Ctrl+V as paste and right-click Paste.
Tests: the dialog-script golden asserts the Edit menu + the timer; a unit test for the enable/disable state machine on changeCount changes. The manager re-tests live with the operator (hello copied -> greyed; copy a dummy SSN while open -> becomes active; Cmd+V and Ctrl+V both paste into "Enter it here"; password field Show toggle).
Rules unchanged. Trailers: Sprint: #512 / Story: #2823 / Story-ID: 86c618a5e25c. Do NOT push.
Operator live finding 2026-10-28 (run 660): paste PASS; Show toggle closes the dialog — must be an in-form toggle that swaps secure/plain field in place, never an alert button, never ends the modal.

== REWORK 3 (operator live re-test 2026-10-28 on Dragon, run 660 build) ==
Passed live: clipboard watch (greyed -> active live), Cmd+V and Ctrl+V paste, Use clipboard fill + conditional clear, Enter it here, I'll type it myself, Skip, Stop, timeout.
FAIL: clicking "Show" on the password (secret) field makes the dialog DISAPPEAR (the run ends as guard:stopped). Likely Show is an NSAlert button (any alert button ends runModal) or its action targets the alert.
Base: yoke ref refs/s512/run-660 (= c0d27169, run 660 tip, which contains everything). In your new workspace fetch it and cherry-pick 4cef4400..c0d27169 (6 commits) UNCHANGED, then a NEW commit:
FIX: Show must be a checkbox/toggle INSIDE the accessory view with its own target/action that swaps NSSecureTextField <-> NSTextField in place, copies stringValue both ways, keeps focus and cursor, and never ends the modal; Windows: the UseSystemPasswordChar toggle, the same way. Clicking Show must not submit, record a choice, or leak the value.
Tests: the dialog-script golden asserts Show is not an alert button; a unit/golden test for the swap. The manager re-tests step 3 live with the operator.
Rules unchanged. Trailers: Sprint: #512 / Story: #2823 / Story-ID: 86c618a5e25c. Do NOT push.
