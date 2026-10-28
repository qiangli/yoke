---
id: 783ca165b81f
kind: feature
title: 'S512.5 bashy browser sensitive-data guard (enforced, protected): refuse to type SSN/ITIN/EIN, bank routing/account, card numbers, IBAN, usernames/passwords/one-time codes; hand those fields to the human'
seq: 2822
status: done
priority: p0
labels:
    - browser
    - security
created: 2026-10-28T20:22:08.846444Z
weave: 664
assignee: claude-opus5.5
sprint: 512
sprint_id: 9f31a532-c25d-5504-8bd8-163f39bd871d
sprint_title: 'bashy browser site skills: the Bash# skill/script duality applied to web pages (thin extension core + per-site skills, self-healing)'
closed: 2026-10-28T23:15:58.007188Z
resolution: fixed
closed_by: claude-opus5.5-n
---

Operator 2026-10-28: a rule agents are asked to follow is not enough; bashy must ENFORCE it. One gate in pkg/browser live Service.Execute (and probe/solo through the same gate) for type/select/evaluate/clipboard_write/dispatch_event/navigate: (1) VALUE check with validators (SSN/ITIN shape rules, EIN prefixes, ABA routing checksum, Luhn, IBAN mod-97) + exact match against bashy vault secret values; (2) FIELD check in extension page.js: refuse type into type=password, autocomplete username/current-password/new-password/one-time-code/cc-*, and fields whose name/label says ssn/tax/ein/routing/account/card/password, whatever the value; (3) HUMAN PATH: on refusal, focus the field in the operator's Chrome and ask through bashy ask so the human types it; the value never passes through agent, LLM, bashy or logs; (4) extract output masks such values shown on a page (screenshots are a recorded gap); (5) protected set (like the secrets firewall): skill-tier site skills cannot disable it, no env bypass, change only via bashy approve. Red/green fixtures per data type and per field kind; false-positive fixture (order numbers, phone numbers, dates) must pass through.

== OPERATOR ACCEPTANCE ADDITIONS (2026-10-28) ==
SCOPE: the guard applies on EVERY site, typeform.com included; no site skill (typeform-fill or any other) can opt out.
FIXTURES (Typeform-style, one question per screen):
(a) a plain text question whose VISIBLE QUESTION TEXT says "Social Security Number" / "SSN" / "Tax ID" / "Bank account" / "Password" / "Username" is refused BEFORE typing, whatever the answer value (the visible question text counts as the field label, not only name/autocomplete/aria-label);
(b) a valid SSN / card (Luhn) / ABA routing value typed into an innocuous-labelled question is refused by the value check;
(c) on refusal, typeform-fill hands that single question to the operator and continues after the human answers; it never presses Enter for them;
(d) no refused value appears in skill receipts, logs, the bashy ask record or extract output.
HUMAN PATH (operator decision): when a form REQUIRES a guarded value, the HUMAN decides. The refusal raises ONE bashy ask to the operator naming the site, the question text and why it was blocked, with choices: (1) "I will type it myself": bashy focuses the field in the operator Chrome and waits for "done"; the value never passes through agent/LLM/bashy/logs; (2) skip this question; (3) stop filling this form. No answer or a timeout = (3) stop, never a default fill. The agent sees only the choice, never the value. Fixtures for all three choices and for the timeout.

== SHARED CONTRACT between S512.5 (core guard) and S512.6 (typeform-fill) ==
The core runs the human path itself; skills only see the outcome. When the guard refuses a type/select/evaluate/dispatch-event/clipboard write, `bashy browser` (live, and probe/solo through the same gate) raises the ONE bashy ask (site, visible question text, reason; choices type-myself / skip / stop; timeout = stop), and on "type-myself" focuses the field in the operator Chrome and waits for "done". It then returns WITHOUT the value: content "guard:human-typed" exit 0; "guard:skipped" exit 4; "guard:stopped" exit 5 (also used when there is no human channel or on timeout); --json outcome_class guard_human_typed | guard_skipped | guard_stopped plus a reason code (e.g. field:label-ssn, value:luhn). The refused value never reaches stdout/stderr/logs/receipts/the ask record. Test seam for fixtures: BASHY_BROWSER_GUARD_ASK=fake:type-myself|fake:skip|fake:stop|fake:timeout, honoured only in test builds or behind a test-only switch, never in a release build; S512.5 decides and documents which.

