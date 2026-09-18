#!/usr/bin/env bash
# Build gvproxy from the Go module cache and gzip it for embedding into the
# coreutils podman engine. gvproxy (containers/gvisor-tap-vsock, Apache-2.0) is
# the user-mode network proxy podman machine forwards host sockets through.
# Output: external/podman/engine/gvproxy_embed/gvproxy.gz (gitignored built
# artifact; consumed only with -tags embed_gvproxy). Ported from ycode.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/external/podman/engine/gvproxy_embed"
PODMAN_SRC="${REPO_ROOT}/external/podman/src"
mkdir -p "${OUT_DIR}"

# Resolve the version the fork pins, then `go install` the cmd main at that
# version into a temp GOBIN. `go install <pkg>@<version>` resolves the main's
# own transitive deps in an isolated context — so it works regardless of which
# subset of gvproxy's tree coreutils' own go.sum covers.
GVPROXY_VER=$(grep -E '^\s*github.com/containers/gvisor-tap-vsock\s' \
    "${PODMAN_SRC}/go.mod" | head -1 | awk '{print $2}')
if [ -z "${GVPROXY_VER}" ]; then
    echo "ERROR: gvisor-tap-vsock not found in ${PODMAN_SRC}/go.mod" >&2
    exit 1
fi

GOBIN_DIR="$(mktemp -d)"
trap 'rm -rf "${GOBIN_DIR}"' EXIT
echo "Installing gvproxy cmd @ ${GVPROXY_VER} (isolated dep resolution)..."
GOWORK=off GOFLAGS=-mod=mod GOBIN="${GOBIN_DIR}" \
    go install "github.com/containers/gvisor-tap-vsock/cmd/gvproxy@${GVPROXY_VER}"
TMP_BIN="${GOBIN_DIR}/gvproxy"
[ -s "${TMP_BIN}" ] || { echo "ERROR: gvproxy install produced no binary" >&2; exit 1; }

if [ "$(uname -s)" = "Darwin" ]; then
    codesign --force --sign - "${TMP_BIN}" 2>/dev/null || true
fi

RAW_SIZE=$(du -h "${TMP_BIN}" | cut -f1)
echo "Compressing gvproxy (${RAW_SIZE}) for embedding..."
gzip -9 -c "${TMP_BIN}" > "${OUT_DIR}/gvproxy.gz"
echo "Compressed: ${OUT_DIR}/gvproxy.gz ($(du -h "${OUT_DIR}/gvproxy.gz" | cut -f1))"
echo "To embed:   go build -tags embed_gvproxy ./cmd/bashy/  (bashy Makefile does this automatically when the blob exists)"
