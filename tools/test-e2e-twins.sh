#!/usr/bin/env bash
# test-e2e-twins.sh — a beta twin installs BESIDE a stable install and touches
# nothing it owns.
#
# THE CLAIM UNDER TEST is the one the whole feature exists for and the one
# nobody can check by reading: after installing stable and then beta into the
# same tree, both are present and every stable file is byte-for-byte what it
# was. The failure it guards against is silent — a twin whose inner installer
# was rendered for the wrong channel overwrites $BIN_DIR/clawee with a beta
# build, and the operator finds out when their production client changes
# behaviour.
#
# SANDBOX ONLY. PREFIX points at a temp dir; nothing here writes outside it, no
# host is reached, no daemon is installed and no unit is loaded. The daemon half
# of spec §3 (both units loaded, both daemons answering) is deliberately NOT
# here: it needs root, launchd/systemd and a real gateway, which is a disposable
# VM's job and an operator's step — see AGENTS.md "Beta twin". What IS covered
# for claweed is that its kit renders to the beta names, roots, label and unit,
# which is what would make that VM run come out right.
#
# The binaries are stubs. Whether clawee runs is tools/test-e2e.sh's question;
# this one is about WHERE files land and WHAT survives.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

REAL_GO="${GO_BIN:-$(command -v go 2>/dev/null || true)}"
[ -x "${REAL_GO}" ] || die "no Go toolchain found (set GO_BIN)"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

if command -v shasum >/dev/null 2>&1; then SUM="shasum -a 256"; else SUM="sha256sum"; fi

# NO NETWORK. The inner installer ends with a burrowee-cli dependency step that
# curls a bootstrap and pipes it to sh — which, run for real, installs burrowee
# into this sandbox's bin and makes "did the beta install change a stable file"
# unanswerable, besides being a network fetch a test has no business making. A
# refusing curl is the supported shape: the installer treats an unreachable
# release host as a REPORTED note, never a failed install, and that tolerance is
# itself worth exercising.
STUBS="${WORK}/stubs"; mkdir -p "${STUBS}"
printf '#!/bin/sh
exit 7
' > "${STUBS}/curl"; chmod +x "${STUBS}/curl"
PATH="${STUBS}:${PATH}"
export PATH

# render <comp> <channel> <goos> <dest> — the kit's own inner-installer
# rendering, driven through the same table release.sh uses.
names_for() {
    ( cd "${REPO_ROOT}" && "${REAL_GO}" run ./cmd/channel-names "$1" "$2" )
}

# ---- the client: install stable, then beta, into one PREFIX -----------------
say "clawee: a beta twin installs beside stable"

PREFIX="${WORK}/prefix"
BIN="${PREFIX}/bin"
mkdir -p "${BIN}"

install_client() {
    local channel="$1" kit CLIENT CLIENT_UPDATER
    eval "$(names_for "${channel}" "$(uname -s | tr '[:upper:]' '[:lower:]')")"
    kit="${WORK}/kit-${channel}"
    rm -rf "${kit}"; mkdir -p "${kit}"
    # The zip's contents: the twin binaries plus the rendered inner installer.
    for b in "${CLIENT}" "${CLIENT_UPDATER}"; do
        printf '#!/bin/sh\necho "v0.3.0.%s.2026.09.05.deadbeef"\n' "${channel}" > "${kit}/${b}"
        chmod +x "${kit}/${b}"
    done
    sed -e "s|@CLIENT@|${CLIENT}|g" -e "s|@CLIENT_UPDATER@|${CLIENT_UPDATER}|g" \
        "${REPO_ROOT}/inner/clawee/install.sh.in" > "${kit}/install.sh"
    chmod +x "${kit}/install.sh"
    ! grep -qE '@[A-Z_]+@' "${kit}/install.sh" \
        || die "the ${channel} inner installer still carries a placeholder"
    ( cd "${kit}" && PREFIX="${PREFIX}" sh ./install.sh >/dev/null 2>&1 ) \
        || die "the ${channel} inner installer failed"
}

install_client stable
[ -x "${BIN}/clawee" ] && [ -x "${BIN}/clawee-updater" ] \
    || die "the stable install did not place its binaries: $(ls -A "${BIN}")"