== WORKER BRIEF S512.5 (claude-opus5.5, repo yoke) ==
Implement the guard per the body above + the shared contract. Code: yoke pkg/browser (one gate in live Service.Execute, shared by probe/solo), pkg/browser/live/extension/page.js (field check: input type, autocomplete, name/id/aria-label/placeholder, associated <label>, AND the visible question text of the enclosing question/section/fieldset/heading). Bump the extension version in manifest.json. An older extension that lacks the field check must make the hub refuse guarded actions LOUDLY (silent pass-through of a guarded field is unsafe), so raise LiveExtensionMinVersion or gate on an advertised method, your choice, documented. Value validators in Go. Vault match through the existing secrets client (cached hashes, never log values). Protected set: no env/flag bypass; changes only through bashy approve (find and reuse the existing secrets-firewall protected-set mechanism). extract masks matching values shown on a page.
Fixtures: Typeform-style pages per data type and field kind for (a)-(d) above; a false-positive page (order numbers, phone numbers, dates, ZIP codes, amounts) must pass through; human path via the fake seam: type-myself, skip, stop, timeout. Page-logic tests like pkg/browser/live/page_test.go (BASHY_BROWSER_LIVE=1, headless Chrome); Go unit tests for validators and the gate.
RULES: Dragon = focused tests only (go test ./pkg/browser/... ./cmds/browser); no podman. Do not touch skills/ or pkg/skills. Commit on your branch, do NOT push. Trailers: Sprint: #512 / Story: #2822 / Story-ID: 783ca165b81f. Report files, tests and results in sprint submit.

