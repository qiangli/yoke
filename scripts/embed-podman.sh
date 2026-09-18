#!/usr/bin/env bash
# Locate or build the podman client and gzip it for embedding into the coreutils
# podman engine. Output: external/podman/engine/podman_embed/podman.gz
# (gitignored built artifact; consumed only with -tags embed_podman).
#
# Source priority:
#   1. A system podman the user already has (version they trust).
#   2. Build from the qiangli/podman fork at external/podman/src/cmd/podman/
#      (Apache-2.0, no external install). macOS/Windows use the `remote` tag
#      (client-only — the engine runs in a podman-machine VM); Linux builds
#      native (full engine).
#
# Soft-skip: if neither source produces a binary, exit 0 with a warning so the
# build still succeeds — bashy podman then falls back to a host/PATH podman until
# the embed is present. Never auto-installs upstream podman. Ported from ycode.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OUT_DIR="${REPO_ROOT}/external/podman/engine/podman_embed"
PODMAN_SRC="${REPO_ROOT}/external/podman/src"

GOOS_BUILD="$(uname -s | tr '[:upper:]' '[:lower:]')"
GOARCH_BUILD="$(uname -m)"
case "${GOARCH_BUILD}" in
    x86_64)  GOARCH_BUILD="amd64" ;;
    aarch64) GOARCH_BUILD="arm64" ;;
esac

PODMAN=""
CLEANUP_PODMAN=""
probe_podman() {
    local candidate="$1"
    [ -x "${candidate}" ] || return 1
    local out
    out="$("${candidate}" --version 2>&1)" || return 1
    case "${out}" in
        "podman version "*) return 0 ;;
        *) return 1 ;;
    esac
}

for candidate in \
    "$(command -v podman 2>/dev/null || true)" \
    /opt/homebrew/bin/podman \
    /usr/local/bin/podman \
    /opt/podman/bin/podman; do
    if [ -n "${candidate}" ] && probe_podman "${candidate}"; then
        PODMAN="${candidate}"
        break
    fi
done

if [ -z "${PODMAN}" ]; then
    if [ ! -d "${PODMAN_SRC}/cmd/podman" ]; then
        echo "WARN: no system podman and ${PODMAN_SRC}/cmd/podman missing (run: git submodule update --init external/podman/src). Skipping embed." >&2
        exit 0
    fi
    case "${GOOS_BUILD}" in
        darwin|windows) BUILDTAGS="remote exclude_graphdriver_btrfs containers_image_openpgp"; VARIANT="podman-remote (client-only)" ;;
        linux)          BUILDTAGS="";                                                          VARIANT="podman (native engine)" ;;
        *) echo "WARN: unsupported GOOS=${GOOS_BUILD} for podman embed; skipping." >&2; exit 0 ;;
    esac
    PODMAN="$(mktemp)"
    CLEANUP_PODMAN="${PODMAN}"
    trap 'rm -f "${CLEANUP_PODMAN}"' EXIT
    echo "Building ${VARIANT} from ${PODMAN_SRC}/cmd/podman (GOOS=${GOOS_BUILD} GOARCH=${GOARCH_BUILD})..."
    if [ -n "${BUILDTAGS}" ]; then
        (cd "${PODMAN_SRC}" && GOWORK=off go build -trimpath -tags "${BUILDTAGS}" -o "${PODMAN}" ./cmd/podman/)
    else
        (cd "${PODMAN_SRC}" && GOWORK=off go build -trimpath -o "${PODMAN}" ./cmd/podman/)
    fi
    if [ ! -s "${PODMAN}" ]; then
        echo "WARN: podman build produced no output. Skipping embed." >&2
        exit 0
    fi
fi

echo "Using podman at: ${PODMAN}"
"${PODMAN}" --version
RAW_SIZE=$(du -h "${PODMAN}" | cut -f1)
echo "Compressing ${PODMAN} (${RAW_SIZE}) for embedding..."
mkdir -p "${OUT_DIR}"
gzip -9 -c "${PODMAN}" > "${OUT_DIR}/podman.gz"
echo "Compressed: ${OUT_DIR}/podman.gz ($(du -h "${OUT_DIR}/podman.gz" | cut -f1))"
echo "To embed:   go build -tags embed_podman ./cmd/bashy/  (bashy Makefile does this automatically when the blob exists)"
