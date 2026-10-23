---
id: 526a23c5d741
kind: feature
title: 'M2 (yoke) local-PR engine in pkg/src: branch/status/diff/log, pr open|show|merge|close into the host-local branch, patch export|apply, upgrade rebase with conflict report, provenance state'
seq: 2034
status: done
priority: p0
created: 2026-10-23T16:41:19.736725Z
weave: 615
assignee: muse-conductor
sprint: 458
sprint_id: 3b16e60f-dce3-5dd1-8ed1-4384110d0741
sprint_title: 'bashy is all you need, MVP: invariant core + every other command as editable Bash# skill/script'
closed: 2026-10-23T16:52:01.080161Z
resolution: fixed
closed_by: muse-conductor-q
---

M2 (yoke half). Spec: umbrella docs/bashy-invariant-core.md sections 2, 3 and 6, plus the Sprint 458 card. Builds on M1 pkg/src (yoke bdede1a9): $BASHY_HOME/src/<repo> checkouts with branch `pristine`, ClassOf/PathClass, ProtectedManifest, Check. Repo: yoke ONLY; the bashy surfaces (doctor, fleet registry, dispatcher, mount) are M2b. Public repo: no hostnames, user ids, cloudbox internals or docs/sprint-* files. Use yoke's pure-Go git only; never shell out to git.

Deliver in pkg/src, exposed as subcommands on the existing src rod (no new verb):
1. `src branch <repo> <name>`: create a topic branch off `local`, the host's own branch. `local` is created from `pristine` on first use. Also `src status`, `src diff <repo> [--base pristine|local]` and `src log`.
2. Local PRs. `src pr open <repo> <branch> --title T [--test CMD]` writes a JSON record (id, title, author agent from BASHY_AGENT, base=local, head, head SHA, test cmd/result, state) under $BASHY_HOME/src/.prs/. `pr show|list` read the records. `pr merge <id>` merges the head into `local` only, never pristine and never any remote; it refuses if a recorded test failed, or if Check() reports a core/protected path in the PR diff unless an approval record exists (see 4). `pr close <id>` closes without merging.
3. Patches: `src patch export <id> > file` and `src patch apply <repo> <file>` (opens a PR from the patch) move changes between hosts by file. `src sync` is not required for the MVP; omit it.
4. Protected approval: ApprovalRecord{repo, paths, approver, reason, time} at $BASHY_HOME/src/.approvals/. Expose a function for bashy's approve verb to write it; pkg/src only reads it. A merge touching protected paths with an approval sets the state `modified-protected`. A core path is always refused.
5. Provenance: State(repo) returns pristine | modified | modified-protected, plus base release commit and local HEAD, for bashy doctor and the fleet registry (M2b).
6. Upgrade: Upgrade(repo, newCommit) fetches the new release commit, moves `pristine` to it and rebases `local` onto it. On conflict it aborts, leaves `local` and its files exactly as they were, and returns the conflicting paths. Never clobber.

Acceptance, red/green with local fixture repos (no network): branch + pr open + merge lands on local only; pristine unchanged; a merge with a failing recorded test is refused; a protected-path PR is refused without approval, and merges with one as modified-protected; a core-path PR is always refused; patch export/apply round-trips; Upgrade rebases cleanly and, on conflict, reports paths with local untouched; State values. go test ./pkg/src + go build ./... + go vet.
