---
name: run-01jb7cexample0000000000000
description: bashy run manifest for render (generated; do not edit)
type: dag
metadata:
  run-kind: one-shot
  workspace: ws-01jb6zexample0000000000
  run-id: 01JB7CEXAMPLE0000000000000
  command:
    name: render
    digest: sha256:4444444444444444444444444444444444444444444444444444444444444444
  artifact:
    darwin/arm64: sha256:3333333333333333333333333333333333333333333333333333333333333333
    linux/amd64: sha256:1111111111111111111111111111111111111111111111111111111111111111
    linux/arm64: sha256:2222222222222222222222222222222222222222222222222222222222222222
  cwd: ~/docs
  egress: none
---
## run
Venue: cluster
Distribution: single
Match: kubernetes.io/os=linux
CPUPerTask: 2
Memory: 4Gi
Env: LANG=C.UTF-8 TZ=UTC
Timeout: 30m0s
Retries: 0
Artifacts: out/report.pdf
```sh
render report.md -o out/report.pdf
```
