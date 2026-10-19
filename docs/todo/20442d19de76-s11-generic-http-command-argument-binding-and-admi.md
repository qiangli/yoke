---
id: 20442d19de76
kind: feature
title: S11 generic HTTP command argument binding and admission metadata
seq: 1897
status: done
priority: p1
labels:
    - yoke
created: 2026-10-11T08:54:00.457707Z
weave: 95
assignee: codex-gpt6-sol
sprint: 418
sprint_id: 9891800f-8da5-5338-92bc-1d75755ca548
sprint_title: 'Agent ecosystem synergy: agentic tools compose like a|b|c through bashy extension points'
closed: 2026-10-11T09:17:27.27163Z
resolution: fixed
closed_by: codex-gpt6-sol
---

Scoped prerequisite for Bashy S11 Codync declaration #14952455911e after its #29 no-op. Extend Yoke pkg/toolcmd typed HTTP transport with declarative positional/named argument binding into JSON body and bounded enum projection (e.g. allow|deny to host enum) plus shared mutation/admission metadata that Bashy can honor for --yes. No Codync-name conditionals, shell/eval/curl, per-command wrappers or string-interpolated JSON. Preserve endpoint-only redirect and bearer-token firewall. Add focused parser/validation/execution tests: two arguments remain distinct JSON fields, enum maps only allowed values, missing/extra/invalid args fail before HTTP, effects gate blocks write absent confirmation, loopback bearer and redirects safe. Document portable YAML schema. Verify GOWORK=off go test ./pkg/toolcmd/... ./pkg/fleet/... and Mac/Linux focused gate. Commit Story-ID and Agent trailer. dev-host short tests only.
