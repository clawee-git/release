#!/usr/bin/env bash
# render-inner.test.sh — a rendered inner installer that still carries a
# placeholder is FATAL, and leaves nothing behind.
#
# WHY THIS IS ITS OWN SUITE. The guard is the one thing standing between a
# template that grew a name and a kit that installs a daemon under a unit file
# literally called "@SYSTEMD_UNIT@". Both templates live outside this repo's
# control — clawee's is half-owned here and claweed's belongs to the daemon repo
# — so the sed list in render_inner can go stale without this script hearing
# about it, and the failure is silent by construction. cmd/rkit's equivalent
# guard is covered by a Go test; this one covers the SHELL path, which is the
# one a real cut takes.
#
# The function is extracted verbatim with awk, per this repo's convention, so
# the test breaks when render_inner's shape changes rather than testing a stale
# hand-copy.
#
# ASSUMPTION: render_inner's closing brace is alone on a column-0 line and its
# body opens no column-0 "{ … }" group — the awk range stops at the first such
# line.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_SH="${REPO_ROOT}/tools/release.sh"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

GO_BIN="${GO_BIN:-$(command -v go 2>/dev/null || true)}"
[ -x "${GO_BIN}" ] || die "no Go toolchain found (set GO_BIN)"

FUNC="${WORK}/render_inner.sh"
awk '/^render_inner\(\) \{/,/^}/' "${RELEASE_SH}" > "${FUNC}"
grep -q '^render_inner() {' "${FUNC}" || die "could not extract render_inner() from release.sh"
grep -q '^}' "${FUNC}" || die "the extracted render_inner() has no closing brace"

# A fixture repo root: `go run ./cmd/channel-names` resolves from it, so it
# carries the module and this kit's own cmd/ and internal/, and its
# inner/clawee/install.sh.in is the stub under test.
FIX="${WORK}/fixture"
mkdir -p "${FIX}/inner/clawee"
cp "${REPO_ROOT}/go.mod" "${REPO_ROOT}/go.sum" "${FIX}/"
cp -R "${REPO_ROOT}/cmd" "${REPO_ROOT}/internal" "${FIX}/"

# run_render <template-content> — renders the clawee branch into ${DEST} and
# writes render_inner's output to stdout, exiting with its status. claweed is
# not exercised here: its template lives in the daemon repo, which this suite
# must not require on disk.
#
# DEST is set by the CALLER, not in here: every call site is `$(run_render …)`,
# a subshell, and an assignment made inside one is gone by the time the
# assertions read it.
N=0
DEST=""
new_dest() { N=$((N+1)); DEST="${WORK}/out-${N}/install.sh"; mkdir -p "${WORK}/out-${N}"; }
run_render() {
    printf '%s' "$1" > "${FIX}/inner/clawee/install.sh.in"
    set +e
    out="$( REPO_ROOT="${FIX}" GO_BIN="${GO_BIN}" \
        bash -c ". '${FUNC}'; render_inner clawee v0.3.0.beta.2026.09.05.deadbeef beta linux '${DEST}'" 2>&1 )"
    rc=$?
    set -e
    printf '%s' "${out}"
    return "${rc}"
}

# ---- the happy case, so the failure case proves something -------------------
say "a fully substituted template renders"
new_dest
out="$(run_render '#!/bin/sh
BINS="@CLIENT@ @CLIENT_UPDATER@"
')" || die "a renderable template was refused: ${out}"
[ -f "${DEST}" ] || die "the render produced no file"
grep -q 'BINS="claweeb claweeb-updater"' "${DEST}" \
    || die "the beta names were not substituted: $(cat "${DEST}")"
[ "$(stat -f '%Lp' "${DEST}" 2>/dev/null || stat -c '%a' "${DEST}")" = "755" ] \
    || die "the rendered installer is not 0755"
printf '  ✓ claweeb / claweeb-updater, mode 0755\n'

# ---- an unknown placeholder is fatal ----------------------------------------
# This is the whole point: render_inner greps the RENDERED FILE rather than
# trusting its own sed list, so a name nobody here knows about still fails.
say "a template carrying an unknown @PLACEHOLDER@ is refused"
new_dest
set +e
out="$(run_render '#!/bin/sh
BINS="@CLIENT@ @CLIENT_UPDATER@"
UNIT="@SYSTEMD_UNIT@"
')"
rc=$?
set -e
[ "${rc}" -ne 0 ] || die "render_inner shipped an installer carrying @SYSTEMD_UNIT@"
printf '%s' "${out}" | grep -q '@SYSTEMD_UNIT@' \
    || die "the refusal does not name the surviving placeholder:
${out}"
[ -e "${DEST}" ] && die "a refused render left ${DEST} behind — the next step would zip it"
printf '  ✓ exit %d, names @SYSTEMD_UNIT@, no file left behind\n' "${rc}"

# ---- the __UNDERSCORE__ shape too -------------------------------------------
# The daemon's template uses both spellings (__CLAWEED_VERSION__,
# __GATEWAY_FLOOR__), and a guard that only knew @…@ would pass a kit whose
# gateway floor check compared against the string "__GATEWAY_FLOOR__".
say "the __UNDERSCORE__ spelling is caught as well"
new_dest
set +e
out="$(run_render '#!/bin/sh
BINS="@CLIENT@ @CLIENT_UPDATER@"
FLOOR="__GATEWAY_FLOOR__"
')"
rc=$?
set -e
[ "${rc}" -ne 0 ] || die "render_inner shipped an installer carrying __GATEWAY_FLOOR__"
printf '%s' "${out}" | grep -q '__GATEWAY_FLOOR__' \
    || die "the refusal does not name the surviving placeholder:
${out}"
[ -e "${DEST}" ] && die "a refused render left ${DEST} behind"
printf '  ✓ exit %d, names __GATEWAY_FLOOR__, no file left behind\n' "${rc}"

printf '\n✓ render-inner.test.sh passed\n'
