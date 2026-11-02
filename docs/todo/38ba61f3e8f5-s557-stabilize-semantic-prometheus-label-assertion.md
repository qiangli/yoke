---
id: 38ba61f3e8f5
kind: bug
title: S557 stabilize semantic Prometheus label assertions
seq: 3051
status: assigned
priority: p1
created: 2026-11-02T00:45:58.462069Z
assignee: s557-test-readiness
sprint: 557
sprint_id: f5ccf563-f5d1-5b94-8be4-9388e51ef86d
sprint_title: 'Bashy v1.0.0 preparation: split private/public development and retire qiangli/dhnt'
---

Native41359794189 macOSworkspace TestTSRemoteIngestRendersPrometheusText failed the initial ratchet suite but PASSED its isolated confirming rerun; evidence/tmp/s557-native-125007548972.log around904. Priorlabelorderfailure retained. Diagnoseactualmetric vs maporder; preserve names/values/labelset/count no skip/baseline. Use semanticcomparisoniforderingnotcontract, deterministicserializationifit is. Boundedtests nofullsuite. WorkisolatedlatestYokeed295cc1, commitnopush.


## Delivery

The recorded macOS workspace result was one initial suite failure followed by an isolated confirmation PASS, not two failures. Both ingested samples had the correct names, values, timestamps, and label sets; randomized map iteration rendered the first sample's labels as path/job while the assertion expected job/path.

`promLabels` already collected label keys before rendering but never sorted them. It now sorts keys so an identical series has stable text across samples and map insertion/iteration order. Metric semantics and escaping are unchanged. The original ingest assertion is strengthened from a substring of the first line to the exact complete two-line request body, retaining both timestamps, both values, metric names, label sets/counts, quote escaping, sample count, request count, and the no-query assertion. Additional cases cover varying map insertion order, backslash/newline/quote escaping, and empty labels.

Validation: `GOWORK=off GOMAXPROCS=2 GOFLAGS=-p=2 go test -race ./pkg/dbts -run '^(TestTSRemoteIngestRendersPrometheusText|TestPromLabelsStableEscaping)$' -count=10 -timeout=60s` passed in 1.990s. No baseline, skip, or threshold changes. Hosted native validation remains the manager's integration gate.
