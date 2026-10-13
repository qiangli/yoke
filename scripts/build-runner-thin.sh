#!/usr/bin/env bash
# Build Ollama's thin llama.cpp runner from the pinned vendored source and
# compress it for runner_embed. The native build is intentionally separate
# from the Go server binary: cgo and the platform GPU backend belong here.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
OLLAMA_SRC="${REPO_ROOT}/external/ollama/src"
OUT_DIR="${REPO_ROOT}/external/ollama/runner_embed"
OUT_FILE="${OUT_DIR}/ycode-runner.gz"

usage() {
    echo "usage: scripts/build-runner-thin.sh [--check]" >&2
    exit 2
}

CHECK_ONLY=0
case "${1:-}" in
    "") ;;
    --check) CHECK_ONLY=1 ;;
    *) usage ;;
esac
[ "$#" -le 1 ] || usage

for tool in git go gzip; do
    command -v "${tool}" >/dev/null 2>&1 || {
        echo "ERROR: required tool not found: ${tool}" >&2
        exit 1
    }
done

# The superproject gitlink is the source pin. Refuse a missing, advanced, or
# locally modified checkout so release bytes cannot silently come from a
# different Ollama revision.
EXPECTED_REV="$(git -C "${REPO_ROOT}" rev-parse :external/ollama/src 2>/dev/null || true)"
if ! [[ "${EXPECTED_REV}" =~ ^[0-9a-f]{40}$ ]]; then
    echo "ERROR: external/ollama/src is not recorded as a pinned gitlink" >&2
    exit 1
fi
if [ ! -f "${OLLAMA_SRC}/go.mod" ] || [ ! -f "${OLLAMA_SRC}/cmd/runner/main.go" ]; then
    echo "ERROR: pinned Ollama source is not hydrated at ${OLLAMA_SRC}" >&2
    echo "       Run: git submodule update --init external/ollama/src" >&2
    exit 1
fi
ACTUAL_REV="$(git -C "${OLLAMA_SRC}" rev-parse HEAD 2>/dev/null || true)"
if [ "${ACTUAL_REV}" != "${EXPECTED_REV}" ]; then
    echo "ERROR: Ollama checkout ${ACTUAL_REV:-<unknown>} does not match pinned ${EXPECTED_REV}" >&2
    exit 1
fi
if [ -n "$(git -C "${OLLAMA_SRC}" status --porcelain --untracked-files=no)" ]; then
    echo "ERROR: pinned Ollama source has tracked local modifications" >&2
    exit 1
fi

TARGET_OS="${RUNNER_TARGET_OS:-$(go env GOOS)}"
TARGET_ARCH="${RUNNER_TARGET_ARCH:-$(go env GOARCH)}"
case "${TARGET_OS}/${TARGET_ARCH}" in
    darwin/arm64)
        # Ollama carries the Apple Metal backend in-tree for this target.
        NEED_CMAKE=0
        ;;
    linux/amd64|linux/arm64)
        NEED_CMAKE=1
        ;;
    *)
        echo "ERROR: unsupported embedded-runner target: ${TARGET_OS}/${TARGET_ARCH}" >&2
        echo "       Supported targets: darwin/arm64, linux/amd64, linux/arm64" >&2
        exit 1
        ;;
esac

if [ "${NEED_CMAKE}" -eq 1 ]; then
    command -v cmake >/dev/null 2>&1 || {
        echo "ERROR: cmake is required for ${TARGET_OS}/${TARGET_ARCH}" >&2
        exit 1
    }
fi

if [ "${CHECK_ONLY}" -eq 1 ]; then
    echo "runner build check: pinned ${EXPECTED_REV} for ${TARGET_OS}/${TARGET_ARCH} (CGO_ENABLED=1, cmake=${NEED_CMAKE})"
    echo "runner build check: output ${OUT_FILE}"
    exit 0
fi

# This is a native cgo build. Cross-compiling it without a matching C/C++
# toolchain can produce a CPU-only or unusable runner while appearing to pass.
HOST_OS="$(uname -s | tr '[:upper:]' '[:lower:]')"
HOST_ARCH="$(uname -m)"
case "${HOST_ARCH}" in
    x86_64) HOST_ARCH=amd64 ;;
    aarch64) HOST_ARCH=arm64 ;;
esac
if [ "${TARGET_OS}/${TARGET_ARCH}" != "${HOST_OS}/${HOST_ARCH}" ]; then
    echo "ERROR: runner builds must be native; target ${TARGET_OS}/${TARGET_ARCH}, host ${HOST_OS}/${HOST_ARCH}" >&2
    exit 1
fi

BUILD_DIR="$(mktemp -d "${TMPDIR:-/tmp}/yoke-runner.XXXXXX")"
trap 'rm -rf "${BUILD_DIR}"' EXIT
RUNNER_BIN="${BUILD_DIR}/ycode-runner"

echo "Building runner from pinned Ollama ${EXPECTED_REV} for ${TARGET_OS}/${TARGET_ARCH}..."
(
    cd "${OLLAMA_SRC}"
    # Export before generation: a parent go.work must never redirect vendored
    # package resolution. Scope generation to the runner's backend families.
    export GOWORK=off
    go generate ./ml/... ./x/...
    CGO_ENABLED=1 go build -trimpath -o "${RUNNER_BIN}" ./cmd/runner/
)
[ -s "${RUNNER_BIN}" ] || {
    echo "ERROR: runner build produced no binary" >&2
    exit 1
}

mkdir -p "${OUT_DIR}"
TMP_GZ="${BUILD_DIR}/ycode-runner.gz"
gzip -n -9 -c "${RUNNER_BIN}" > "${TMP_GZ}"
gzip -t "${TMP_GZ}"
mv "${TMP_GZ}" "${OUT_FILE}"

echo "Built ${OUT_FILE} from ${EXPECTED_REV}"
echo "Embed with the build tag: embed_runner"
