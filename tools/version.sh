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
# TWO CHANNELS, TWO FILES, ONE SCRIPT. CHANNEL=beta reads and writes
# versions/<comp>.beta.stamp instead, and composes
#
#   v<X.Y.Z>.beta.<YYYY>.<MM>.<DD>.<sha8>
#
# The beta line is a SEPARATE file, not a suffix on the stable one, for the
# reason beta.md §5 gives: a cycle's beta cuts climb their own patch
# (v0.3.0-beta, v0.3.1-beta, …) while stable stays exactly where it is, and the
# stable cut at close adopts the reached patch. One file cannot hold two lines
# that move independently.
#
# THE FILE'S PRESENCE IS THE OPEN-CYCLE MARKER. `beta` the branch is permanent
# and says nothing about whether a cycle is open; the seeded version file is
# what says it (beta.md §3). So --semver/--stamp on the beta channel REFUSE
# when the file is absent, naming --seed-beta, rather than inventing a line —
# a beta cut of a closed cycle is the operator error this catches.
#
# Usage:
#   tools/version.sh <comp> --semver           # just X.Y.Z
#   tools/version.sh <comp> --stamp            # full stamp (needs SRC_DIR)
#   tools/version.sh <comp> --bump-patch       # X.Y.(Z+1)  + git add the file
#   tools/version.sh <comp> --bump-minor       # X.(Y+1).0  + git add the file (gated)
#   tools/version.sh <comp> --bump-major       # (X+1).0.0  + git add the file (gated)
#   tools/version.sh <comp> --seed-beta X.Y.0  # open a cycle: write versions/<comp>.beta.stamp
#
# CHANNEL=stable|beta (default stable) selects which file the first five act on.
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
CHANNEL="${CHANNEL:-stable}"
case "${CHANNEL}" in
    stable|beta) ;;
    *) echo "✗ CHANNEL must be stable or beta (got '${CHANNEL}')" >&2; exit 2 ;;
esac

if [ "${CHANNEL}" = beta ]; then
    VERSION_REL="versions/${COMP}.beta.stamp"
else
    VERSION_REL="versions/${COMP}"
fi
VERSION_FILE="${REPO_ROOT}/${VERSION_REL}"

# require_file — the version file must exist before it can be read or bumped.
# On beta the absence is not a broken checkout but a CLOSED CYCLE, and the two
# deserve different messages: one is "your tree is wrong", the other is "there
# is nothing to cut, and opening a cycle is the operator's step".
require_file() {
    [ -f "${VERSION_FILE}" ] && return 0
    if [ "${CHANNEL}" = beta ]; then
        echo "✗ no open beta cycle for ${COMP}: ${VERSION_REL} does not exist." >&2
        echo "  The beta BRANCH is permanent and says nothing about whether a cycle is open;" >&2
        echo "  this file is what does (beta.md §3). Opening a cycle is an operator step:" >&2
        echo "      tools/version.sh ${COMP} --seed-beta <X.Y.0>" >&2
    else
        echo "✗ ${VERSION_REL} not found at ${VERSION_FILE}" >&2
    fi
    exit 1
}

read_semver() {
    require_file
    local raw; raw="$(tr -d '\r\n[:space:]' < "${VERSION_FILE}")"
    [[ "${raw}" =~ ^[0-9]+\.[0-9]+\.[0-9]+$ ]] || { echo "✗ ${VERSION_REL} '${raw}' not MAJOR.MINOR.PATCH" >&2; exit 1; }
    printf '%s' "${raw}"
}
# Side-effect: stages the file so the caller (release.sh) can commit/revert it as one unit.
write_semver() { printf '%s\n' "$1" > "${VERSION_FILE}"; ( cd "${REPO_ROOT}" && git add "${VERSION_REL}" ); }

# seed_beta <X.Y.0> — open a cycle by creating versions/<comp>.beta.stamp.
#
# It REFUSES to overwrite. Overwriting is how a cycle silently restarts: the
# line the cycle had climbed to would be replaced by the seed, the next beta cut
# would re-mint a stamp already published, and the catalog would carry two
# different builds under one version. Closing a cycle is a deliberate `git rm`,
# not a re-seed.
#
# The seed must be a .0 patch — beta.md §5's cycle opens at X.Y.0 and climbs
# from there. A seed that is not .0 means the operator typed the line the cycle
# is expected to REACH, not the one it starts at.
seed_beta() {
    local want="${1:-}"
    [ "${CHANNEL}" = beta ] \
        || { echo "✗ --seed-beta only applies to the beta channel (run it with CHANNEL=beta, or without CHANNEL — it sets its own)" >&2; exit 2; }
    [[ "${want}" =~ ^[0-9]+\.[0-9]+\.0$ ]] \
        || { echo "✗ --seed-beta takes a X.Y.0 version (got '${want}') — a cycle opens at the .0 patch and climbs from there (beta.md §5)" >&2; exit 2; }
    if [ -f "${VERSION_FILE}" ]; then
        echo "✗ ${VERSION_REL} already exists ($(tr -d '\r\n[:space:]' < "${VERSION_FILE}")) — a cycle is already open for ${COMP}." >&2
        echo "  Re-seeding would replace the line the cycle has already climbed to, and the next" >&2
        echo "  beta cut would re-mint a stamp that is already published. Closing the cycle is a" >&2
        echo "  deliberate 'git rm ${VERSION_REL}'." >&2
        exit 1
    fi
    printf '%s\n' "${want}" > "${VERSION_FILE}"
    ( cd "${REPO_ROOT}" && git add "${VERSION_REL}" )
    echo "✓ opened the ${COMP} beta cycle at ${want} (${VERSION_REL}, staged)" >&2
    printf '%s\n' "${want}"
}

src_sha() {
    [ -n "${SRC_DIR:-}" ] || { echo "✗ --stamp needs SRC_DIR (the component source worktree)" >&2; exit 2; }
    [ -d "${SRC_DIR}" ]   || { echo "✗ SRC_DIR '${SRC_DIR}' not a directory" >&2; exit 1; }
    git -C "${SRC_DIR}" rev-parse --short=8 HEAD 2>/dev/null \
        || { echo "✗ SRC_DIR '${SRC_DIR}' is not a git worktree" >&2; exit 1; }
}
today_utc() { date -u +%Y.%m.%d; }
# The beta marker is a segment INSIDE the stamp, not a suffix on it: every
# reader (the catalog's channel validation, the bootstrap's channel_tags, core's
# ChannelOf) tells the channels apart by ".beta." being present or absent, and
# the date and hash still have to follow it.
stamp() {
    if [ "${CHANNEL}" = beta ]; then
        printf 'v%s.beta.%s.%s' "$1" "$(today_utc)" "$2"
    else
        printf 'v%s.%s.%s' "$1" "$(today_utc)" "$2"
    fi
}

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
    # --seed-beta sets its own channel: it is meaningless on stable, and asking
    # the operator to spell CHANNEL=beta for a flag with "beta" in its name is
    # a step that only exists to be forgotten.
    --seed-beta)   CHANNEL=beta; VERSION_REL="versions/${COMP}.beta.stamp"
                   VERSION_FILE="${REPO_ROOT}/${VERSION_REL}"; seed_beta "${3:-}" ;;
    -h|--help)     sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//' ;;
    "")            echo "✗ usage: version.sh ${COMP} <--semver|--stamp|--bump-patch|--bump-minor|--bump-major|--seed-beta X.Y.0>" >&2; exit 2 ;;
    *)             echo "✗ unknown action: ${2}" >&2; exit 2 ;;
esac
