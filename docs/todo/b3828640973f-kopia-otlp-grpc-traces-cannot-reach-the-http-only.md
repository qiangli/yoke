---
id: b3828640973f
kind: bug
title: kopia OTLP/gRPC traces cannot reach the HTTP-only local collector
seq: 2003
status: done
priority: p3
created: 2026-10-12T18:05:57.277361Z
assignee: muse-spark1.2
sprint: 434
sprint_id: f0fadef8-daec-5fc2-9c78-40979f01a0a7
sprint_title: bashy verified builds for binmgr tools and services (fork, build, sign, distribute)
closed: 2026-10-12T19:00:33.019592Z
closed_by: s434-manager
---

Sprint 434 (OTel rule: services' OTel enabled and pointed ONLY at bashy's local collector). yoke main a68f68bd made kopia run with --otlp-trace and pinned OTEL_EXPORTER_OTLP_* to http://127.0.0.1:31415 (external/internal/serviceenv). Kopia v0.23.1 --otlp-trace uses the Go OTLP/gRPC exporter, but external/otel/stack's proxy on 31415 accepts OTLP/HTTP only, so kopia traces fail locally (no vendor leak - policy holds, but traces are lost).
Task, KISS, pick the smaller fix after checking kopia v0.23.1 source (repo/telemetry or internal/otlp setup): (a) if kopia's exporter honours OTEL_EXPORTER_OTLP_PROTOCOL / an http exporter option, set it for kopia only; else (b) add a loopback-only OTLP/gRPC receiver to external/otel/stack (127.0.0.1, port next to 31415, forwarding to the same stores) and give kopia the gRPC endpoint via serviceenv. Never a non-loopback bind, never a vendor endpoint.
Acceptance: unit test asserting kopia's child env/args target a receiver that speaks its protocol; focused go test of touched packages green (external/otel is its own module: run go test inside external/otel). Commit in yoke with Sprint/Story/Story-ID trailers, then bashy sprint submit 434 b3828640973f.
