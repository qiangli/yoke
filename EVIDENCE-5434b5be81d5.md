# S515.2 live acceptance — run #629 (yoke)

Spec: dhnt `docs/sprint-515-satellite-loom.md` §S515.2 + `docs/peer-store-name.md`.
Code under test: yoke `259a9d82` (loom `[storage]` + `SERVE_DIRECT`, `LOOM_S3_*`),
outpost `997b8a3` (`internal/agent/storeroute`, fixed port 31890).

Host: the S515 peer A (Windows/amd64), reached only with `outpost ssh`; the
claim's ACCESS line is not reproduced here. Everything below ran on the peer
with bashy's own tools (`bashy go` 1.27.2, `bashy git`, `bashy seaweedfs`).

## Result

**Live acceptance NOT reached inside the time box.** Two environment-level
defects had to be dug out first; one of them is a real yoke bug and is FIXED in
this commit. What was established:

| step | state |
|---|---|
| clone bashy / yoke / outpost / sh on the peer with `bashy git clone` | done |
| yoke main carries `external/loom/storage.go` | confirmed |
| bashy main's yoke pin does NOT carry it yet | confirmed (`yoke@v0.0.0-20261027145640-03fe40ca29a6` has no `storage.go`) → a `-replace` onto the yoke clone is required to build a bashy that can serve S515.2 |
| outpost main carries `internal/agent/storeroute` (port 31890) | confirmed |
| build bashy + outpost on the peer | BLOCKED by the toolchain defect below, then unblocked by re-provisioning; builds not re-run before the time box closed |
| seaweedfs S3 + loom + store router + image/LFS round-trip + tx counters | NOT RUN |

## Defect 1 (yoke, FIXED here) — a failed tree extraction poisons the binmgr cache

`bashy go build` on the peer failed with:

```
...\go\1.27.2\go\src\net\netip\netip.go:21:2: package unique is not in std
...\go\1.27.2\go\src\database\sql\driver\types.go:12:2: package uuid is not in std
...\go\1.27.2\go\src\...\maphash.go:18:2: package weak is not in std
```

while `bashy go version` happily printed `go1.27.2 windows/amd64`. The
provisioned GOROOT was **truncated mid-alphabet**: `go/src` listed 71 entries
ending at `time`, with `unicode`, `unique`, `unsafe`, `vendor` and `weak`
missing. `go/VERSION` read `go1.27.2`, so this was a genuine 1.27.2 tree whose
extraction had died partway.

Root cause in `pkg/binmgr`: `Asset.Tree` extraction unpacked straight into the
version dir, and `ensure`'s cache check was `isExecutable(dest)` alone. `bin/go`
is written long before `src/`, so a partial extraction leaves an executable
entrypoint over a half-written tree — and every later call takes that corpse for
a cache hit with no network. The cache is poisoned permanently; re-running never
repairs it, only `rm -rf` of the version dir does.

Fix (`pkg/binmgr/binmgr.go`):

- extract a Tree asset into a `.stage-*` dir inside the version dir, require the
  entrypoint there, then promote the top-level entries with per-entry
  `os.Rename` — the entrypoint only becomes visible together with its tree;
- drop a `.binmgr-tree-complete` marker on success and require it on the
  cache-hit path for Tree assets, so a cache already poisoned by the old code
  self-heals (and a pre-marker healthy cache re-installs once — idempotent and
  checksum-verified).

Regression test: `TestEnsure_TreePartialExtractIsNotACacheHit`.

## Defect 2 (outpost, NOT fixed here) — `outpost ssh` does not export PATH

In an `outpost ssh … -- <cmd>` session, `PATH` is set as a shell variable but
not exported, so `echo $PATH` is right while any bashy verb run **inside a
pipeline** dies with `bashy: command not found` (the pipeline element execs a
child that no longer sees it). `export PATH` as the first statement is the
workaround and every command below used it. Belongs to outpost/the ssh server
side, not this repo.

## Defect 3 (to triage) — `/c/...` paths are not translated for verb arguments

`bashy git clone --depth 1 <url> /c/Users/<u>/s515/sh` reported
`Cloned into '/c/Users/<u>/s515/sh'` and exited 0, but the worktree landed in
`C:\c\Users\<u>\s515\sh` — the msys-style `/c/...` prefix was taken as a
relative path under the drive root. bash's own builtins (`cd`, redirection,
`ls`) translate the same string correctly, so the translation is missing on the
path arguments handed to a Go verb. Not reproduced far enough to place it in
yoke's `git/` vs bashy's shell boundary, so it is reported, not fixed.

## Limitation to carry forward (pre-existing, by design of this round)

Even once built, this was only ever going to be a **single-host proof** — loom
and the store live on the same peer, so `storeroute`'s *self* branch is what
gets exercised. Cross-host comes with S515.4.

One more thing the live attempt surfaced, for the outpost side of S515.2:
`serveStoreRoute` is started only under `meshHost != nil && meshResolver != nil`
(`cmd/outpost/main.go`), i.e. it needs a cloudbox access token and a running
mesh — yet the *self* branch it would serve needs neither, only the locally
exposed `s3` service. A host that exposes `s3` locally therefore gets no store
router, and the feature cannot be exercised without the full paired daemon.

Also recorded for whoever picks this up: `external/seaweedfs` has no way to
configure S3 identities (no `-s3.config`), so "create a bucket and keys" is not
expressible today — the gateway runs open on loopback and SigV4 signatures are
not checked. That is fine for a loopback single-host proof and a gap for
anything else.

## Gate for the fix in this commit

```
go vet ./pkg/binmgr/                  → clean
go test ./pkg/binmgr/ -count=1        → ok  github.com/qiangli/yoke/pkg/binmgr  1.191s
GOOS={windows,linux,darwin} go build ./pkg/binmgr/   → clean
GOOS=windows go vet ./pkg/binmgr/     → clean
GOOS=aix GOARCH=ppc64 go vet ./pkg/binmgr/           → clean
scripts/crossvet.sh                   → PASS for every GOOS except aix
```

`crossvet.sh`'s aix leg fails in the vendored forks — `external/go-billy/osfs`
(`undefined: unix.Flock`) and the `mvdan.cc/sh/v3` fork's `interp`
(`unix.F_DUPFD_CLOEXEC`, `syscall.Getpgrp`, `unix.Futimes`, `unix.AT_EACCESS`).
Pre-existing and unrelated: this commit touches only `pkg/binmgr`, which vets
clean under `GOOS=aix` on its own.
