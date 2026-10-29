---
id: 01a18921-9e4a-7aa2-80e0-8ff84032129f
seq: 12
form: page
type: gotcha
title: Bare Bash# decorator without parens parses as a plain command, not a decorator
description: 'WHEN reading Bash# isolation requests with sh/syntax LangBashPP: a bare @sandbox line (no parens) does not attach to the function declaration and parses as a CallExpr with literal @sandbox, so a decorator-only walk silently reports no request and the unit runs unsandboxed. Match CallExpr nodes whose command word is the bare literal too (quoted forms are string parts, not literals, and stay prose), and refuse unreadable contracts that textually name @sandbox. Applies to any @-decorator read in pkg/unit.'
status: candidate
source:
    tool: muse-conductor-s
    episode: ep-0c0c0bdbe94699b5
    parent_episode: ep-76eca2b24645f668
created: "2026-10-29T19:27:24Z"
---
