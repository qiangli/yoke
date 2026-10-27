# S515.2 live acceptance, round 2 — 2026-10-27

**FAIL / not ready for acceptance.** The real Windows peer exposed two
incorrect assumptions in the previous spike: Go/Gitea does not resolve the
multi-label `.localhost` name there, and open SeaweedFS rejects writes signed
with unconfigured keys. Anonymous image/LFS round trips work, but downloads
are proxied through loom and its TX counter grows with the payload.

This was a **single-host** investigation: loom, the real outpost router, and
SeaweedFS all ran on the assigned peer. Cross-host proof remains S515.4.
No system hosts/DNS edits, OpenSSH, harness router, installed-outpost changes,
pushes, or branch switches were used. Clones, builds, and Go tests ran on the
peer. Its real user/host access details are intentionally absent here.

## Build and setup

The complete commands are in the [reproduction README](../scripts/s515-live/README.md).
All remote commands began with `export PATH;`, through `outpost ssh`.
Scratch root: `C:/s515/round2-633`. Five shallow public clones were created.

| Source | Resolved main |
|---|---|
| bashy | `3125ac9add24f7b75c2ca57294cf1f1c53798063` |
| yoke | `b7402c75b96cc6dd08838962af96d2120daee4e3` |
| outpost | `a0c7791024b5cf2d03cd106433b60e34955a8a64` |

Bashy and outpost each used explicit `go mod edit -replace` entries for the
fresh yoke and sh clones. Initial native builds both returned 0. The Go gate
was a real compile and execution, not just a version probe:

```text
$ bashy go version
go version go1.27.2 windows/amd64
$ bashy go build -o tiny.exe tiny.go
$ ./tiny.exe
s515 tiny build PASS
```

The newer binary re-provisioned the old Go tree because it lacked the
completion marker. A concurrent version probe exposed a second cache bug:
its promotion partially deleted the first install while `go.exe` was running.
It returned `Access is denied`, and subsequent builds reported missing standard
packages despite the completion marker. Removing that marker and provisioning
serially restored the cache; the tiny compile passed again.

SeaweedFS was the wrapper's pinned `bashy-4.48-2` build. **It was open on
loopback; no S3 identities were configured**, as requested. The wrapper also
starts its internal master/volume/filer services on their default loopback
ports. The scratch outpost used its own HOME, USERPROFILE, config/cache and
APPDATA, an ephemeral admin port, synthetic name `s515self`, mesh disabled,
and local service `s3 -> 127.0.0.1:8333`.

```text
$ bashy curl -i -X PUT http://127.0.0.1:8333/loom
HTTP/1.1 200 OK
Location: /loom
Server: SeaweedFS 30GB 4.48

$ bashy curl -i http://s515self.s515.localhost:31890/loom
HTTP/1.1 200 OK
Server: SeaweedFS 30GB 4.48
<ListBucketResult ...><Name>loom</Name>...</ListBucketResult>
```

That second request passed through the actual new outpost's SELF route. It
proves the router works for a localhost-aware client, not that Go resolves
the hostname.

## Fixes delivered in this branch

* Windows loom startup now provisions bashy's verified MinGit and writes its
  path into Gitea's `[git] PATH`. Before the fix the real loom failed with
  `git not found: exec: "git": executable file not found in %PATH%`.
  Afterward it logged `Git version: 2.55.0` and reached S3 initialization.
  Both foreground and daemon startup use the fix.
* Tree cache misses are serialized with coreutils' cross-process file lock,
  spanning cache recheck, extraction and publication. A waiting installer
  reuses the winner instead of deleting a tree already handed to a caller.
  Complete cache hits retain their existing fast path.
* The stable-name document now records the failed Windows result instead of
  asserting portable Go resolution.

## Required stable-name gate

```text
$ ./bashy-s515-fixed.exe dag live-dag.md live
==> live
FAIL: loom exited: exit status 1; inspect loom.log
==> live FAILED (exit 1)
dag: 1/1 targets failed
```

The exact Gitea startup failure is in
[stable-name-error.txt](s515-live/stable-name-error.txt):

```text
ObjectStorage.BucketExists: endpoint=s515self.s515.localhost:31890,
location=us-east-1, bucket=loom:
Head "http://s515self.s515.localhost:31890/loom/":
dial tcp: lookup s515self.s515.localhost: no such host
```