# Checksum EVERY stable file before the beta install, so "untouched" is a
# measurement and not an inspection of the two names we happened to think of.
BEFORE="${WORK}/before.sums"
( cd "${BIN}" && ${SUM} ./* | sort > "${BEFORE}" )
printf '  stable installed: %s\n' "$(ls -A "${BIN}" | tr '\n' ' ')"

install_client beta

[ -x "${BIN}/claweeb" ] && [ -x "${BIN}/claweeb-updater" ] \
    || die "the beta install did not place its binaries: $(ls -A "${BIN}")"
[ -x "${BIN}/clawee" ] && [ -x "${BIN}/clawee-updater" ] \
    || die "the beta install REMOVED a stable binary: $(ls -A "${BIN}")"

AFTER="${WORK}/after.sums"
( cd "${BIN}" && ${SUM} ./clawee ./clawee-updater | sort > "${AFTER}" )
diff "${BEFORE}" "${AFTER}" >/dev/null \
    || die "the beta install changed a stable file:
$(diff "${BEFORE}" "${AFTER}")"
printf '  ✓ both present, stable checksums unchanged\n'
printf '    %s\n' "$(ls -A "${BIN}" | tr '\n' ' ')"

# ---- uninstalling the twin leaves stable alone ------------------------------
# The reverse direction is the same defect read backwards: an uninstall that
# removed the stable binaries would take a production client down while
# "removing the beta soak build".
say "uninstalling the twin leaves stable alone"
eval "$(names_for beta "$(uname -s | tr '[:upper:]' '[:lower:]')")"
( cd "${WORK}/kit-beta" && PREFIX="${PREFIX}" CLAWEE_UNINSTALL=1 sh ./install.sh >/dev/null ) \
    || die "the beta uninstall failed"
[ -e "${BIN}/claweeb" ] && die "the beta uninstall left claweeb behind"
( cd "${BIN}" && ${SUM} ./clawee ./clawee-updater | sort > "${AFTER}" )
diff "${BEFORE}" "${AFTER}" >/dev/null \
    || die "the beta uninstall touched a stable file:
$(diff "${BEFORE}" "${AFTER}")"
printf '  ✓ twin gone, stable checksums unchanged\n'

# ---- the daemon: no path, name, label or unit is shared ---------------------
# The daemon installer is not RUN here (root, launchd/systemd, a gateway). What
# is checked is the rendering it would run with — the one root the first beta
# kit got wrong was SYSTEM_BIN, and doctor refused on every beta host naming a
# path nothing had created.
say "claweed: the twin's roots and identities are disjoint from stable's"
for goos in darwin linux; do
    s_names="$(names_for stable "${goos}")"
    b_names="$(names_for beta "${goos}")"
    for key in DAEMON DAEMON_UPDATER LABEL SYSTEMD_UNIT SYSTEM_ROOT SYSTEM_ETC SYSTEM_BIN RUN_DIR; do
        sv="$(printf '%s\n' "${s_names}" | sed -n "s/^${key}='\(.*\)'$/\1/p")"
        bv="$(printf '%s\n' "${b_names}" | sed -n "s/^${key}='\(.*\)'$/\1/p")"
        [ -n "${sv}" ] && [ -n "${bv}" ] || die "${key} is empty for ${goos}"
        [ "${sv}" != "${bv}" ] || die "${goos}: ${key} is ${sv} on BOTH channels"
    done
    # And beta's roots nest UNDER stable's rather than beside a differently
    # named sibling — burrowee's side-by-side design, and what the daemon's own
    # canonicalRootClaweedBin() answers.
    s_root="$(printf '%s\n' "${s_names}" | sed -n "s/^SYSTEM_ROOT='\(.*\)'$/\1/p")"
    b_root="$(printf '%s\n' "${b_names}" | sed -n "s/^SYSTEM_ROOT='\(.*\)'$/\1/p")"
    [ "${b_root}" = "${s_root}/beta" ] || die "${goos}: beta root ${b_root} is not ${s_root}/beta"
    printf '  ✓ %s: %s vs %s\n' "${goos}" "${s_root}" "${b_root}"
done

printf '\n✓ test-e2e-twins PASSED (sandbox only — no host touched)\n'
