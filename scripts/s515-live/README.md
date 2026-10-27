# S515.2 Windows live gate, round 3

This client drives real loom, outpost, and keyed SeaweedFS processes on one
Windows peer. Cross-host proof belongs to S515.4. Historical failed evidence:
[round 2](../../docs/s515-2-live-round2.md).
The current [round-3 result](../../docs/s515-2-live-round3.md) passes OCI
redirects and direct LFS batch downloads, but fails the strict LFS object
endpoint 30x check. The default gate intentionally still exits nonzero.

Use the manager's private `s515-peer` claim for access. Do not put the real
access string in this repository. Every remote command begins with:

```sh
outpost ssh <user>@<peer>.local:2222 -- 'export PATH; <commands>'
```

In a fresh scratch directory (round 3 used `C:/s515/round3-635`), clone and
build current mains on the peer:

```sh
for repo in bashy yoke outpost sh coreutils; do
  bashy git clone --depth 1 https://github.com/qiangli/$repo.git $repo
done
cd bashy
bashy go mod edit -replace=github.com/qiangli/yoke=../yoke -replace=mvdan.cc/sh/v3=../sh
bashy go build -o ../bashy-s515-fixed.exe ./cmd/bashy
cd ../outpost
bashy go mod edit -replace=github.com/qiangli/yoke=../yoke -replace=mvdan.cc/sh/v3=../sh
bashy go build -o ../outpost-s515.exe ./cmd/outpost
cd ..
```

Provision Go serially. A version probe is not a compile gate. Record each
clone's `bashy git rev-parse HEAD` and inspect build logs before proceeding.

Copy `acceptance_windows.go`, `stack_windows.go`, and `dag.md` to the scratch
root with `outpost scp -P 2222`; rename the latter to `live-dag.md`. Run:

```sh
./bashy-s515-fixed.exe dag live-dag.md live
```

The stack helper refuses occupied storage ports, writes an isolated outpost
configuration with synthetic peer ID `s515self` and mesh disabled, and starts
real outpost and SeaweedFS processes. It uses scratch HOME/config/cache paths
for outpost, an independent `weed` directory, and one test S3 identity:
`SEAWEEDFS_S3_ACCESS_KEY=s515-proof`,
`SEAWEEDFS_S3_SECRET_KEY=s515-proof-secret`. These are disposable fixture
credentials, never production credentials. No manual bucket creation is
needed: loom creates `s3-s515self` using its matching credentials.

The client launches loom on a dynamic HTTP port with `LOOM_S3_PEER=s515self`
and matching `LOOM_S3_ACCESS_KEY`/`LOOM_S3_SECRET_KEY`. No endpoint/bucket
override is needed. It pushes and pulls a valid OCI manifest/config/tar layer
and an LFS object, requiring:

* unsigned PUT rejected with 403 both directly and through outpost;
* download 30x to `127.0.0.1:31890/s3-s515self/...`, with a SigV4 signature;
* complete payload and SHA-256 agreement;
* loom TCP TX unchanged during the direct transfer;
* a second direct SeaweedFS read matching the payload, retaining the original
  signed Host header while dialing port 8333.

For LFS it first fetches the batch response's download action, as a standard
client does, then checks the object endpoint for the requested HTTP redirect.
Gitea currently returns 200 from that endpoint, so this last assertion fails.

The stack helper cleans up scratch outpost, the SeaweedFS wrapper and child;
loom cleanup is owned by the client. The installed outpost is never stopped.
Inspect `loom.log`, `weed.log`, and `outpost.log` on failure. Run in a fresh
scratch directory to avoid reusing a previous fixture.

Counters use Windows `GetExtendedTcpTable` to select the loom PID's established
IPv4 sockets, then `SetPerTcpConnectionEStats`/`GetPerTcpConnectionEStats`
(`TcpConnectionEstatsData.DataBytesOut`). A normal loom HTTP response is a
positive control. These are sampled TCP data-byte counters, not whole-host
NIC counters or every historical socket. The flat-counter window starts
after the redirect and covers the direct store fetch.

For manual service setup, run only `go run acceptance_windows.go`; omitting
`stack_windows.go` leaves service lifecycle to the caller. Historical
`S515_DIAGNOSTIC_ENDPOINT` / `S515_DIAGNOSTIC_ANONYMOUS` modes are diagnostic
only and are never stable-address acceptance. Do not set them for this gate.
