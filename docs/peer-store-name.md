# Stable peer store address (S515.2)

## Decision (round 3, 2026-10-27): bucket route, no DNS name

The `<peer>.<sat>.localhost` name is dropped. Loom's `[storage]` uses
`MINIO_ENDPOINT=127.0.0.1:<store port, default 31890>`,
`MINIO_BUCKET=s3-<peer-id>` (`LOOM_S3_PEER`), path-style lookup,
`SERVE_DIRECT=true`, keys from `LOOM_S3_ACCESS_KEY`/`LOOM_S3_SECRET_KEY`.
The peer is selected by the first path segment (`/s3-<peer-id>/...`), which
outpost routes to that peer's `s3` service; the Host is always loopback, so it
resolves on every OS and SigV4 stays valid. `LOOM_S3_ENDPOINT` / `LOOM_S3_BUCKET`
remain explicit overrides.

The seaweedfs wrapper configures one S3 identity from
`SEAWEEDFS_S3_ACCESS_KEY`/`SEAWEEDFS_S3_SECRET_KEY` (`s3-config.json` in the
data dir, 0600); unset keeps open mode. Open mode rejects signed writes, so
SERVE_DIRECT needs the keys set on both sides.

## Why the DNS name failed (historical)

Status of the DNS-name design: **not accepted on Windows**. The intended name is
`<peer-id>.<sat-id>.localhost:31890`, with outpost routing the HTTP Host to
that peer's `s3` service. Host must survive unchanged because SigV4 signs it.

The earlier spike incorrectly asserted that Go's `net.isLocalhost` resolves
all `*.localhost` names to loopback on every OS. That predicate is not a
portable DNS mapping. In the round-2 Windows run, the real Gitea 28.1.0
binary, built with Go 1.27.2, failed during MinIO initialization:

```text
ObjectStorage.BucketExists: endpoint=s515self.s515.localhost:31890,
location=us-east-1, bucket=loom:
Head "http://s515self.s515.localhost:31890/loom/":
dial tcp: lookup s515self.s515.localhost: no such host
```

The real outpost `a0c77910` router **does** route the SELF peer to a configured
local `s3` service without a mesh. A localhost-aware HTTP client returned 200
and `Server: SeaweedFS 30GB 4.48` through the stable name. That proves the
router, not Go/Gitea name resolution.

To complete this design without system hosts/DNS changes, the Gitea S3
transport and each consuming Go client need an explicit loopback dial rule
for the reserved store-name namespace, preserving the original URL and Host.
Gitea's MinIO transport does not currently use its general proxy settings.
Do not treat browser/curl resolution as proof for Gitea or container clients.

A second live constraint: open SeaweedFS 4.48 rejects writes signed with
unconfigured credentials (`Signed request requires setting up SeaweedFS S3
authentication`). Anonymous uploads work, but cannot establish the intended
presigned-download behavior. The requested open-loopback proof must not be
reported as authenticated S3 acceptance.

See [round-2 evidence](s515-2-live-round2.md) and the reproducible
[Windows client](../scripts/s515-live/acceptance_windows.go). This is a
single-host investigation; cross-host proof remains S515.4.
