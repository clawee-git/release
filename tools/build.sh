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
#   CHANNEL       stable | beta (default: stable)
#
# ldflags: always `-X main.version=$STAMP -X main.channel=$CHANNEL`.
#
# THE CHANNEL IS A BUILD-TIME FACT, and both halves of it live here. It decides
# what the binary DERIVES at runtime (core/channel.Parse of the -X value: config
# file, data dir, system roots, launchd label, systemd unit) and what the file
# is CALLED on disk (clawee/claweeb, claweed/claweedb, and their updaters). The
# two must agree or a host ends up with a binary named claweeb that writes into
# stable's tree; they agree because both come from ONE read of core/channel via
# ./cmd/channel-names, never from a `case` table spelled here in shell.
#
# There is deliberately no default for the -X value inside the binaries: Parse
# refuses an empty string, so a build that loses this flag fails to start rather
# than silently running as stable.
#
# darwin signing (only when TARGETOS=darwin AND the build host is darwin):
#   - default          → ad-hoc (`codesign --sign - --force`); macOS refuses to
#                        exec an unsigned native binary. For dev/CI/normal builds.
#   - APPLE_SIGN set    → real Developer ID signature via `modernech-sign sign`
#                        (hardened runtime + secure timestamp). RELEASE-only;
#                        release.sh sets it under its `--apple` flag. Notarization
#                        of the assembled zip happens in release.sh, not here.
# Cross-compiled (linux) outputs are left untouched.
#
# Optional env:
#   APPLE_SIGN     non-empty → Developer ID sign darwin outputs (release mode)
#   MODERNECH_SIGN path to the modernech-sign tool (default: PATH, then ~/bin)
set -euo pipefail

: "${COMP:?COMP is required (clawee|claweed)}"
: "${SRC_DIR:?SRC_DIR is required (component source worktree)}"
: "${TARGETOS:?TARGETOS is required (darwin|linux)}"
: "${TARGETARCH:?TARGETARCH is required (arm64|amd64)}"
: "${STAMP:?STAMP is required}"
: "${OUT_DIR:?OUT_DIR is required}"
CHANNEL="${CHANNEL:-stable}"
case "${CHANNEL}" in
    stable|beta) ;;
    *) echo "✗ CHANNEL must be stable or beta (got '${CHANNEL}')" >&2; exit 2 ;;
esac

GO_BIN="${GO_BIN:-go}"
command -v "${GO_BIN}" >/dev/null 2>&1 || GO_BIN=/opt/homebrew/bin/go
command -v "${GO_BIN}" >/dev/null 2>&1 || { echo "✗ go not found on PATH or /opt/homebrew/bin/go" >&2; exit 1; }

[ -d "${SRC_DIR}" ] || { echo "✗ SRC_DIR '${SRC_DIR}' is not a directory" >&2; exit 1; }

# Resolve the shared Modernech signer once, only when release-mode Apple signing
# is requested (keeps normal/dev builds free of any dependency on it).
if [ -n "${APPLE_SIGN:-}" ]; then
    SIGN_BIN="${MODERNECH_SIGN:-modernech-sign}"
    command -v "${SIGN_BIN}" >/dev/null 2>&1 || SIGN_BIN="${HOME}/bin/modernech-sign"
    command -v "${SIGN_BIN}" >/dev/null 2>&1 \
        || { echo "✗ APPLE_SIGN set but modernech-sign not found on PATH or ~/bin" >&2; exit 1; }
fi

# binary -> package map (space-separated "bin:pkg" pairs per component).
# clawee's source package is cmd/clawee — the binary keeps the clawee name.
#
# claweed is TWO binaries. The setuid-root clawee-spawn helper was retired in the
# daemon repo (the daemon runs as root and forks its own per-user children), and
# ./cmd/clawee-spawn no longer exists — naming it here fails the build outright.
# Keep this map and internal/relconfig's Bins() in step; both must only name
# packages the component actually has.
# The OUTPUT NAME comes from core/channel; the PACKAGE path never does. A twin
# is the same source built twice — ./cmd/clawee builds both clawee and claweeb —
# so renaming the package here would look for a directory that does not exist.
#
# REPO_ROOT is this kit, not SRC_DIR: cmd/channel-names is ours. The `go run`
# happens BEFORE the cd into SRC_DIR for the same reason.
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Captured to a variable FIRST, then eval'd. `eval "$(cmd)"` swallows cmd's
# exit status — the substitution yields an empty string and eval succeeds on it
# — which would leave every name unset and fall through to the emptiness check
# below with no idea why. This way the failure names itself.
CHANNEL_NAMES="$( cd "${REPO_ROOT}" && "${GO_BIN}" run ./cmd/channel-names "${CHANNEL}" "${TARGETOS}" )" \
    || { echo "✗ could not read the ${CHANNEL} names out of core/channel" >&2; exit 1; }
eval "${CHANNEL_NAMES}"
[ -n "${CLIENT:-}" ] && [ -n "${CLIENT_UPDATER:-}" ] && [ -n "${DAEMON:-}" ] && [ -n "${DAEMON_UPDATER:-}" ] \
    || { echo "✗ cmd/channel-names returned an incomplete name set for channel '${CHANNEL}'" >&2; exit 1; }

case "${COMP}" in
    clawee)   MAP="${CLIENT}:./cmd/clawee ${CLIENT_UPDATER}:./cmd/clawee-updater" ;;
    claweed)  MAP="${DAEMON}:./cmd/claweed ${DAEMON_UPDATER}:./cmd/claweed-updater" ;;
    *)        echo "✗ unknown COMP: ${COMP}" >&2; exit 2 ;;
esac

LDFLAGS="-X main.version=${STAMP} -X main.channel=${CHANNEL}"

mkdir -p "${OUT_DIR}"
HOST_OS="$(uname -s)"

cd "${SRC_DIR}"
# shellcheck disable=SC2086  # ${MAP} is an intentional space-list of "bin:pkg" pairs; word-splitting into pairs is the point.
for pair in ${MAP}; do
    bin="${pair%%:*}"
    pkg="${pair#*:}"
    out="${OUT_DIR}/${bin}"
    echo "→ ${COMP}: ${bin}  (GOOS=${TARGETOS} GOARCH=${TARGETARCH}, channel=${CHANNEL}, version=${STAMP})"
    CGO_ENABLED=0 GOOS="${TARGETOS}" GOARCH="${TARGETARCH}" \
        "${GO_BIN}" build -trimpath -ldflags "${LDFLAGS}" -o "${out}" "${pkg}"
    if [ "${TARGETOS}" = "darwin" ] && [ "${HOST_OS}" = "Darwin" ]; then
        if [ -n "${APPLE_SIGN:-}" ]; then
            # release mode: real Developer ID signature (hardened runtime + timestamp)
            "${SIGN_BIN}" sign "${out}" >&2
        else
            # default: ad-hoc — macOS only needs *a* signature to exec the binary
            codesign --sign - --force "${out}" >/dev/null 2>&1 || true
        fi
    fi
    echo "✓ ${out}"
done
