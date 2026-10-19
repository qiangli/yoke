---
id: 01a129d0-1cbd-732c-ad68-2cbb872c4bc3
seq: 3
form: page
type: gotcha
title: Run llmgw gates from its nested module
description: WHEN a yoke gate names ./pkg/llmgw/..., the root Go module rejects that pattern because pkg/llmgw has its own go.mod. Run go test ./... (and go vet ./...) with cwd pkg/llmgw; keep the root cligw, broker, and fleet gates separate so the nested-module setup error cannot mask their results.
status: candidate
source:
    tool: codex-gpt5.6-sol-b
    episode: ep-dd7732ba7e9c708e
created: "2026-10-11T07:14:27Z"
---
