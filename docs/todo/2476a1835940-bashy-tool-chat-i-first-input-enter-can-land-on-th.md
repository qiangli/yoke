---
id: 2476a1835940
kind: bug
title: 'bashy <tool> / chat -i: first input + Enter can land on the vendor''s folder-trust dialog and pick its default (''No, exit'')'
seq: 3137
status: done
priority: p1
labels:
    - agents
created: 2026-11-20T10:47:01.875502Z
assignee: claude-opus5.5
sprint: 576
sprint_id: 0dcfdbeb-04ba-549f-96e0-a3febc9a6618
sprint_title: 'Proactive sprint manager: wake, supervise, review'
closed: 2026-11-20T18:11:51.224702Z
closed_by: claude-opus5.5
---

2026-11-20: 'bashy claude' in an untrusted folder (/private/tmp/s674-wt) shows Claude Code's trust dialog with the cursor on 'No, exit'; the operator saw bashy exit immediately. Likely cause (to confirm): since yoke 56346984/462bb170 the readiness check treats a static screen as 'settled', so bashy delivers its first input (inbox prepend / queued steer / opening instruction) plus Enter into the dialog, selecting the default and exiting the vendor CLI. Fix options: recognise vendor trust/consent dialogs (tool seeds already know trust_preseed for claude/agy) and never send into them — preseed trust for the launch cwd or wait for the human; or only deliver after a tool-declared ready pattern. Repro without quota: fresh untrusted dir, bashy claude, watch the screen.

Operator decision 2026-11-20: bashy should PRESEED trust, not auto-answer the dialog. For 'bashy <tool>' (yolo default) write the vendor's own trust entry for the launch cwd only before launch (claude: the per-project trust flag in its config; agy: .gemini/antigravity-cli/settings.json trustedWorkspaces with the realpath; others per their seed's trust_preseed), log the exact entry written, and skip it with --no-trust (and with --no-yolo), leaving the vendor's dialog to the human. Reuse the existing trust_preseed seed field and agent-bench scripts/trust_preseed.py semantics. Second layer kept: never deliver input into a recognised trust/consent dialog. Tests: untrusted dir -> trust written for that dir only, no dialog; --no-trust -> dialog shown, nothing sent into it.

Operator decision 2026-11-20 (amended: 'or both'): layered. (1) preseed trust for the launch cwd (as above); (2) FALLBACK only if a recognised trust dialog still appears: navigate to the option whose text names trust ('Yes, I trust this folder') and confirm THAT option — never a blind Enter — log 'answered trust dialog for <dir>' and report the preseed miss; (3) never deliver ordinary input (inbox, instruction, steer) while a dialog is on screen. --no-trust disables (1) and (2).

Closed 2026-11-20 (S576 triage, conductor): Fixed per operator layered decision: yoke 9376cd7a, merge e1d879d1 (run #91) preseed + trust-option fallback, never type into dialogs; 8cbf212a symlink-resolved preseed; pinned bashy 6c618aaa/71649157; in rc.38 candidate. Merged-run evidence reused; no fresh tests.
