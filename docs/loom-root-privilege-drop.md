# loom drops privileges itself (S515.1 companion, Sprint #515)

## The fault, measured

Found live 2026-10-27 on a fresh v0.18.41 product satellite (evidence:
cloudbox `docs/s515-1-live-round4.md`). The satellite-local outpost runs **as
root** — it drives podman, systemd and DKS — and its service monitor now
starts the enabled loom unit. loom then exits:

```
loom: refusing to run as root (Gitea forbids it); run bashy as a regular user
```

So loom never served on **any** product satellite. The refusal was correct
about Gitea (upstream's `mustNotRunAsRoot`) and wrong about the deployment:
there is no regular user to run bashy as on a satellite.

## The decision (manager, 2026-10-27)

loom drops the privilege **itself** when `euid == 0` on Linux. Same behaviour
for a root satellite outpost and a user-run home outpost; no outpost spec
change, no user-data wrapper, no systemd `User=` to get right. The rule is
unchanged: **bashy is all you need.**

Gitea's own guard is satisfied honestly — loom does **not** set Gitea's
undocumented run-as-root override.

## What loom does

Implementation: `external/loom/privdrop.go` (+ `privdrop_unix.go` /
`privdrop_windows.go`). Entered from `Start`, `Serve` and `StartDaemon` after
the ports are proven free and the gitea binary is in hand, so a start that is
going to be refused anyway never creates an OS account.

1. **Decide** — `shouldDropPrivileges(goos, uid)`: Linux **and** uid 0, nothing
   else. A pure function, so every platform's answer is testable from one host.
2. **Provision the account** — a dedicated unprivileged system account named
   `loom` (`$LOOM_SYSTEM_USER` overrides). Nothing in yoke or coreutils creates
   OS accounts, so this is the minimal correct mechanism: the distro's own tool,
   in the two spellings a Linux host actually ships —

   | tooling | group | user |
   | --- | --- | --- |
   | shadow-utils | `groupadd --system loom` | `useradd --system --gid loom --no-create-home --home-dir <dataDir> --shell <nologin> loom` |
   | busybox | `addgroup -S loom` | `adduser -S -D -H -h <dataDir> -s <nologin> -G loom loom` |

   A same-named group is created first so the gid is ours, not whatever the
   distro's `useradd` defaults picked. No login shell (`/usr/sbin/nologin`,
   `/sbin/nologin`, `/usr/bin/nologin`, else `/bin/false`), no home creation —
   the account's home **is** the loom data dir. Both tools exit non-zero when
   the name is already taken, so loom re-looks-up rather than trusting the exit
   code: an account an operator pre-created, or an earlier start left behind, is
   reused as-is. A host with neither tool fails loudly with the command to run,
   it does not fall back to uid 0. An account that resolves to uid 0 or gid 0 is
   refused — dropping to root drops nothing.
3. **Hand over the data dir** — `privDrop.own` chowns the tree (skipping any
   entry already correct, so a warm data dir costs one `lstat` per entry and no
   write) and makes every ancestor traversable. Run **after** the config
   writers, because `app.ini` and `custom/` are written by the root parent.
4. **Start Gitea under the credential** — `SysProcAttr.Credential{Uid,Gid}`, so
   the child never holds root even momentarily. `HOME`/`USER`/`LOGNAME` and
   `app.ini`'s `RUN_USER` all name the same account: Gitea resolves "the current
   user" from `$USER` before `os/user` and exits when `RUN_USER` disagrees with
   the uid it finds itself with.
5. **The `gitea admin` CLI drops too** (`runGiteaAdmin`). Those calls write the
   same SQLite DB; run as root they would trip Gitea's guard and leave
   root-owned files in a tree the server can only touch as the loom account.