Gitea's MinIO constructor supplies a custom `http.Transport` with no proxy
function. Its general proxy configuration cannot repair this path. Source
reference: [Gitea MinIO transport](https://github.com/go-gitea/gitea/blob/main/modules/storage/minio.go).
The actual binary's failure, rather than a source inference, is the gate result.

## Diagnostic round trips, explicitly not acceptance

Using `S515_DIAGNOSTIC_ENDPOINT=127.0.0.1:8333` bypassed name resolution.
With test access/secret keys, the first registry blob PUT returned 500:

```text
unable to save object in content store:
Signed request requires setting up SeaweedFS S3 authentication
```

Keeping S3 open and adding `S515_DIAGNOSTIC_ANONYMOUS=1` allowed an actual
OCI Distribution API push/pull (manifest, config, valid tar layer) and LFS
batch API upload/download. The client used a fresh loom fixture and ephemeral
HTTP port each time. Full output, hashes and object keys are in
[anonymous.txt](s515-live/anonymous.txt).

```text
$ S515_DIAGNOSTIC_ENDPOINT=127.0.0.1:8333 S515_DIAGNOSTIC_ANONYMOUS=1 ./bashy-s515-fixed.exe go run acceptance_windows.go
loom ready pid=24316 port=59549
counter_positive_control before=20828 after=21084 delta=256
```

| Download | Status | Payload bytes | Loom TCP TX before | After | Delta |
|---|---:|---:|---:|---:|---:|
| OCI layer | 200, proxied | 8,390,144 | 8,415,943 | 16,806,870 | 8,390,927 |
| OCI config | 200, proxied | 151 | 16,806,870 | 16,807,800 | 930 |
| LFS object | 200, proxied | 4,194,304 | 21,007,286 | 25,202,127 | 4,194,841 |

For each object, a separate GET directly from SeaweedFS matched the complete
payload and SHA-256. Thus the backing store works, but the required redirect
and flat TX conditions do not. The diagnostic ends nonzero:

```text
FAIL: acceptance failures: OCI layer returned 200 with payload through loom;
OCI config returned 200 with payload through loom;
LFS returned 200 with payload through loom
exit status 1
```

The counter is Windows TCP EStats `DataBytesOut`, summed over captured
established IPv4 sockets owned by the measured loom PID. A normal API response
increased it by 256 bytes as a positive control. It is not a host NIC or disk
I/O counter, nor a claim about every historical process socket. For a true
redirect the client would take its baseline after the 30x and check the
subsequent direct transfer; no such successful transfer occurred here.

## Validation and remaining work

Peer gates:

```text
# Disable the new lock in the scratch source, run its regression:
TestConcurrentTreeProvisionDoesNotReplaceWinner:
concurrent tree fetched/published 2 times; want 1
exit 1

# Restore the delivered source:
focused_final_rc=0
ok github.com/qiangli/yoke/external/loom
ok github.com/qiangli/yoke/pkg/binmgr
binmgr_rc=0                 # full binmgr suite
vet_rc=0                   # loom + binmgr
final_build_rc=0            # full native bashy consumer build
```

Focused tests cover managed Git configuration, storage configuration and tree
cache behavior. The full loom suite failed in six tests, reproduced with the
original startup calls restored: `TestArenaPushBaseAndBundleLocalRepo`,
`TestBoothCopyBaseToBareFork`, `TestStopReclaimsTheProxyPort`,
`TestStopReclaimsTheGiteaPort`, `TestStopReclaimsAnUnregisteredGiteaByItsArgv`,
and `TestStartReclaimsAStaleOwnedGiteaByItsArgv`. These are bare-Git-path and
Windows process/port cleanup baseline failures; the full suite is **not green**.

To finish S515.2, the pinned Gitea S3 transport and consuming Go clients need
explicit loopback dialing for the reserved hostname while preserving the
signed URL/Host. The store also needs configured identities for keyed,
presigned downloads, or the intentionally anonymous design needs a supported
direct-download mechanism. No system resolver workaround or unsigned rebuilt
Gitea was substituted for the required product path.

Exact focused validation commands, run from the peer's `bashy` clone:

```sh
../bashy-s515-fixed.exe go test -count=1 -run 'TestManagedGitConfig|TestPeerStoreEndpoint|TestEnsureConfig.*Storage|TestEnsureConfigNoStorage|TestStorageFromEnv|Test.*Tree' github.com/qiangli/yoke/external/loom github.com/qiangli/yoke/pkg/binmgr
../bashy-s515-fixed.exe go test -count=1 github.com/qiangli/yoke/pkg/binmgr
../bashy-s515-fixed.exe go vet github.com/qiangli/yoke/external/loom github.com/qiangli/yoke/pkg/binmgr
../bashy-s515-fixed.exe go build -o ../bashy-s515-final.exe ./cmd/bashy
```

The final built executable was copied to the fixture's candidate filename and
the [required DAG gate](s515-live/acceptance.txt) was rerun: **exit 1**.

Cleanup completed: the scratch `outpost-s515.exe`, `bashy-s515.exe` wrapper,
and its `seaweedfs.exe` child were terminated by PID after verifying their
scratch executable ancestry. Both the S3 and stable-name router probes then
returned nonzero (no listener). The installed outpost remained reachable via
`outpost ssh`. Scratch sources and evidence remain for follow-up.
