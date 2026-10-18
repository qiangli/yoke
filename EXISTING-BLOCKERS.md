# Existing blockers — Sprint 457 / 83739c5261a3

Partial candidate only; do not accept as full client admission.

1. Registry owner: `external/registry.Entry.Ensure` returns a cached gcloud when
   PATH lacks the SDK. Reproduce with `S457_AUDIT_HOST_ONLY=1 go test -short
   -count=1 -v ./external/gcloud -run TestQualificationProbeCacheMustNotReplaceHostSDK`.
   Registry is read-only for this worker; no fake executable is run by the probe.
2. 446 owner/manager: supply the delivered adapt rows/engine and accepted shared
   bounds, errors and bashy-cursor-v1 implementation. No private ABI added.
3. Admission owner/manager: reviewed release/artifact pins, per-platform SHA-256,
   dependency/SBOM/license/NOTICE and no-egress evidence remain absent. gh/rclone
   latest defaults are not an admitted pin; passthrough is not per-mode enforcement.
4. Explicit rclone artifact, test accounts/endpoints and native remote gates are
   missing. No actual transfer, authenticated pagination, general redaction, CLI/MCP
   parity or signed Ollama runtime support is claimed.

Three serious investigation paths and all raw gates are recorded in
[the candidate-linked evidence note](docs/sprint-457-83739c5261a3-evidence.md).
Scoped fixes and passing fixtures are committed; manager must arrange the remote
whole-package gate before push. No push, service startup or provisioning occurred.
