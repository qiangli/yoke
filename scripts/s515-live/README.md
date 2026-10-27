# S515.2 Windows live gate

This client drives real loom, outpost, and SeaweedFS processes. It is not a
replacement server/router. Run all builds and this gate on the assigned peer.
The current result is **FAIL**, documented in
[round-2 evidence](../../docs/s515-2-live-round2.md).

Use the private manager claim for access; never put the real access string in
this repository. Every SSH command must start with `export PATH;`:

```sh
outpost ssh <user>@<peer>.local:2222 -- 'export PATH; <peer commands>'
```

In a dedicated peer scratch directory (this run used `C:/s515/round2-633`):

```sh
bashy git clone --depth 1 https://github.com/qiangli/bashy.git bashy
bashy git clone --depth 1 https://github.com/qiangli/yoke.git yoke
bashy git clone --depth 1 https://github.com/qiangli/outpost.git outpost
bashy git clone --depth 1 https://github.com/qiangli/sh.git sh
bashy git clone --depth 1 https://github.com/qiangli/coreutils.git coreutils
printf 'package main\nimport "fmt"\nfunc main(){fmt.Println("s515 tiny build PASS")}\n' > tiny.go
bashy go build -o tiny.exe tiny.go
./tiny.exe
cd bashy
bashy go mod edit -replace=github.com/qiangli/yoke=../yoke -replace=mvdan.cc/sh/v3=../sh
bashy go build -o ../bashy-s515.exe ./cmd/bashy
cd ../outpost
bashy go mod edit -replace=github.com/qiangli/yoke=../yoke -replace=mvdan.cc/sh/v3=../sh
bashy go build -o ../outpost-s515.exe ./cmd/outpost
cd ..
```

Overlay this delivery's yoke changes before building the fixed candidate:

```sh
cd bashy
../bashy-s515.exe go build -o ../bashy-s515-fixed.exe ./cmd/bashy
cd ..
```

Provision Go serially. Old tree caches without the completion marker are
re-provisioned on first use of the new binary; `go version` alone is not a
sufficient toolchain gate. The delivered tree lock protects concurrent new
installers; older binaries do not participate in it.

Run SeaweedFS in a separate SSH session, with its own data directory:

```sh
./bashy-s515.exe seaweedfs serve --data C:/s515/round2-633/weed --port 8333 > weed.log 2>&1
```

The wrapper starts the pinned bashy-built SeaweedFS and its internal loopback
services. No S3 identities are configured in this proof. Create the bucket:

```sh
bashy curl -i -X PUT http://127.0.0.1:8333/loom
```

Write `home/config/matrix/agent.json` under the scratch directory:

```json
{"agent_name":"s515self","admin_addr":"127.0.0.1:0","mesh_enabled":false,"ssh_enabled":false,"mesh_services":[{"name":"s3","addr":"127.0.0.1:8333"}]}
```

Run the actual new outpost in another SSH session:

```sh
HOME=C:/s515/round2-633/home USERPROFILE=C:/s515/round2-633/home XDG_CONFIG_HOME=C:/s515/round2-633/home/config XDG_CACHE_HOME=C:/s515/round2-633/home/cache APPDATA=C:/s515/round2-633/home/appdata ./outpost-s515.exe start --admin-addr 127.0.0.1:0 > outpost.log 2>&1
bashy curl -i http://s515self.s515.localhost:31890/loom
```

Copy `acceptance_windows.go` and `dag.md` from this directory to the scratch
root, naming the latter `live-dag.md`. The client starts `bashy-s515-fixed.exe
loom serve` on a dynamic loopback port, enables LFS in a fresh fixture, and
uses `LOOM_S3_PEER=s515self`, `LOOM_S3_SAT=s515`, test keys, and bucket `loom`.
It pushes an OCI manifest/config/tar layer through the Distribution API and
an LFS object through the batch API. It checks download status, signed store
hostname, payload hashes, and direct SeaweedFS reads. It kills its loom
processes when the run finishes.

```sh
./bashy-s515-fixed.exe dag live-dag.md live
```

Two explicitly non-acceptance diagnostic modes isolate the observed failures:

```sh
S515_DIAGNOSTIC_ENDPOINT=127.0.0.1:8333 ./bashy-s515-fixed.exe go run acceptance_windows.go
S515_DIAGNOSTIC_ENDPOINT=127.0.0.1:8333 S515_DIAGNOSTIC_ANONYMOUS=1 ./bashy-s515-fixed.exe go run acceptance_windows.go
```

The second mode still exits nonzero when downloads are proxied. It reports
hashes and TX deltas for the image and LFS before reporting the failures.
Do not call either diagnostic mode stable-name acceptance.

Counters use Windows `GetExtendedTcpTable` to select the loom PID's established
IPv4 sockets, then `SetPerTcpConnectionEStats`/`GetPerTcpConnectionEStats`
(`TcpConnectionEstatsData.DataBytesOut`). A normal loom HTTP response is a
positive control. These are sampled TCP data-byte counters, not whole-host
NIC counters, disk I/O counters, or a claim about every historical socket.
For a redirect, the flat-counter window starts after reading the redirect and
covers the direct store fetch. A 200 fallback instead measures the loom
request and reports the growing counter as a failure.

Stop only the scratch outpost, SeaweedFS, and their wrappers after collecting
evidence. Preserve the installed outpost and other lanes' processes/data.
The proof is single-host; cross-host validation belongs to S515.4.
