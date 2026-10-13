---
id: 46787d701f6b
kind: bug
title: Include signed Prima no-egress evidence rows in verifier allowlist
seq: 2016
status: assigned
priority: p1
assignee: codex-gpt6-sol
sprint: 436
sprint_id: 20ad6f7b-d3d3-5ebf-973c-0bbb999db96f
---

Bashy's Prima release workflow uploads every `out/*` file from each successful
native build. Its no-egress gate creates `prima-<os>-<arch>-egress.txt` for all
four supported platforms, so the signed SHA256SUMS includes those rows.
Allow only those exact four supplementary names. Test a complete release
manifest and rejection of unknown or unsafe extras. Preserve exact workflow
identity and issuer, the independent executable SHA pin, duplicate and
traversal checks, and offline cache verification. Do not add SHA pins until
hosted CI signs assets. Do not push or run a Dragon native build/container.
