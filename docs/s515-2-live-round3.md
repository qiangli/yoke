# S515.2 live acceptance, round 3 — 2026-10-27

**Strict requested acceptance: FAIL.** The decided address model works:
keyed OCI downloads return HTTP 307 to
`127.0.0.1:31890/s3-s515self/...`, and both OCI and standard LFS batch downloads
come from SeaweedFS with zero measured loom TX growth. However, the pinned
Gitea LFS object endpoint returns HTTP 200 and proxies the bytes. Its normal
batch API supplies a presigned storage URL directly, not an HTTP redirect.
The gate preserves the requested LFS 30x assertion and exits 1.

This is single-host evidence only. Cross-host routing remains S515.4.
No actual host/user access names, production credentials, DNS/hosts changes,
OpenSSH, replacement router, installed-outpost changes, or pushes are involved.

## Builds and setup

All clones and consumer builds ran on the claimed Windows peer, using
`outpost ssh` with `export PATH;` and `bashy git` / `bashy go`.
Scratch root: `C:/s515/round3-635`. See the complete
[reproduction instructions](../scripts/s515-live/README.md).

| Source | Current main resolved at clone time |
|---|---|
| bashy | `023c5d2cce89f6645e4baf921e755b33db4752c3` |
| yoke | `da4f7ceb13860b3b4a37f806c7c9422dbec69280` |
| outpost | `1992b94e9ecb5bb02b5ff33e0892fecdaae54d41` |
| sh | `6663c93a2d3cd3528a8d270080cfb10ead828135` |
| coreutils | `211789fadf75a29b73772b8ab2923ac03b6935ec` |

Bashy and outpost used explicit yoke/sh replacements to the fresh sibling
clones. Both native builds passed, using Go 1.27.2 windows/amd64. No product
source overlay was needed: the address/identity changes were already merged.
The managed binaries were Gitea `bashy-v28.1.0-4` and SeaweedFS `bashy-4.48-2`.

The new [stack helper](../scripts/s515-live/stack_windows.go) runs the real
SeaweedFS wrapper and outpost in scratch directories, checks that their ports
are free first, and cleans up their processes afterward. Outpost has synthetic
peer ID `s515self`, mesh disabled, and local service `s3 -> 127.0.0.1:8333`.
SeaweedFS receives the disposable fixture keys from `SEAWEEDFS_S3_*`; its
written `weed/s3-config.json` was inspected:

```text
identity_count: 1
name: admin
actions: Admin, Read, Write, List, Tagging
```

Loom gets the matching `LOOM_S3_*` keys and `LOOM_S3_PEER=s515self`, with no
endpoint or bucket override. It creates the bucket through the real router.
Unsigned PUTs to both the direct store and routed bucket return **403**.
The signed OCI and LFS uploads succeed.

## Observed bytes and counters

Full final DAG output: [acceptance.txt](s515-live/round3/acceptance.txt).
Measured loom PID: 26464. The TCP counter positive control increased from
23,603 to 24,180 (+577), proving the measurement was active.

| Download | Behavior | Payload bytes | Loom TX before | After | Delta |
|---|---|---:|---:|---:|---:|
| OCI manifest | 307 to bucket route | 400 | 8,432,918 | 8,432,918 | 0 |
| OCI layer | 307 to bucket route | 8,390,144 | 8,434,371 | 8,434,371 | 0 |
| OCI config | 307 to bucket route | 151 | 8,435,824 | 8,435,824 | 0 |
| LFS batch action | presigned URL in JSON | 4,194,304 | 12,642,919 | 12,642,919 | 0 |
| LFS object endpoint | 200 through loom | 4,194,304 | 12,642,919 | 16,838,104 | 4,195,185 |

Every direct transfer matched the complete uploaded payload and SHA-256. A
second read dialed SeaweedFS directly on port 8333 while preserving the signed
Host (`127.0.0.1:31890`), and matched again. The client verifies the bucket path
and presence of the SigV4 signature without logging its query credentials.

Counters are Windows TCP EStats `DataBytesOut`, summed over captured established
IPv4 sockets owned by the loom PID. The direct-transfer window begins after
the redirect or batch action has been received. This is not a host NIC counter
or a claim about every historical socket.

The first attempt exposed a stale client assumption that OCI manifests would
return 200; they also return 307 with SERVE_DIRECT. The client now applies the
same direct-transfer checks to manifests. The next attempt found the LFS
endpoint distinction. The final attempt measured the normal LFS batch action
before retaining the strict endpoint check:

```text
FAIL: LFS expected redirect: 200 OK bytes=4194304 loom_tx_before=12642919 after=16838104 delta=4195185
exit status 1
==> live FAILED (exit 1)
dag: 1/1 targets failed
```

DAG also prints an effect-classification warning for the scratch executable's
unregistered filename. It executes the client; the failure above is the real
runtime assertion, not an inference from DAG's exit code.

## Validation

Required local gates passed:

```sh
bashy go test ./external/loom ./external/seaweedfs ./pkg/binmgr
GOOS=windows bashy go vet ./external/loom ./external/seaweedfs ./pkg/binmgr
GOOS=windows bashy go vet ./scripts/s515-live
```

The native Windows run from the fresh bashy clone used:

```sh
../bashy-s515-fixed.exe go test github.com/qiangli/yoke/external/loom github.com/qiangli/yoke/external/seaweedfs github.com/qiangli/yoke/pkg/binmgr
../bashy-s515-fixed.exe go vet github.com/qiangli/yoke/external/loom github.com/qiangli/yoke/external/seaweedfs github.com/qiangli/yoke/pkg/binmgr
```

Native Windows vet passed. SeaweedFS and binmgr tests passed. The full Windows
loom suite failed in the same six tests documented in round 2: two bare-Git
PATH tests and four process/port cleanup tests. Exact output:
[windows-tests.txt](s515-live/round3/windows-tests.txt). The Windows suite is
not green; no claim is made that those failures were fixed in this round.

Cleanup was verified after the final run: HTTP probes of ports 8333 and 31890
both failed to connect, while a new command through the installed outpost
completed. Scratch sources, binaries, and logs remain available on the peer.

## Remaining decision

Standard LFS clients already fetch the presigned batch-action URL directly,
with the required bucket route and flat loom TX. If that is accepted as the
LFS equivalent of a redirect, the storage behavior is proven. If HTTP 30x from
the LFS object endpoint is mandatory, the managed Gitea implementation needs
that behavior and a new signed release; the yoke storage configuration alone
does not add it. The default gate has not silently relaxed this requirement.
