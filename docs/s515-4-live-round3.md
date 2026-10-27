# S515.4 live round 3 — why Gitea's post-receive hook never ran on Windows

Round 2 ([docs/s515-4-live-round2.md](s515-4-live-round2.md)) passed every
replica acceptance except the one the design hangs on: after a push to the
Windows primary the replica did NOT update, the mirror reported
`last_error=""`, and an explicit `push_mirrors-sync` landed the same commit in
2s. The diagnosis was deferred: *"`hooks/post-receive` is present and is the
stock `#!/usr/bin/env bash` script Gitea generates, so it is being written and
not executed on this host."*

This round finds why, fixes it, and proves the fix live on the same Windows
peer. Hosts are named by ROLE only; access strings live in the manager's
private claims (`bashy claim list | grep s515`), never here.

## The cause: MinGit ships bash, but not under the name `bash`

Gitea writes a two-level hook into every repository (verified verbatim on the
peer, in round 2's own `loom-primary` data dir):

```
hooks/post-receive                # delegating script  — #!/usr/bin/env bash
hooks/post-receive.d/gitea        # calls loom.exe hook — #!/usr/bin/env bash
```

Windows has no kernel shebang. Git for Windows emulates one: it reads the
first line, takes the **basename of the interpreter — `env`; the `bash`
argument is dropped** — looks *that* up on PATH, and spawns it with the script
as `argv[1]`. msys `env.exe` then execs the script, the msys runtime honours
the same shebang, and `env` finally looks up **`bash`** on PATH.

The MinGit that bashy provisions and sha256-verifies
(`external/gitscm`, `MinGit-2.55.0.2-64-bit.zip`,
`e3ea2944cea4b3fabcd69c7c1669ef69b1b66c05ac7806d81224d0abad2dec31`) contains
`usr/bin/env.exe` and `usr/bin/sh.exe` — and that `sh.exe` **is** GNU bash
(`@(#)Bash version 5.3.15(1) release`, `etc/package-versions.txt`: `bash
5.3.015-1`, 2,456,832 bytes) — but the archive contains **no `bash.exe`
anywhere**: 364 files, checked entry by entry against the pinned artifact.

So `env bash` found nothing, git never ran the hook, and since a push is
served *before* its hook runs, the push SUCCEEDED while everything downstream
of the hook stayed dead: the push mirror never synced, the activity feed
recorded no push. Webhooks and Actions are equally dead on such a host.

### Measured on the peer, with the real git

Four variants over (`bash.exe` present?) × (MinGit `usr/bin` on the PATH git
inherits?), each a real `git push` into a real bare repo whose `post-receive`
is `#!/usr/bin/env bash` and writes a file:

| variant | `bash.exe` | PATH given to git | result |
|---|---|---|---|
| a | absent | `C:/Windows/System32;C:/Windows` | `remote: /usr/bin/env: 'bash': No such file or directory` — **hook did not run** |
| b | present | `C:/Windows/System32;C:/Windows` | **hook ran**, `interpreter=/usr/bin/bash` |
| c | absent | + MinGit `usr/bin`, `mingw64/bin` | same error — **hook did not run** |
| d | present | + MinGit `usr/bin`, `mingw64/bin` | **hook ran**, `interpreter=/usr/bin/bash` |

Two conclusions, both load-bearing:

* the missing **name** is the entire fault — a → b flips it with nothing else
  changed;
* **PATH is not the fault.** Even with `PATH=C:/Windows/System32;C:/Windows`,
  the hook's own `$PATH` came out as
  `/mingw64/libexec/git-core:…:/mingw64/bin:/usr/bin:…` — the `cmd/git.exe`
  wrapper loom points Gitea at (`[git] PATH`) prepends the tree's own
  directories itself. loom therefore does **not** touch Gitea's PATH; an
  earlier draft of this fix did, and the measurement removed it.

## The fix

`external/loom/hooks.go`: on Windows, `loom start` ensures the one name MinGit
omits inside the managed tree — `usr/bin/bash.exe`, hardlinked to
`usr/bin/sh.exe` (copy fallback), idempotent and safe against a concurrent
start. An unexpected tree layout or a missing shell now fails `loom start`
loudly rather than leaving hooks silently unrunnable.

The name goes *next to* `sh.exe` rather than into a shim directory of loom's
own, because a msys program has to stay in its installation tree: that is
where it finds `msys-2.0.dll` and the mount table that makes `/usr/bin/env`
resolve for the **nested** `post-receive.d/*` script, which carries the same
shebang. For the same reason it is MinGit's bash rather than bashy's own
shell — bashy runs these scripts fine, but on Windows it resolves an absolute
shebang interpreter through its virtual POSIX root (`BASHY_ROOT`), not through
the PATH git hands it. Either way nothing is installed on the host: this bash
is the one bashy already downloads and verifies.

Red/green unit test (`TestHookShellRunsGiteaStyleHook`): a Gitea-shaped
post-receive pair runs through the tree's own bash — proven by `$BASH` — only
after `ensureHookShell`. It is skipped on Windows, where the shebang is
resolved by git and msys rather than by the kernel or Go's `os/exec`; that leg
is the live A/B below.

## Live A/B on the Windows peer

One host, two scratch looms (primary `:33300`, replica `:33400`, both
`--actions=false`, disposable loopback admin), one fixture repo `admin/alpha`,
one sync-on-commit push mirror primary → replica, and the ONLY thing that
changes between pushes is whether the managed git tree can serve the hook's
interpreter:

* `bashy-s638.exe` — bashy main (`023c5d2c`) + yoke main (`797da431`), no fix;
* `bashy-s639.exe` — the same mains plus this story's commit.

Built on the peer with `bashy go` (bashy's go.mod replaced onto the yoke
clone), reached with `outpost ssh` throughout; no OpenSSH. Each phase stops
the primary and restarts it on the other binary against the SAME data dir, so
the repo, the mirror and the tokens are identical across the A/B.

### Provenance of the `bash.exe` that was already on the host

When this round started, the peer's managed MinGit tree already contained
`usr/bin/bash.exe`, byte-identical to `sh.exe`, with the same mtime and the
same NTFS creation time as the extraction. That is NOT evidence it was there
during round 2: a hardlink shares the source file's inode and therefore its
timestamps, and the fix produces exactly that signature — `loom start` below
recreates a `bash.exe` stamped `11:38`, the extraction time, minutes after the
file was deleted. Its origin is unknown; the pinned archive does not contain
it, nothing in yoke/bashy/outpost created it before this story, and the peer
carries other agents' scratch work. It was deleted before the first push, so
the no-fix pushes ran against the pristine tree (`MinGit shells before:
env.exe sh.exe`).

### Result

Six pushes went to the same fixture repo `admin/alpha` over the run, three
with a hook-capable tree and three without. Each line is the state after that
push; nothing ever called `push_mirrors-sync` after the mirror was created
(`replica add` does one explicit sync by design, before push #4 — and it only
got that far once the fixture's app.ini carried `[migrations]
ALLOW_LOCALNETWORKS = true`; see below):

| # | file | `bash.exe` | internal hook calls in `loom.log` | `activities/feeds` | replica `master` |
|---|---|---|---|---|---|
| 1 | a.txt | absent | 0 | `create_repo` | (repo not mirrored yet) |
| 2 | b.txt | absent | 0 | `create_repo` | unchanged |
| 3 | c.txt | **present** | **1** | `commit_repo,create_repo` | — |
| 4 | x.txt | absent | 1 (no new call) | unchanged | unchanged (`19c51b69`) |
| 5 | y.txt | **present** | **2** | `commit_repo,commit_repo,create_repo` | **advanced to `3737bce0`** |
| 6 | z.txt | **present** | **3** | three `commit_repo` | **`27bd7172`, ~2s after the push** |

* **Without the fix the hook never runs.** `POST
  /api/internal/hook/post-receive/admin/alpha` — the call Gitea's own hook
  script makes — appears in the primary's log exactly three times, once per
  push with a hook-capable tree, and never for the three without. The pushes
  themselves all succeeded (`Pushed to origin`), which is why round 2 saw a
  healthy mirror and a stale replica.
* **Push activity is recorded only with the fix**: `create_repo` alone before,
  one `commit_repo` per hooked push after — the exact symptom round 2
  reported.
* **The replica updates within seconds, with no manual sync.** Measured at git
  level on both bare repos (`git rev-parse master`, independent of Gitea's
  API): primary `27bd7172…` after the push, replica equal **~2 s** later, with
  the mirror reporting `sync_on_commit=true interval=8h0m0s error=""`. Push #4
  on the same mirror, with `bash.exe` removed again, left the replica on its
  previous commit for the full 30 s window.

One more symptom of the same cause, worth knowing: on a primary whose hook
never ran, Gitea's own API has no idea what the repo contains —
`/repos/admin/alpha/commits` returns nothing and `/branches/{name}` panics
(nil dereference), because the branch table is populated by the post-receive
hook. The git data is fine; only the DB is empty. Both recovered once hooked
pushes landed.


## Also recorded, not fixed

**`replica add` cannot mirror to a replica on the local network.** Gitea
refuses a push mirror whose remote address is a local-network address unless
`[migrations] ALLOW_LOCALNETWORKS = true`: `POST
/repos/{owner}/{repo}/push_mirrors` answers
`401 {"message":"Permission denied"}` — `HandleRemoteAddressError` maps
`ErrInvalidCloneAddr{IsPermissionDenied}` to 401, which reads like an auth
failure and is not one (basic auth and an `all`-scoped token fail
identically). `bashy loom replica add` surfaces it as
`admin/alpha: create mirror: POST /repos/admin/alpha/push_mirrors: 401
Unauthorized: Permission denied`. Round 2 never hit it because its replica was
a public-IP VM; every mesh-local replica — the selvedge case — is a private
address, so loom's generated app.ini probably needs that key. It loosens an
SSRF guard on a single-user local forge, which is a decision for the owning
story rather than a drive-by here; this round set it in the fixture's app.ini
only, and says so where it is used.

**Token-in-URL for `pushurl`.** `bashy git push` carries no credential path of
its own (`nativePush` sets no `Auth`), so a token-authenticated primary has to
be given as a URL with userinfo — `loom clone` then stores that token inside
`remote.origin.pushurl` and `loom.primary` in plain `.git/config`, where every
later `git push` and anything that reads the config can see it. Round 2 hit
the same thing. A credential path for `bashy git` (helper, netrc, or a loom
token resolver) is the real fix and belongs to its own story.

**Pre-existing `external/loom` test failures on Windows.** `go test
./external/loom` on the peer fails the same six tests before and after this
change — `TestArenaPushBaseAndBundleLocalRepo`, the four
`Test*Reclaims*` port tests (`loom.go` reclaims ports with `lsof`, which no
Windows host has), and `TestCloneAndRefreshFailover` ("repository not found",
also reproduced on the unpatched tree at `25a6d144`). Not introduced here and
not in `test/known-failures.txt`; they are a Windows gap in loom's own suite.

**The aix canary in `scripts/crossvet.sh`** fails on the base tree too
(`go-billy/osfs` + the sh fork use `unix.Flock`, `syscall.Getpgid`). The three
shipping targets — windows, linux, darwin — pass.

## Gate

* `go test ./external/loom` — PASS on darwin (host) and on the Windows peer
  modulo the six pre-existing failures above.
* `GOOS=windows go vet ./external/loom` — PASS; `scripts/crossvet.sh` passes
  windows/linux/darwin.

## Cleanup

Both scratch looms were stopped (`loom stop`, both data dirs), the scratch
build, data dirs and fixture repos under `C:/s515/r639` are disposable, and no
installed service on the peer was touched.
The managed MinGit tree keeps `usr/bin/bash.exe` — that is the fix's artifact,
and `loom start` recreates it anyway.
