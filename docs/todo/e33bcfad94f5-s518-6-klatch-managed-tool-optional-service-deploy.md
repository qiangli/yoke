---
id: e33bcfad94f5
kind: feature
title: S518.6 klatch managed tool + optional service, deployed on one dev satellite
seq: 2814
status: done
priority: p0
labels:
    - klatch
created: 2026-10-28T05:42:55.640071Z
due: 2026-10-31T00:00:00Z
assignee: codex-gpt6-sol
sprint: 518
sprint_id: bbe9153b-b299-5be8-8ef8-215ad002deb9
sprint_title: 'Klatch MVP: rebranded Databag chat server + web + federation on one satellite (replaces Matrix), due Sat 10-31'
closed: 2026-10-28T07:39:51.763193Z
closed_by: s518-manager
---

S518.6 klatch managed tool + optional service unit (yoke), and the live MVP deploy on the dev satellite host.

Repo: this workspace is yoke. Deployment model (decided): Klatch runs on the macOS arm64 outpost host novidesign (paired to the alien dev satellite) on 127.0.0.1:7100 and is published by the satellite at https://<satellite>/matrix/h/novidesign/app/klatch/ (outpost app, login not required: chat accounts are Klatch's own). Federation domain = "<satellite-host>/matrix/h/novidesign/app/klatch" (path-bearing, supported by the fork since S518.6a). Host names/IPs: NEVER in yoke files or commit messages; read the ACCESS lines with `bashy claim list | grep s518` (host:novidesign:s518-klatch and host:alien:s518-klatch).
Signed release: github.com/qiangli/klatch release tag bashy-v1.2.0-3 built+signed by .github/workflows/bashy-build.yml (Sigstore keyless; <asset>.sigstore.json beside each asset). Assets: klatch-<ver>-{linux,darwin}-{amd64,arm64}.tar.gz (server binary + transform scripts) and klatch-web-<ver>.tar.gz (static web UI).
1. yoke/pkg/fleet/baseline/tools/klatch.yaml modeled exactly on tuwunel.yaml (kind system, hidden, service: true, sigstore identity = the klatch workflow ref at that tag, per-platform url+sha256 — compute sha256 from the downloaded release assets). Check whether binmgr supports .tar.gz archives (pkg/binmgr); if it needs a raw executable, use what binmgr supports (e.g. an archive member/extract option if one exists) — do not add new binmgr features beyond a minimal, tested extension if strictly required.
2. yoke/pkg/fleet/optional/klatch.yaml modeled on matrix-chat.yaml: loopback bind, settings.binary, settings.data_dir (default ${BASHY_HOME:-$HOME/.bashy}/klatch), settings.web_dir, settings.transform_dir, port 7100; exec `klatch -p 127.0.0.1:<port> -s <data> -w <web> -t <transform>`; no secrets in the file. Add/extend the existing tests that validate optional units and tool pins (find them next to matrix-chat/tuwunel tests) so the new files are covered.
3. LIVE DEPLOY (the acceptance): on novidesign (outpost ssh per ACCESS line; scratch ~/s518-klatch only): download the darwin-arm64 + web assets, verify sha256 AND the sigstore bundle (cosign verify-blob or the binmgr path), unpack, install the unit with `/opt/homebrew/opt/bashy/bin/bashy service add -f <unit file> --set ...` (or the documented equivalent for that bashy version) and start it so it survives restart; then publish `/opt/homebrew/opt/bashy/bin/outpost apps add klatch --scheme http --port 7100` (leave require-login off). Bootstrap with the bashy examples/klatch provisioner (python3 provision.py setup CONFIG against http://127.0.0.1:7100 on the host; admin + test user passwords created with `bashy secret gen | bashy secret set klatch/<name>` ON THAT HOST, never printed) with domain = satellite path above, users alice,bob,carol and group "S518 test" (owner alice).
4. Verify from Dragon through the satellite: GET https://<sat>/matrix/h/novidesign/app/klatch/ -> 200 index.html with Klatch title; admin/status 200; restart the service and re-check.
Verification: yoke unit tests for the new YAML pass (`go test ./pkg/fleet/...` — whole package); live transcript (commands + status codes, no secrets, no hostnames in the commit message — use <sat>/<host>) in the commit message; record the exact host-side commands in a private note at the END of your final message (manager copies it to the umbrella).
Provisioner source: bashy main b3e2cc1f examples/klatch/ (provision.py + README) — fetch it from https://raw.githubusercontent.com/qiangli/bashy/b3e2cc1ffd509aa3e887bc68ee2a572b4a27eda9/examples/klatch/provision.py onto the host. Signed assets: https://github.com/qiangli/klatch/releases/tag/bashy-v1.2.0-3 (asset names klatch-v1.2.0-darwin-arm64.tar.gz, klatch-web-v1.2.0.tar.gz, each with .sigstore.json; signer identity https://github.com/qiangli/klatch/.github/workflows/bashy-build.yml@refs/tags/bashy-v1.2.0-3). The S518 alien-host rules below do not apply: this story uses only the novidesign host (~/s518-klatch). Remove the probe app when done: outpost apps rm s518-probe (or the documented remove verb) and kill the python echo on 7199.

== RULES (all S518 workers) ==
- Your cwd is your isolated workspace (a full git clone). Stay inside it; relative paths only; never follow git remotes. Do not push. Do not switch branches. git add files BY NAME. Keep the bashy-injected BASHY_AGENT and the commit trailers weave gives you.
- Plan of record: umbrella docs/sprint-518-master-execution-plan.md (Klatch = our rebranded fork of Databag, github.com/qiangli/klatch, pinned to upstream main 45343338582b394f4f5fdb4b7e271ea1dc8dd5fd; deadline Sat 2026-10-31 PDT).
- Dragon (this machine) is for editing + short unit tests only: no podman/docker, no full builds of large trees, keep disk use small (delete node_modules you created before finishing).
- Public repos (klatch fork, bashy, yoke): NEVER write real hostnames, IPs, user names or emails into files or commit messages. Hosts come from env/config only.
- Remote Linux access when your story needs it: read `bashy claim list | grep s518` — the host:alien:s518-klatch intent carries the ACCESS line (outpost ssh ...). Use only /opt/s518-klatch on that host, loopback ports 7100/7101 (7000 is taken). Never touch cloudbox, DKS, k3s or the 80/443 front there.
- Credit: Klatch is based on Databag by Roland Osborne (github.com/balzack/databag), Apache-2.0. Keep upstream LICENSE and copyright headers.
- Done = the story's verification passes and is committed. State EXACT measured results in the commit message. If stuck after 3 serious attempts, commit what works plus a BLOCKERS.md with diagnosis, then exit 0. Headless: exit cleanly (exit 0) when done. COMMIT YOUR WORK.
