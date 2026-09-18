#!/usr/bin/env bash
# Build all podman-engine embed blobs for the host platform. Each sub-script
# soft-skips on failure so a partial set still yields a working (degraded) build.
# After running, build bashy with `make build` (its Makefile auto-adds the
# embed_* tags for whichever blobs exist).
set -uo pipefail
D="$(cd "$(dirname "$0")" && pwd)"
"${D}/embed-podman.sh"  || echo "podman embed skipped"
"${D}/embed-gvproxy.sh" || echo "gvproxy embed skipped"
if [ "$(uname -s)" = "Darwin" ]; then "${D}/embed-vfkit.sh" || echo "vfkit embed skipped"; fi
