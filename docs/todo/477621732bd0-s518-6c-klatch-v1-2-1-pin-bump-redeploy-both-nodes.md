---
id: 477621732bd0
kind: task
title: S518.6c Klatch v1.2.1 pin bump + redeploy both nodes
seq: 2816
status: done
priority: p0
labels:
    - klatch
created: 2026-10-28T08:35:08.457295Z
due: 2026-10-31T00:00:00Z
assignee: codex-gpt6-sol
sprint: 518
sprint_id: bbe9153b-b299-5be8-8ef8-215ad002deb9
sprint_title: 'Klatch MVP: rebranded Databag chat server + web + federation on one satellite (replaces Matrix), due Sat 10-31'
closed: 2026-10-28T08:53:48.05198Z
closed_by: s518-manager
---

S518.6c Klatch v1.2.1: yoke pin bump + redeploy both nodes. Repo: yoke. Signed release https://github.com/qiangli/klatch/releases/tag/bashy-v1.2.1-1 (assets klatch-v1.2.1-{linux,darwin}-{amd64,arm64}.tar.gz, klatch-web-v1.2.1.tar.gz, each with .sigstore.json; identity https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.1-1). 1) Update yoke/pkg/fleet/baseline/tools/klatch.yaml (version, urls, sha256 computed from the downloaded assets, sigstore identity) and any test fixtures; go test ./pkg/fleet/... green. 2) LIVE on the novidesign host (ACCESS via bashy claim list | grep s518; scratch ~/s518-klatch): download + verify (sha256 + cosign verify-blob) darwin-arm64 + web assets, swap the binary/web bundle used by BOTH services klatch (:7100) and klatch2 (:7101) keeping their data dirs, restart both, confirm active/ok. 3) Verify from Dragon through the public satellite URL that the STOCK web login works: use Playwright or a node script that loads https://167-71-135-195.sslip.io/matrix/h/novidesign/app/klatch/ and signs in as alice through the real UI (password from the host vault klatch/alice, passed via env, never printed) — sign-in must succeed WITHOUT any test-side header rewriting; also GET admin/status 200 for both apps. Commit yoke changes with trailers; put the live transcript (status codes, no secrets, use <sat>/<host> in the commit message) in the commit message.

== RULES (all S518 workers) ==
- Your cwd is your isolated workspace (a full git clone). Stay inside it; relative paths only; never follow git remotes. Do not push. Do not switch branches. git add files BY NAME. Keep the bashy-injected BASHY_AGENT and the commit trailers weave gives you.
- Plan of record: umbrella docs/sprint-518-master-execution-plan.md (Klatch = our rebranded fork of Databag, github.com/qiangli/klatch, pinned to upstream main 45343338582b394f4f5fdb4b7e271ea1dc8dd5fd; deadline Sat 2026-10-31 PDT).
- Dragon (this machine) is for editing + short unit tests only: no podman/docker, no full builds of large trees, keep disk use small (delete node_modules you created before finishing).
- Public repos (klatch fork, bashy, yoke): NEVER write real hostnames, IPs, user names or emails into files or commit messages. Hosts come from env/config only.
- Remote Linux access when your story needs it: read `bashy claim list | grep s518` — the host:alien:s518-klatch intent carries the ACCESS line (outpost ssh ...). Use only /opt/s518-klatch on that host, loopback ports 7100/7101 (7000 is taken). Never touch cloudbox, DKS, k3s or the 80/443 front there.
- Credit: Klatch is based on Databag by Roland Osborne (github.com/balzack/databag), Apache-2.0. Keep upstream LICENSE and copyright headers.
- Done = the story's verification passes and is committed. State EXACT measured results in the commit message. If stuck after 3 serious attempts, commit what works plus a BLOCKERS.md with diagnosis, then exit 0. Headless: exit cleanly (exit 0) when done. COMMIT YOUR WORK.
