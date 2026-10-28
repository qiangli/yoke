---
id: c09edf3729e2
kind: task
title: S518.6g Klatch v1.2.3 pin bump + redeploy both nodes
seq: 2818
status: done
priority: p0
labels:
    - klatch
created: 2026-10-28T10:33:59.321442Z
due: 2026-10-31T00:00:00Z
assignee: codex-gpt6-sol
sprint: 518
sprint_id: bbe9153b-b299-5be8-8ef8-215ad002deb9
sprint_title: 'Klatch MVP: rebranded Databag chat server + web + federation on one satellite (replaces Matrix), due Sat 10-31'
closed: 2026-10-28T10:56:51.813269Z
closed_by: s518-manager
---

S518.6g Klatch v1.2.3: yoke pin bump + redeploy both nodes. Repo: yoke (base already has klatch.yaml pinned to v1.2.2 + pkg/fleet/klatch_test.go). Signed release https://github.com/qiangli/klatch/releases/tag/bashy-v1.2.3-1 (assets klatch-v1.2.3-*.tar.gz, klatch-web-v1.2.3.tar.gz + .sigstore.json; identity https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.3-1). 1) Update pkg/fleet/baseline/tools/klatch.yaml (version 1.2.3-bashy.1, urls, sha256 measured from downloads, identity) and pkg/fleet/klatch_test.go expectations; go test ./pkg/fleet/... green. 2) LIVE on novidesign (ACCESS via bashy claim list | grep s518; ~/s518-klatch): download + verify sha256 + cosign verify-blob for darwin-arm64 + web; swap binary + web for BOTH services klatch (:7100) and klatch2 (:7101) keeping data; restart; active/ok; public GET admin/status 200 for both apps. No E2E needed (manager reruns it). Commit with trailers + transcript (no secrets, <sat>/<host>).

== RULES (all S518 workers) ==
- Your cwd is your isolated workspace (a full git clone). Stay inside it; relative paths only; never follow git remotes. Do not push. Do not switch branches. git add files BY NAME. Keep the bashy-injected BASHY_AGENT and the commit trailers weave gives you.
- Plan of record: umbrella docs/sprint-518-master-execution-plan.md (Klatch = our rebranded fork of Databag, github.com/qiangli/klatch, pinned to upstream main 45343338582b394f4f5fdb4b7e271ea1dc8dd5fd; deadline Sat 2026-10-31 PDT).
- Dragon (this machine) is for editing + short unit tests only: no podman/docker, no full builds of large trees, keep disk use small (delete node_modules you created before finishing).
- Public repos (klatch fork, bashy, yoke): NEVER write real hostnames, IPs, user names or emails into files or commit messages. Hosts come from env/config only.
- Remote Linux access when your story needs it: read `bashy claim list | grep s518` — the host:alien:s518-klatch intent carries the ACCESS line (outpost ssh ...). Use only /opt/s518-klatch on that host, loopback ports 7100/7101 (7000 is taken). Never touch cloudbox, DKS, k3s or the 80/443 front there.
- Credit: Klatch is based on Databag by Roland Osborne (github.com/balzack/databag), Apache-2.0. Keep upstream LICENSE and copyright headers.
- Done = the story's verification passes and is committed. State EXACT measured results in the commit message. If stuck after 3 serious attempts, commit what works plus a BLOCKERS.md with diagnosis, then exit 0. Headless: exit cleanly (exit 0) when done. COMMIT YOUR WORK.
