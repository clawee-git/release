#!/usr/bin/env bash
# version.sh — per-component version + deploy stamp for the Clawee release repo.
#
# Each component (clawee|claweed) has its own one-line MAJOR.MINOR.PATCH file
# under versions/<comp> — the single source of truth for that component's semver
# segment. This composes the full stamp used in ldflags, git tags, and marker
# commits:
#
#   v<X.Y.Z>.<YYYY>.<MM>.<DD>.<sha8>
#
# where <sha8> = the HEAD short hash of the COMPONENT SOURCE worktree
# (pass its path via SRC_DIR), and the date is today (UTC).
#
# Usage:
#   tools/version.sh <comp> --semver       # just X.Y.Z
#   tools/version.sh <comp> --stamp        # full stamp (needs SRC_DIR)
#   tools/version.sh <comp> --bump-patch   # X.Y.(Z+1)  + git add versions/<comp>
#   tools/version.sh <comp> --bump-minor   # X.(Y+1).0  + git add versions/<comp> (gated)
#   tools/version.sh <comp> --bump-major   # (X+1).0.0  + git add versions/<comp> (gated)
#
# Minor/major prompt unless CLAWEE_RELEASE_YES=1 (or non-TTY → refuse).
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

COMP="${1:-}"
case "${COMP}" in
    clawee|claweed) ;;
    "")  echo "✗ usage: version.sh <clawee|claweed> <action>" >&2; exit 2 ;;
    *)   echo "✗ unknown component: ${COMP}" >&2; exit 2 ;;
esac
VERSION_FILE="${REPO_ROOT}/versions/${COMP}"
[ -f "${VERSION_FILE}" ] || { echo "✗ versions/${COMP} not found at ${VERSION_FILE}" >&2; exit 1; }

read_semver() {
    local raw; raw="$(tr -d '\r\n[:space:]' < "${VERSION_FILE}")"
    [[ "${raw}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "✗ versions/${COMP} '${raw}' not MAJOR.MINOR.PATCH" >&2; exit 1; }
    printf '%s' "${raw}"
}
# Side-effect: stages versions/<comp> so the caller (release.sh) can commit/revert it as one unit.
write_semver() { printf '%s\n' "$1" > "${VERSION_FILE}"; ( cd "${REPO_ROOT}" && git add "versions/${COMP}" ); }

src_sha() {
    [ -n "${SRC_DIR:-}" ] || { echo "✗ --stamp needs SRC_DIR (the component source worktree)" >&2; exit 2; }
    [ -d "${SRC_DIR}" ]   || { echo "✗ SRC_DIR '${SRC_DIR}' not a directory" >&2; exit 1; }
    git -C "${SRC_DIR}" rev-parse --short=8 HEAD 2>/dev/null \
        || { echo "✗ SRC_DIR '${SRC_DIR}' is not a git worktree" >&2; exit 1; }
}
today_utc() { date -u +%Y.%m.%d; }
stamp()     { printf 'v%s.%s.%s' "$1" "$(today_utc)" "$2"; }

bump() {
    local kind="$1" cur major minor patch new
    cur="$(read_semver)"; IFS='.' read -r major minor patch <<<"${cur}"
    case "${kind}" in
        patch) new="${major}.${minor}.$((patch+1))" ;;
        minor) new="${major}.$((minor+1)).0" ;;
        major) new="$((major+1)).0.0" ;;
        *) echo "✗ unknown bump kind: ${kind}" >&2; exit 1 ;;
    esac
    if [ "${kind}" != "patch" ] && [ "${CLAWEE_RELEASE_YES:-0}" != "1" ]; then
        [ -t 0 ] || { echo "✗ ${kind} bump ${cur}→${new} needs a TTY or CLAWEE_RELEASE_YES=1" >&2; exit 1; }
        printf '%s %s bump %s → %s. Continue? [y/N] ' "${COMP}" "${kind}" "${cur}" "${new}" >&2
        local r; read -r r; case "${r}" in y|Y|yes|YES) ;; *) echo "✗ aborted" >&2; exit 1 ;; esac
    fi
    write_semver "${new}"; printf '%s\n' "${new}"
}

case "${2:-}" in
    --semver)      read_semver; printf '\n' ;;
    --stamp)       _sv="$(read_semver)"; _sha="$(src_sha)"; stamp "${_sv}" "${_sha}"; printf '\n' ;;
    --bump-patch)  bump patch ;;
    --bump-minor)  bump minor ;;
    --bump-major)  bump major ;;
    -h|--help)     sed -n '2,21p' "$0" | sed 's/^# \{0,1\}//' ;;
    "")            echo "✗ usage: version.sh ${COMP} <--semver|--stamp|--bump-patch|--bump-minor|--bump-major>" >&2; exit 2 ;;
    *)             echo "✗ unknown action: ${2}" >&2; exit 2 ;;
esac
