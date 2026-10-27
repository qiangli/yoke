# S515.4 live round 4 — mirroring to a private-address replica, with no hand-edited app.ini

Round 3 ([docs/s515-4-live-round3.md](s515-4-live-round3.md)) fixed the Windows
hook fault and proved sync-on-commit end to end, but it had to set
`[migrations] ALLOW_LOCALNETWORKS = true` in the fixture's `app.ini` **by hand**
and recorded the gap as not fixed: Gitea refuses a push mirror to a private or
loopback address, and every mesh-local replica — the selvedge case — is one.

This round closes it from inside loom. Hosts are named by ROLE only; access
strings live in the manager's private claims (`bashy claim list | grep s515`),
never here.

## The gap, exactly

`POST /repos/{owner}/{repo}/push_mirrors` answers `401 {"message":"Permission
denied"}` for a private-address remote. It is **not** an auth failure:
`HandleRemoteAddressError` maps `ErrInvalidCloneAddr{IsPermissionDenied}` to
401, and a correct `all`-scoped token fails identically — which is exactly how
an operator loses an hour re-minting tokens.

The pinned fork's `services/migrations.Init` (`qiangli/gitea`,
`bashy-v28.1.0-4`) is what has to be satisfied:

```go
allowList = hostmatcher.ParseSimpleMatchList("migrations.ALLOWED_DOMAINS/ALLOW_LOCALNETWORKS", setting.Migrations.AllowedDomains)
if allowList.IsEmpty() {
	// the default policy is that migration module can access external hosts
	allowList.AppendBuiltin(hostmatcher.MatchBuiltinExternal)
}
if setting.Migrations.AllowLocalNetworks {
	allowList.AppendBuiltin(hostmatcher.MatchBuiltinPrivate)
	allowList.AppendBuiltin(hostmatcher.MatchBuiltinLoopback)
}
```

Two consequences drive the design:

* `ALLOW_LOCALNETWORKS = true` is the only key that admits a private replica —
  it appends the private + loopback allow-rules.
* **`ALLOWED_DOMAINS` is not optional once you care about GitHub.** The
  `MatchBuiltinExternal` rule — "external hosts are allowed" — is appended
  *only while `ALLOWED_DOMAINS` is empty*. The moment loom names a replica
  host, every other external host stops being allowed, github.com included. So
  the GitHub pair has to ride along for S515.5's GitHub push mirror to keep
  working. This is measured upstream behaviour, not caution.

## The fix

`external/loom/mirror.go`: when `LOOM_MIRROR_ALLOWED_DOMAINS` is set, `loom
start` writes

```ini
[migrations]
ALLOW_LOCALNETWORKS = true
ALLOWED_DOMAINS = <replica patterns>,github.com,*.github.com
```

into the generated `app.ini`, and **converges an existing one** through
`ensureSectionKeys` — the same minimal INI surgery `[server]`/`[actions]`
already use, so an initialized data dir picks the keys up on its next start
instead of needing surgery.

Why an env opt-in rather than a default: opening local networks wholesale is an
SSRF hole on a hosted multi-owner satellite, whose own services listen on
loopback. So the opt-in is per-host and explicit, and the domain list bounds
what the loosened guard can reach. **When the env is unset loom writes
nothing** and Gitea's defaults stand — including for a loom that opted in
earlier and then unset it: `ensureConfig` converges `app.ini` forward, it does
not police keys the owner may have set by hand.

Ports are stripped from the patterns. Gitea matches this list against the
remote address's *hostname* (`IsMigrateURLAllowed` splits the port off first),
so `replica.local:33400` could never match; accepting `host[:port]` and
normalizing it keeps the env in the shape an operator reads off a replica URL.

`bashy loom replica add` now names the knob on any 401 from that call — a bad
token lands there too, so the first line of the hint says which to rule out
first.

## Live A/B on the Windows peer

One host, two scratch looms (primary `:33310`, replica `:33410`, both
`--actions=false`), one fixture repo `admin/demo`, one binary
(`bashy-s640.exe` = bashy main `ed16b12a` + yoke main `5e7edcf1` + this
story), and the ONLY thing that changes between the two phases is whether
`LOOM_MIRROR_ALLOWED_DOMAINS` is set on the primary. Both phases use the SAME
primary data dir, so phase 2 exercises the converge path, not the seed path.
No `app.ini` was edited by hand at any point.

| phase | `LOOM_MIRROR_ALLOWED_DOMAINS` | primary `app.ini` `[migrations]` | `replica add` | mirrors on primary |
|---|---|---|---|---|
| 1 | unset | **absent** | `401 ... Permission denied` + the hint | 0 |
| 2 | `127.0.0.1:33410` | `ALLOW_LOCALNETWORKS = true`, `ALLOWED_DOMAINS = 127.0.0.1,github.com,*.github.com` | `mirrored (sync requested)` | 1, `sync_on_commit=true error=""` |

Then, with no manual sync anywhere: push #2 to the primary →
`primary=23335315…`, and the replica's `master` reached the same commit
**~2 s later** (polled every 2 s through the replica's own API).

The replica's `app.ini` keeps **no** `[migrations]` section — the key belongs to
the side that *initiates* the mirror, and a replica that mirrors nowhere is not
asked to loosen anything.

What phase 1 printed, verbatim (this is the whole point of the round-4 hint):

```
Error: admin/demo: create mirror: POST /repos/admin/demo/push_mirrors: 401 Unauthorized: {"message":"Permission denied", ...}

hint: if the primary's token is good, this 401 is not about credentials.
Gitea refuses a push mirror to a private or loopback address unless its
app.ini carries [migrations] ALLOW_LOCALNETWORKS = true, and reports that
refusal as 401 "Permission denied". Every mesh-local replica is such an
address. Set LOOM_MIRROR_ALLOWED_DOMAINS on the PRIMARY loom and restart it
(bashy loom stop && bashy loom start) — loom writes the key, and the
allow-list that bounds it, on start:

    LOOM_MIRROR_ALLOWED_DOMAINS=127.0.0.1:33410
```

### A bug the live run found: the hint was buried in cobra's usage block

The first live run printed the 401 and then cobra's full `Usage:` block for
`loom replica add`, which scrolled the explanation off the screen — the hint
existed and the operator still could not see it. A forge refusing a request is
a runtime failure, not a mistyped command, so `replica` subcommands now set
`SilenceUsage`. Fixed inside this round and re-measured: the output above is
from the rebuilt binary. `TestReplicaAddCmd_HintIsNotBuriedInUsage` pins it.

## Gate

* `go test ./external/loom` — PASS on darwin (host). New tests
  (`external/loom/mirror_test.go`) were proven red before the fix and green
  after: `TestEnsureConfig_SeedsMirrorAllowList`,
  `TestEnsureConfig_ConvergesMirrorAllowList`, `TestReplicaAdd_LocalNetworkHint`
  all fail on the neutered tree.
* `GOOS=windows go vet ./external/loom` — PASS; `go vet` also clean for the
  host and linux.
* The six pre-existing Windows failures in this package recorded by round 3
  (`lsof` port reclaim, `TestArenaPushBaseAndBundleLocalRepo`,
  `TestCloneAndRefreshFailover`) are untouched by this change.

## Cleanup

Both scratch looms were stopped (`loom stop`, both data dirs). The scratch
build, clones, data dirs and the fixture repo under `C:/s515/r640` are
disposable; no installed service and no other round's directory on the peer was
touched.
