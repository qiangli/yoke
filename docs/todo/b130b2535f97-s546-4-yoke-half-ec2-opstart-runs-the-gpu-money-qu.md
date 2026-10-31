---
id: b130b2535f97
kind: feature
title: 'S546.4 (yoke half): EC2 OpStart runs the GPU money + quota preflight before StartInstances'
seq: 3029
status: todo
priority: p1
created: 2026-10-31T05:47:59.97843Z
sprint: 546
sprint_id: ae111a75-889f-50a4-a423-de1bab0fbca6
sprint_title: On-demand cloud GPU workers for large/custom Ollama models via Cloudbox and Outpost (AWS EC2 first)
---

Yoke half of S546.4 (676d853600b1), split only because work runs are per repo. Operator review gap: every on-demand wake is OpStart -> power(StartInstances) in pkg/provider/aws/ec2.go with NO money preflight; only Create runs gpuPreflight (hourly session estimate vs dhnt-monthly-cap remaining, dhnt-gpu-daily, quota L-DB2E81BA). dhnt-gpu-daily is alert-only in AWS, so repeated requests could wake a GPU all day. Scope: OpStart on a stopped worker runs the same gpuPreflight for the instance's own type (from DescribeInstances) BEFORE StartInstances; refuse with a distinct provider error code (e.g. budget_exceeded / quota_exceeded) whose message names the budget, its limit, month-to-date or today-to-date spend and the session estimate; an already-running instance needs no preflight (no-op start). OpStop never preflights. Red first with fakeec2 + fakelightsail budgets: start refused when month-to-date + session estimate > cap; refused when the daily GPU budget is at 100%; refused when quota would be exceeded; allowed under budget (exactly one StartInstances); refused start makes ZERO StartInstances calls. Gate: cd pkg/provider && go test ./... + go vet ./aws/.... No live AWS. No account ids/IPs/hostnames (OSS).
