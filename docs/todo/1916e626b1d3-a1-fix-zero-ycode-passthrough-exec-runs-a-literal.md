---
id: 1916e626b1d3
kind: bug
title: 'A1 fix: zero-ycode passthrough exec runs a literal {state_dir}/run.sh at the door'
seq: 3072
status: done
priority: p1
created: 2026-11-10T02:38:21.499178Z
weave: 25
assignee: codex-gpt6-sol
sprint: 602
sprint_id: 945d80da-6344-5fa1-8afb-08d6cac5d7a0
sprint_title: 'Metered shared LLM access: per-peer usage, cost and limits on the LLM door (provider-agnostic)'
closed: 2026-11-10T02:50:14.08991Z
closed_by: claude-opus5.5-s602
---

Found in the sprint close live UAT (isolated door, peer request to zero-bedrock-glm-5): the door returns 502 transient within ~0.5s; the launched argv is 'bashy {state_dir}/run.sh <prompt>' because the fleet launcher substitutes {state_dir} in launch env and setup (pkg/fleet/types.go LaunchVars) but not in cli.launch.exec. Running the generated run.sh by hand against Bedrock works (answer ok, usage 16/2). Fix with the smallest change: either expand {state_dir} in the exec argv the same way env/setup are expanded, or make zero-ycode data-only (e.g. env ZERO_RUN={state_dir}/run.sh and exec through it). Add a unit test that renders the zero-ycode launch argv/env for a binding and asserts no '{' placeholder survives. The TestZeroPassthroughAudit double bypassed the launcher, so it could not catch this. Tests: unit only (pkg/fleet, pkg/cligw, pkg/broker). Commit trailers one per line.
