# S515.4 live round 2 — replica reads, primary writes

Cross-host live result for the yoke half of S515.4 (`bashy loom replica` /
`bashy loom clone` / `bashy loom remote refresh`). Round 1 was code + unit
tests; this round is LIVE ONLY, on three real hosts, reusing the build and
scratch-dir pattern of [S515.2 round 3](../scripts/s515-live/README.md).

Hosts are named by ROLE only, per the repo rule. Access strings live in the
manager's private claims (`bashy claim list | grep s515`), never here.

## Topology

| role    | host                            | loom                                  |
|---------|---------------------------------|---------------------------------------|
| primary | the Windows peer                | scratch loom, port 33100              |
| replica | the staging satellite VM (Linux)| scratch loom, port 33200              |
| client  | a macOS member host             | no loom; `bashy loom clone` only      |

All three built `bashy` from current mains with `bashy go`
(`-replace` onto the yoke clone), in fresh scratch dirs, as S515.2 round 3
does. Recorded revisions: bashy `023c5d2c`, yoke `25a6d144`, outpost
`1992b94e`, sh `6663c93a`, coreutils `211789fa`. Reached with `outpost ssh`
throughout; no OpenSSH. Fixture repos: an ORG-owned `s638/demo` and a
USER-owned `admin/userdemo`, both on the primary. Both looms ran with
`--actions=false` and a disposable loopback admin.

Two notes on the hosts, neither a defect in this story's code:

* loom refuses to run as root, so the replica runs as an unprivileged user.
* `loom start` re-execs `os.Args[0]` for its proxy, so it must be invoked by
  ABSOLUTE path; a relative `./bashy-…` fails with
  `start proxy: fork/exec ./bashy-…: no such file or directory`.

## What passed

**`replica add` ensures + mirrors, for both owner kinds.** One command on the
client against the live primary and replica:

```
s638/demo       -> http://<replica>/s638/demo.git: exists (sync requested)
admin/userdemo  -> http://<replica>/admin/userdemo.git: mirrored (sync requested) (replica repo created)
```

Afterwards both replica repos exist and their HEADs equal the primary's
(`12dd2c18…` and `2d009c2e…`), and `replica list` reports
`sync_on_commit=true interval=8h0m0s error=""` for both. Re-running is a
no-op (`exists`).

**Clone and fetch hit the replica — proven by removing the primary.** Rather
than sample a byte counter, the primary's inbound port was closed so it was
provably unreachable from the client (`curl` to the primary: connect timeout;
to the replica: 200). With the primary in that state:

* `bashy loom clone <primary-url> --replica <replica-url>` succeeded, reporting
  `cloned from http://<replica>/s638/demo.git (push -> http://<primary>/…)`,
  and checked out `12dd2c18 Initial commit`;
* `bashy git fetch origin` succeeded.

Zero bytes could have reached the primary, so read traffic is served entirely
by the replica. `.git/config` after the clone:

```
[remote "origin"]
	url = http://<replica>/s638/demo.git
	pushurl = http://<primary>/s638/demo.git
[loom]
	primary = http://<primary>/s638/demo.git
	replica = http://<replica>
```

**Push hits the primary.** With the primary still unreachable, `git push
origin main` failed dialing the PRIMARY
(`…//<primary>/s638/demo.git/info/refs?service=git-receive-pack: i/o
timeout`) — it never fell back to the replica. With the primary reachable
again the same push succeeded and the primary's HEAD advanced to the new
commit `463b89ec…`.

`bashy git push` carries no credentials of its own (`nativePush` sets no
`Auth`), so a token-authenticated primary must be given as a URL with
userinfo; `loom clone` then stores it as `pushurl` and `loom.primary`.

**Failover and failback.** Replica stopped (`loom stop`; the client's health
probe then times out) → `bashy loom remote refresh` printed
`origin fetch url: http://<primary>/s638/demo.git`, `remote.origin.url`
became the primary, `pushurl` was untouched, and `git fetch origin`
succeeded against the primary. Replica restarted → a second `refresh`
re-picked `http://<replica>/s638/demo.git`.

## Bugs found live and FIXED in this story

1. **A user-owned repo could not be replicated, and aborted the whole run.**
   `ensureReplicaRepo` treated every owner as an org: for `admin/userdemo` it
   tried `POST /orgs/admin/repos` (404, not an org) and then `POST /orgs` with
   that name, which Gitea rejects — `422 user already exists [name: admin]`.
   Because `replica add` fails loudly per repo, that aborted every repo after
   it, so on a personal forge the command replicated nothing. `ensureReplicaOwner`
   now branches on the primary's `owner.type`: an org owner is ensured through
   `POST /orgs`, a user owner through `POST /admin/users`, and the repo is
   created with `POST /admin/users/{user}/repos` rather than through an org.

2. **The replica stayed EMPTY after `replica add`.** Creating a push mirror
   does not populate it: Gitea pushes on the next commit (`sync_on_commit`) or
   after the interval — here 8h. The replica repo existed but had no refs, so a
   client cloning right after `replica add` got an empty repo, which defeats
   the point of adding a replica. `ReplicaAdd` now issues
   `POST /repos/{o}/{r}/push_mirrors-sync` per repo, which is also what makes
   a re-add converge rather than merely report `exists`.

Both are covered by new fake-Gitea unit tests (`TestReplicaAddUserOwnedRepo`,
`TestReplicaAddTriggersInitialSync`) and were re-verified live.

## Gap: `sync_on_commit` does not fire on the Windows primary

The spec's "replica shows the new commit within seconds" did NOT hold for a
hook-triggered sync. After the push above, the replica still served the old
HEAD 60s later, with the mirror reporting `last_error=""` and a `last_update`
from the earlier explicit sync.

The mirror is healthy and the fault is not in the API calls this story makes:
an explicit `POST …/push_mirrors-sync` immediately afterwards put the pushed
commit `463b89ec…` on the replica **within 2s**.

What fails is Gitea's post-receive hook. The push itself was served
(`POST /s638/demo.git/git-receive-pack 200`), but Gitea never recorded it —
the repo's activity feed holds only `create_repo`, with no `push` op — and no
mirror work appears in the log at push time. `hooks/post-receive` is present
and is the stock `#!/usr/bin/env bash` script Gitea generates, so it is being
written and not executed on this host. Anything downstream of that hook is
equally dead there: `sync_on_commit`, webhooks, Actions.

This is a Gitea-on-Windows hook-execution problem, not a replica defect, and
fixing it is outside this story. It is load-bearing for the design, though:
the one-way mirror is only "within seconds" if the hook runs. Until it is
fixed on Windows, a primary on that platform needs the explicit sync
(`push_mirrors-sync`) to propagate a push. Picking that up — a diagnosis of
why the hook does not run, and whether loom should sync explicitly after a
push when the hook is unavailable — belongs to a follow-up story.

## Cleanup

Both scratch looms were stopped, the temporary inbound rule on the primary was
removed, and the client's `outpost ssh` alias was deleted. The installed
system services on either host were never touched.
