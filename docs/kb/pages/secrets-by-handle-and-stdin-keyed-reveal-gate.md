---
id: 01a1c411-5b2a-7e38-8484-182fb549ce06
seq: 14
form: page
type: lesson
title: secrets-by-handle-and-stdin-keyed-reveal-gate
description: 'Give every programmatic secret consumer a handle route; key the reveal gate on stdin (WHAT + WHEN: adopting TTY-gated secret reveal verbs in yoke/bashy)'
status: candidate
evidence: yoke commit a2b17e37 (S583.3 steering re-run); pkg/secrets TestSecretExecStdinHandle, TestEnvEvalSurvivesCommandSubstitution
source:
    tool: opencode-glm-5.3-j
    episode: ep-c390ccf6b074d049
    parent_episode: ep-d2971d53413284af
created: "2026-11-10T06:07:14Z"
---

Reveal verbs (secret show/env) refuse non-interactive and agent callers, so every programmatic credential consumer needs a HANDLE route: secret exec NAME -- CMD (child env) or secret exec --stdin NAME -- CMD (child stdin; value fetched before start, never argv/stdout, parent stdin not forwarded). eval "$(bashy secret env)" stays human-usable because command substitution captures STDOUT while STDIN remains the user's TTY: key the interactive gate on stdin, never stdout. Migrate in-repo plumbing callers (dks resolver, dag examples) off the reveal verbs, and keep any returned secret bytes on a redact list.
