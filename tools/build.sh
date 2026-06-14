#!/usr/bin/env bash
# build.sh — cross-compile ONE Clawee component for ONE target.
#
# Builds from the component's OWN source worktree (so its in-worktree go.work
# resolves the tag-pinned `core` — and, for claweed, `cli`). Each component
# emits one or more binaries; the binary→package map is fixed below. CGO is
# always off (pure-Go, portable).
#
# Env in (all required unless noted):
#   COMP          clawee | claweed
#   SRC_DIR       the component's source worktree (cd target)
#   TARGETOS      GOOS  (darwin | linux)
#   TARGETARCH    GOARCH (arm64 | amd64)
#   STAMP         version string baked via -X main.version=…
#   OUT_DIR       output directory for the built binaries (created if absent)
#
# ldflags: always `-X main.version=$STAMP`.
# If TARGETOS=darwin and the build host is darwin, each output is ad-hoc
# codesigned (`codesign --sign - --force`) — macOS refuses to exec unsigned
# native binaries. Cross-compiled (linux) outputs are left untouched.
set -euo pipefail

: "${COMP:?COMP is required (clawee|claweed)}"
: "${SRC_DIR:?SRC_DIR is required (component source worktree)}"
: "${TARGETOS:?TARGETOS is required (darwin|linux)}"
: "${TARGETARCH:?TARGETARCH is required (arm64|amd64)}"
: "${STAMP:?STAMP is required}"
: "${OUT_DIR:?OUT_DIR is required}"

GO_BIN="${GO_BIN:-go}"
command -v "${GO_BIN}" >/dev/null 2>&1 || GO_BIN=/opt/homebrew/bin/go
command -v "${GO_BIN}" >/dev/null 2>&1 || { echo "✗ go not found on PATH or /opt/homebrew/bin/go" >&2; exit 1; }

[ -d "${SRC_DIR}" ] || { echo "✗ SRC_DIR '${SRC_DIR}' is not a directory" >&2; exit 1; }

# binary -> package map (space-separated "bin:pkg" pairs per component).
# clawee's source package is cmd/clawee — the binary keeps the clawee name.
case "${COMP}" in
    clawee)   MAP="clawee:./cmd/clawee" ;;
    claweed)  MAP="claweed:./cmd/claweed clawee-spawn:./cmd/clawee-spawn" ;;
    *)        echo "✗ unknown COMP: ${COMP}" >&2; exit 2 ;;
esac

LDFLAGS="-X main.version=${STAMP}"

mkdir -p "${OUT_DIR}"
HOST_OS="$(uname -s)"

cd "${SRC_DIR}"
# shellcheck disable=SC2086  # ${MAP} is an intentional space-list of "bin:pkg" pairs; word-splitting into pairs is the point.
for pair in ${MAP}; do
    bin="${pair%%:*}"
    pkg="${pair#*:}"
    out="${OUT_DIR}/${bin}"
    echo "→ ${COMP}: ${bin}  (GOOS=${TARGETOS} GOARCH=${TARGETARCH}, version=${STAMP})"
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
        "${GO_BIN}" build -trimpath -ldflags "${LDFLAGS}" -o "${out}" "${pkg}"
    if [ "${TARGETOS}" = "darwin" ] && [ "${HOST_OS}" = "Darwin" ]; then
        codesign --sign - --force "${out}" >/dev/null 2>&1 || true
    fi
    echo "✓ ${out}"
done
