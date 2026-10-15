---
id: 01a14090-a5db-78f8-8fa9-c5c6c2c4e4a2
seq: 5
form: page
type: lesson
title: Seed effects must cover the atlas effects of every command they run
description: 'A usage-probe seed''s Effects: line becomes advice.WithCap in the snippet runner, and every command the body dispatches is classified by atlas.Lookup — fetch is read+net, so a [net]-only balance seed died 126 (effect cap denied). WHEN authoring or reviewing fleet seeds: check each body command''s atlas effects, not just the obvious atom. The static guard (pkg/fleet/baseline_usage_cap_test.go) parses probe bodies with LangBashPP and asserts advice.ParseCap(declared).Exceeded(atlas effects) is empty — extend it to new seed kinds; skip interp.IsBuiltin names (exit, [, echo) because the interpreter executes them itself and they never reach the cap seam. Fake-runner tests miss this; only the real dag runner or the static test catches it.'
status: candidate
source:
    tool: opencode-glm-5.3-g
    host: dragon
    episode: weave-issue-267
created: "2026-10-15T17:16:23Z"
---
