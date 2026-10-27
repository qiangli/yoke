# Stable peer store name (S515.2 spike)

Decision: **works**. Store host = `<peer-id>.<sat-id>.localhost` + one fixed
local outpost port (`loom.DefaultStorePort`, 31890).

Why it works
- Go's resolver maps every `*.localhost` name to loopback itself (`net.isLocalhost`),
  on all OSes, with no /etc/hosts, root or DNS. Verified: multi-label names resolve
  to `::1`/`127.0.0.1` (Go on macOS). curl >= 7.78 and browsers special-case it too.
- A Host-routing proxy on that port keeps the original `Host` header when
  forwarding (verified); that is required, because a presigned SigV4 URL signs Host,
  so the peer's seaweedfs must see the same name the signer used.
- Presigned URLs from loom therefore work from any member host, and the same
  URL names the same peer everywhere.

Caveats
- Non-Go clients on Linux using plain glibc without nss-myhostname/systemd-resolved
  may not resolve `*.localhost`; Go clients (containerd/podman, act_runner) are fine.
- Plain HTTP on loopback; use path-style buckets (loom sets MINIO_BUCKET_LOOKUP_TYPE=path).

Needed outside this repo (outpost follow-up): listen on the fixed port on every
outpost, route by Host (`<peer>.<sat>.*`) to that peer's `s3` mesh service
(LAN first, else mesh), passing Host through unchanged.

Loom side (this repo): `external/loom/storage.go` writes Gitea's global `[storage]`
(STORAGE_TYPE=minio, MINIO_*, SERVE_DIRECT=true) from `Options.Storage` or
`$LOOM_S3_PEER`/`LOOM_S3_SAT` (or `LOOM_S3_ENDPOINT`) + `LOOM_S3_ACCESS_KEY`,
`LOOM_S3_SECRET_KEY`, `LOOM_S3_BUCKET`.
