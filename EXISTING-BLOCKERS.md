# Existing blockers — Sprint 457 / 83739c5261a3

Local/loopback scope is delivered; see
[the candidate-linked evidence note](docs/sprint-457-83739c5261a3-evidence.md).
Remaining items are recorded, not faked:

1. **Sprint 465 (operator decision 2026-10-17):** every check needing a real
   account or backend — gh authenticated reads/scopes/pagination against
   github.com or GHES, gcloud project/account calls, rclone remote backends
   (listing, transfer, integrity, cleanup). Not attempted here.
2. **Artifact SBOM/notices:** dependency lock, SBOM, vulnerability audit and
   NOTICE inventory for gh, rclone, curl, gcloud and Ollama artifacts bashy does
   not ship remain unverified. rclone's SHA256SUMS PGP signature is unverified.
3. **Platform coverage:** only darwin/arm64 was executed. gh/rclone pins for the
   other five platforms are committed but not exercised natively.
4. **446 shared seams:** `--since`/bashy-cursor-v1 binding (S19) and typed
   provider error mapping remain owned by 446; no private cursor or error ABI was
   added, so no CLI/MCP typed-result parity is claimed.

No push, service startup or upstream Ollama provisioning occurred.