The front **proxy stays in the parent process** — it only binds a loopback port
and forwards, and keeping it there is what the decision allows. Since S515.1
round 5 that is literal for `loom serve` too: it serves the proxy itself
(`serveProxy`) instead of leaving it to `loom start`'s detached child, because
`loom serve` is the argv the service unit supervises and a unit supervises ONE
foreground command. Gitea's own listener is pinned to loopback
(`giteaBindAddr`) whatever `--addr` says: `ENABLE_REVERSE_PROXY_AUTHENTICATION`
is always on, so there an `X-WEBAUTH-USER` header IS a login, and only the
proxy — which strips a client's own `X-WEBAUTH-*` — may face anything else.

### The two traversal problems

Neither is optional on a satellite, and both would have been the next live
failure after the guard:

- The data dir defaults to `~/.agents/bashy/loom`, which under a root outpost is
  `/root/.agents/bashy/loom` — on the satellite's **persistent volume**, which
  is why it must stay there. `/root` is `0700`, so the loom account cannot even
  *traverse* to the dir it owns.
- The gitea binary lives in the root parent's binmgr cache
  (`~/.cache/bashy/bin`), with the same problem.

`ensureTraversable` adds the others-**search** bit to each ancestor and stops at
the first one that already has it. Only `+x` is ever added — never `+r`, never
`+w`: `/root` goes `0700` → `0701`, so the loom account can walk *through* root's
home to its own data dir and still cannot list or read anything else in it.
`allowExec` additionally makes the gitea binary others-read+execute; ownership of
the cache is left alone, since it belongs to whoever manages the binaries.

## Idempotence

Every step converges rather than assuming a first run — restarts and upgrades
are the normal case, and the fault left real debris behind:

- An existing account (pre-created, or from an earlier start) is reused.
- A data dir left **root-owned** by a failed root start is chowned forward; one
  already owned by the account costs no writes.
- `RUN_USER` is reconciled in **both directions**. Dropping stamps
  `RUN_USER = loom`; running as the invoking user **removes** the key so Gitea
  falls back to its own default. That removal is what makes a data dir a root
  start left behind usable again by a plain user run — without it Gitea would
  exit on the mismatch.

## Scope

| platform | behaviour |
| --- | --- |
| Linux, root | drops to the `loom` account (this document) |
| Linux, non-root | **unchanged** — Gitea runs as the invoking user, no account, no chown, no `RUN_USER` key |
| macOS, root | **unchanged** — still `ErrRefuseRoot`. No satellite there, no `useradd`; inventing a mechanism for a case nobody hits is how you get an untested one |
| macOS, non-root | unchanged |
| Windows | unchanged — no euid, and loom never refused there |

## Red/green

- `go test ./external/loom` — the decision for every platform
  (`TestShouldDropPrivileges`), command construction on both start paths
  (credential set when root, absent when not; `HOME`/`USER`/`LOGNAME` and
  `GITEA_WORK_DIR` present; the merge with `setDetached` in either order),
  `RUN_USER` convergence in both directions, traversal (`+x` only) and
  ownership. The guard itself:
  `TestStartDaemonAsRootOnLinuxGetsPastTheGuard` /
  `TestServeAsRootOnLinuxGetsPastTheGuard`.
- The account provisioner is faked **package-wide** by `TestMain`: a unit test
  must never add an account to the host's `/etc/passwd`, and the suite does run
  as root in some CI images.
- `TestRootIntegrationProvisionsAccountAndOwnsDataDir` is the real path (real
  `useradd`, real chown, run twice to prove convergence). It needs root on
  Linux and is opt-in, so it runs nowhere by default — and not on the dev host
  this story was built on, which has no podman:

  ```bash
  podman run --rm -v $PWD:/src -w /src golang:1.27 \
    env LOOM_ROOT_INTEGRATION=1 go test ./external/loom -run RootIntegration -v
  ```

- `scripts/crossvet.sh` for the cross-OS gate.
- **Live** (manager arranges after release): the satellite-local outpost's loom
  serves.

## The one new seam

`binmgr.RunSpec.Configure func(*exec.Cmd)` — called with the assembled command
just before it starts. It is the only way a caller can set per-OS process
attributes that `pkg/binmgr` must not know about, and loom is its first user.
`setDetached` was changed to **merge** into `SysProcAttr` rather than assign, so
the detach attribute and the credential cannot erase each other.