== OPERATOR ADD (2026-10-28): 5th choice "Use clipboard" (S512.7 implements; S512.5 unchanged except clipboard_read masking) ==
(1) read the system pasteboard ONLY on that click; (2) validate the value against the field kind (SSN/EIN/card/routing/IBAN shape etc., same validators as S512.5's value check: put the shared validators where both can use them without an import cycle, e.g. a small leaf package, and tell the manager which); a mismatch = refuse with a reason and NEVER display the content; (3) clear the pasteboard right after reading, but ONLY if it still holds the same value (never clobber a newer copy); (4) the value takes the same path as "Enter it here": 0600 sink under the ask ID, filled by reference (browser type --from-ask <id>), admitted once for that ask id + field, never in agent stdout/receipts/logs. ask.Form returns Choice "clipboard" with the ID; the pkg/browser HumanAsker adapter maps it to GuardChoice "provide" (no new browser outcome); (5) the dialog states that clipboard-history apps / Universal Clipboard may keep a copy.
S512.5 also: the guard MASKS sensitive values in `browser clipboard_read` output (agents can read the clipboard through the extension today).
Tests: shape-mismatch refusal (no content in the error), conditional clear (same value cleared; a newer copy left alone), no value in any output.

== OPERATOR ADD (2026-10-28): protect the shared validators ==
yoke/pkg/sensitive (the shared leaf validator package, created by S512.7; S512.5 moves its pkg/browser/guard validators into it and reuses ClipboardShape/Category) now decides what the guard blocks, so it MUST be PROTECTED like the secrets firewall: add yoke/pkg/sensitive AND the guard gate in pkg/browser to the protected set: dhnt docs/bashy-invariant-core.md Appendix A row "Secrets" plus whatever table the M1 protected-set enforcement actually reads (find it: atlas class table / protected path list). Skill-tier edits (site skills, skill run --adapt overlays, bashy src local PRs at skill tier) must be unable to weaken it. Acceptance: a test proving a skill-tier change to pkg/sensitive or the gate is refused by that enforcement.

== REWORK INSTRUCTIONS (run 656 delivery aaf4f8e4 used the SUPERSEDED v1 human path) ==
Your previous delivery is preserved in the yoke repo at refs/s512/run-656 (= aaf4f8e491084cc43c14772f438e225a22ee69d8). S512.7's delivery is at refs/s512/run-658 (dbd91412 + ac41a785: ask.Form/ask.TakeValue, 5-choice native forms, and the leaf pkg/sensitive with ClipboardCategory/ClipboardShape/ClipboardPattern).
In your new workspace, in this order and unchanged (resolve only rebase conflicts): (1) fetch both refs; (2) cherry-pick dbd91412 and ac41a785 (S512.7); (3) cherry-pick aaf4f8e4 (yours). Then as NEW commits:
A. Human path -> SHARED CONTRACT v2: GuardAsk/GuardChoice/HumanAsker seam in pkg/browser (guard pkg OK) + fake; choices provide/self/skip/stop (clipboard maps to provide); provide = fill BY REFERENCE via a value-source seam (askID -> bytes; the host adapter uses ask.TakeValue), admitted ONCE per ask id + field; `browser type --from-ask <id> <target>`; outcomes guard:filled-by-human 0 | guard:human-typed 0 | guard:skipped 4 | guard:stopped 5. REVERT your own pkg/ask edits (approve_cmd.go, approve_option_test.go) unless S512.7's ask.Form genuinely lacks something; pkg/ask belongs to S512.7. Write the host adapter HumanAsker -> ask.Form/ask.TakeValue in a small file outside pkg/ask and pkg/browser/guard (e.g. pkg/browser/guardask) so bashy can set it at init.
B. Validators -> pkg/sensitive: MOVE your pkg/browser/guard validators (CheckValue/Scan/ValidSSN/ITIN/EIN/ABA/Luhn/IBAN) into pkg/sensitive, keeping S512.7's API, and make ClipboardShape AUTHORITATIVE with checksums (ABA 3-7-1, Luhn, EIN prefixes, ITIN groups, IBAN mod-97). The native dialog peek (ClipboardPattern) stays a regex PRE-FILTER that may only DISABLE the button; update S512.7's test that asserts regex == Go check accordingly (e.g. a regex miss implies a Go miss, not the reverse). Tests: routing 123456789 refused (bad checksum), a valid routing number accepted, Luhn card, EIN prefix, ITIN group.
C. Protected set (OPERATOR ADD above): pkg/sensitive + the guard gate in the protected set that the M1 enforcement reads (your pkg/src/manifest.go change may already be that; extend it to pkg/sensitive) + the Appendix A row text for the manager to put in dhnt docs/bashy-invariant-core.md (put the proposed text in your submit; the umbrella doc is the manager's). Test: a skill-tier change to pkg/sensitive or the gate is refused.
D. clipboard_read masking (OPERATOR ADD above), if not already in aaf4f8e4.
Rules as above. Trailers: Sprint: #512 / Story: #2822 / Story-ID: 783ca165b81f. Do NOT push. Submit with the exported names of the seam + adapter.

== REWORK 2 (operator 2026-10-28, after the live dialog check passed) ==
Your S512.5 work is now on yoke local main (b6027041, integrated under your binding; S512.7 + all live dialog fixes included). Start from main as cloned.
ADD: the fill-by-reference path (provide via "Enter it here" or "Use clipboard", and `browser type --from-ask`) must VERIFY that the page field equals the sink value and report ONLY the result: data.verified = "match" | "mismatch", plus data.field_len and data.value_len. NEVER the value, and NEVER a hash of it (an SSN-class value is brute-forceable). Do the comparison where the value already is (the extension page core, comparing inside the page, or the hub just before the response is built), so no new path ever carries the value back to the agent. A mismatch is a failed fill: exit non-zero with guard outcome "guard:fill-mismatch" (pick the code and document it beside 0/4/5).
Tests: a fixture where the page accepts the value (match), and one where the page rewrites or truncates it (mismatch); assert neither the value nor a hash appears in any output.
Rules unchanged. Trailers: Sprint: #512 / Story: #2822 / Story-ID: 783ca165b81f. Do NOT push.

== REWORK 3 (live failure 2026-10-28 on Dragon, run 662 build 9b2331e4, extension 0.9.2) ==
REGRESSION: on the live hub with the real extension, a guarded `browser --mode live --json type 'input[aria-label=ssn]' <dummy SSN>` on the S512.6 fixture typeform-sensitive-a.html, answered by the operator with "Use clipboard"/"Enter it here", returns guard:stopped via fill-failed (exit 5) with no verified/field_len/value_len; clipboard not cleared. The SAME live case on the run 661 build (extension 0.9.1) ended guard:filled-by-human with the field filled (length 11). Your page tests (TestGuardFill*) pass in headless Chrome, so the break is in the path they do not cover: hub -> background.js guardFill (runInTab "guardFill"/"guardVerify", the line ~339 text-handling branch for guard_fill) -> Go live guardBackend.FillAdmitted/fillVerdict (raw shape: top-level vs data string).
Base: yoke ref refs/s512/run-662 (9b2331e4) on top of local main b6027041 (cherry-pick 9b2331e4 UNCHANGED, then fix as a NEW commit).
FIX + TEST: find and fix the cause; ADD an end-to-end test that loads the REAL extension behind a real hub in headless Chrome (or the closest existing harness: see how S511 tests drive background.js) and runs guard_fill through Service.Execute with a fake asker answering "provide", asserting outcome guard:filled-by-human with data.verified=="match" and the two lengths, and a mismatch fixture asserting guard:fill-mismatch exit 6. Keep the generic errors, but have the hub log a VALUE-FREE cause code (e.g. ext-threw | no-verdict | parse) so a live failure is diagnosable.
Rules unchanged. Trailers: Sprint: #512 / Story: #2822 / Story-ID: 783ca165b81f. Do NOT push.

== FINAL COMMIT (after the live read-back proof PASSED on d404fb98: verified=match 11/11) ==
Base: yoke ref refs/s512/run-663 (a70eb825 + d404fb98) on top of local main b6027041: cherry-pick a70eb825 d404fb98 UNCHANGED, then ONE new commit: add pkg/browser/live/extension/{background.js,page.js,guard.js} (the guard tab pin, field check and in-page verify) to the pkg/src protected manifest, with a test that a skill-tier change to each is refused (same style as the pkg/sensitive rows). Nothing else.
Trailers: Sprint: #512 / Story: #2822 / Story-ID: 783ca165b81f. Do NOT push.
