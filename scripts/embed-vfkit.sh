#!/usr/bin/env bash
# Locate or build vfkit and gzip it for embedding into the coreutils podman
# engine. vfkit (crc-org/vfkit, Apache-2.0) is the Apple Virtualization Framework
# helper podman machine uses on macOS. Output:
# external/podman/engine/vfkit_embed/vfkit.gz (gitignored; consumed only with
# -tags embed_vfkit). macOS-only meaningful; ported from ycode.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/external/podman/engine/vfkit_embed"
PODMAN_SRC="${REPO_ROOT}/external/podman/src"
mkdir -p "${OUT_DIR}"

VFKIT=""
CLEANUP_VFKIT=""
if command -v vfkit &>/dev/null; then
    VFKIT="$(command -v vfkit)"
elif [ -f /opt/homebrew/bin/vfkit ]; then
    VFKIT="/opt/homebrew/bin/vfkit"
elif [ -f /usr/local/bin/vfkit ]; then
    VFKIT="/usr/local/bin/vfkit"
elif [ -f /opt/podman/bin/vfkit ]; then
    VFKIT="/opt/podman/bin/vfkit"
else
    VFKIT_VER=$(grep -E '^\s*github.com/crc-org/vfkit\s' "${PODMAN_SRC}/go.mod" | head -1 | awk '{print $2}')
    if [ -n "${VFKIT_VER}" ]; then
        GOMODCACHE=$(go env GOMODCACHE)
        GOBIN_DIR="$(mktemp -d)"
        CLEANUP_VFKIT="${GOBIN_DIR}"
        trap 'rm -rf "${CLEANUP_VFKIT}"' EXIT
        echo "Installing vfkit cmd @ ${VFKIT_VER} (isolated dep resolution)..."
        GOWORK=off GOFLAGS=-mod=mod GOBIN="${GOBIN_DIR}" \
            go install "github.com/crc-org/vfkit/cmd/vfkit@${VFKIT_VER}"
        VFKIT="${GOBIN_DIR}/vfkit"
        if [ -s "${VFKIT}" ] && [ "$(uname -s)" = "Darwin" ]; then
            ENT="${GOMODCACHE}/github.com/crc-org/vfkit@${VFKIT_VER}/vf.entitlements"
            if [ -f "${ENT}" ]; then
                codesign --force --entitlements "${ENT}" --sign - "${VFKIT}" 2>/dev/null || true
            else
                codesign --force --sign - "${VFKIT}" 2>/dev/null || true
            fi
        fi
        [ -s "${VFKIT}" ] || VFKIT=""
    fi
fi

if [ -z "$VFKIT" ]; then
    echo "ERROR: vfkit not found and module-cache fallback failed." >&2
    echo "Install with:  brew install vfkit" >&2
    exit 1
fi

echo "Using vfkit at: ${VFKIT}"
RAW_SIZE=$(du -h "${VFKIT}" | cut -f1)
echo "Compressing ${VFKIT} (${RAW_SIZE}) for embedding..."
gzip -9 -c "${VFKIT}" > "${OUT_DIR}/vfkit.gz"
echo "Compressed: ${OUT_DIR}/vfkit.gz ($(du -h "${OUT_DIR}/vfkit.gz" | cut -f1))"
