#!/usr/bin/env bash
# test-upgrade-bootstrap.sh — prove the hosted upgrade bootstrap, OFFLINE.
#
#   curl -fsSL https://release.clawee.org/<comp>/upgrade.sh | sh -s -- 0.1.15
#
# <comp>/upgrade.sh is <comp>/install.sh plus one step: same template, same
# baked pubkey, same verify -> unzip -> run the inner installer — and then
# `migrations/upgrade.sh <line>` out of the SAME verified kit. This script
# drives the RENDERED artifact end to end against a fabricated local release
# and asserts the things that can silently be wrong about it:
#
#   1. ORDER + MODE — upgrade.sh runs the installer and THEN the migration; a
#      rendered install.sh runs only the installer. The mode split is real,
#      and the two renders differ in MODE and in nothing else.
#   2. THE ARGUMENT — a malformed or extra argument is refused BEFORE the
#      network, and the <line> handed to the ladder is derived from the
#      RESOLVED TAG (so the kit-level cross-check compares the release
#      actually installed against the kit it came from, not a string against
#      itself).
#   3. THE EXIT MAPPING — the ladder's five-value contract, in which 2 means
#      RUNS HAPPENED and is a SUCCESS. A bootstrap that treats non-zero as
#      failure reports every real upgrade as broken.
#   4. A KIT WITH NO LADDER (missing, or present-but-empty) — refuses, naming
#      the component and the version just installed, rather than succeeding
#      silently. `sh <script>` exits 2 on a script it cannot open (dash, and
#      therefore /bin/sh on Debian/Ubuntu) — the SAME 2 the ladder uses for
#      "rungs ran, success" — so this is a real hazard, not a hypothetical one.
#
# IT NEVER TOUCHES THE WORKING TREE. It copies the template + generator into a
# scratch root and renders THERE with an ephemeral minisign keypair, so a
# failed run leaves nothing behind to restore. Nothing is installed anywhere:
# the "inner installer" and the "ladder" are stubs that append to a log and
# exit with a code the scenario chose.
#
# RUN UNDER EVERY sh-LIKE SHELL ON THE BOX (the rendered bootstrap is POSIX
# sh; macOS /bin/sh is bash 3.2, Debian/Ubuntu's is dash — a shell difference
# has shipped a fatal bug undetected before).
#
# Needs: minisign, zip, unzip, curl, python3, and shasum or sha256sum.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
PORT="${UPGRADE_TEST_PORT:-8842}"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n\xe2\x9c\x97 UPGRADE-BOOTSTRAP TEST FAILED: %b\n' "$*" >&2; exit 1; }
pass() { printf '  OK: %s\n' "$*"; }

# ---- work dir + cleanup -----------------------------------------------------
W="$(mktemp -d "${TMPDIR:-/tmp}/test-upgrade-bootstrap-XXXXXX")"
SERVER_PID=""
cleanup() {
    if [ -n "${SERVER_PID}" ]; then kill "${SERVER_PID}" 2>/dev/null || true; fi
    rm -rf "${W}"
}
trap cleanup EXIT INT TERM

# ---- (0) tools ---------------------------------------------------------------
for t in minisign zip unzip curl python3; do
    command -v "${t}" >/dev/null 2>&1 || die "required tool not found: ${t}"
done
if command -v sha256sum >/dev/null 2>&1; then SUMS="sha256sum"
elif command -v shasum >/dev/null 2>&1; then SUMS="shasum -a 256"
else die "neither sha256sum nor shasum found"; fi

case "$(uname -s)" in Darwin) OS=darwin ;; Linux) OS=linux ;; *) die "unsupported OS $(uname -s)" ;; esac
case "$(uname -m)" in arm64 | aarch64) ARCH=arm64 ;; x86_64 | amd64) ARCH=amd64 ;; *) die "unsupported arch $(uname -m)" ;; esac

COMP=claweed
STAMP="v0.1.15.2026.06.14.86f2a984"
TAG="${COMP}/${STAMP}"
LINE="0.1.15"
ZIP="clawee-${COMP}-${OS}-${ARCH}.zip"

# ---- (1) render into a SCRATCH root ------------------------------------------
say "RENDER: gen-bootstraps.sh into a scratch root (the worktree is not touched)"
mkdir -p "${W}/repo/tools" "${W}/home"
for f in bootstrap.template.sh gen-bootstraps.sh; do
    cp "${REPO_ROOT}/tools/${f}" "${W}/repo/tools/${f}"
done
minisign -G -W -p "${W}/test.pub" -s "${W}/test.key" >/dev/null 2>&1 \
    || die "could not generate an ephemeral minisign keypair"
CLAWEE_PUBKEY_FILE="${W}/test.pub" sh "${W}/repo/tools/gen-bootstraps.sh" >/dev/null \
    || die "gen-bootstraps.sh failed in the scratch root"

INSTALL_SH="${W}/repo/${COMP}/install.sh"
UPGRADE_SH="${W}/repo/${COMP}/upgrade.sh"
[ -f "${UPGRADE_SH}" ] || die "gen-bootstraps.sh rendered no ${COMP}/upgrade.sh — the hosted URL would 404"
pass "rendered ${COMP}/{install,upgrade}.sh under ${W}/repo"

# ---- (2) the two renders differ in MODE and in nothing else that matters ----
say "BAKE: upgrade.sh carries install.sh's trust anchor, and only the mode differs"
baked() { sed -n "s/^$2=\"\\(.*\\)\"\$/\\1/p" "$1"; }
a="$(baked "${INSTALL_SH}" PUBKEY)"
b="$(baked "${UPGRADE_SH}" PUBKEY)"
[ -n "${a}" ] || die "${COMP}/install.sh bakes no PUBKEY"
[ "${a}" = "${b}" ] || die "PUBKEY differs between the two renders: install='${a}' upgrade='${b}'"
[ "$(baked "${INSTALL_SH}" MODE)" = install ] || die "install.sh did not render as MODE=install"
[ "$(baked "${UPGRADE_SH}" MODE)" = upgrade ] || die "upgrade.sh did not render as MODE=upgrade"
diff_lines="$(diff "${INSTALL_SH}" "${UPGRADE_SH}" | grep -c '^[<>]' || true)"
# Every differing line must be a MODE-dependent one — the sole intentional
# substitution point. Each render's own diff line carries the literal word
# "install" or "upgrade" (the baked MODE, and the mode-explaining comments
# beside it) — a divergence anywhere else would show up as a line with
# neither word, and this loop is what actually proves that, not just the
# line count.
other_diff="$(diff "${INSTALL_SH}" "${UPGRADE_SH}" | grep '^[<>]' | grep -vE 'install|upgrade' || true)"
[ -z "${other_diff}" ] \
    || die "install.sh and upgrade.sh differ outside the MODE substitution:\n${other_diff}"
[ "${diff_lines}" -gt 0 ] || die "install.sh and upgrade.sh rendered byte-identical — MODE did not substitute"
pass "same pubkey; every differing line is MODE-dependent (${diff_lines} lines)"

say "SYNTAX: the rendered artifacts parse under every sh on this box"
SHELLS=""
for s in sh dash bash; do
    command -v "${s}" >/dev/null 2>&1 || continue
    SHELLS="${SHELLS} ${s}"
    "${s}" -n "${UPGRADE_SH}" || die "${s} -n failed on the rendered ${COMP}/upgrade.sh"
    "${s}" -n "${INSTALL_SH}" || die "${s} -n failed on the rendered ${COMP}/install.sh"
done
[ -n "${SHELLS}" ] || die "no sh-like shell found"
pass "parsed by:${SHELLS}"

# ---- (3) fabricate a signed local release ------------------------------------
# THREE kits, same stamp, served from three paths: one WITH migrations/upgrade.sh,
# one with no migrations/ at all (clawee's own real shape — it ships no ladder),
# and one whose ladder is PRESENT BUT EMPTY (`sh` on an unopenable/empty script
# exits 2, the ladder's OWN success code for "rungs ran" — the guard that tells
# the two apart is load-bearing, not belt-and-braces).
say "FABRICATE: a signed local release, with / without / and with an empty ladder"
mkdir -p "${W}/kit/migrations" "${W}/nokit"
cat > "${W}/kit/install.sh" <<'INNER'
#!/bin/sh
printf 'install prefix=%s\n' "${CLAWEE_PREFIX:-}" >> "$UB_LOG"
exit "${UB_INSTALL_CODE:-0}"
INNER
cat > "${W}/kit/migrations/upgrade.sh" <<'LADDER'
#!/bin/sh
printf 'migrate argc=%s arg1=%s\n' "$#" "${1:-}" >> "$UB_LOG"
exit "${UB_LADDER_CODE:-0}"
LADDER
# The ledger the bootstrap's line-derivation reads. Its newest target (0.1.3)
# is DELIBERATELY NOT the release line (0.1.15): a release that ships no new
# rung keeps an older ladder top, and the old tag-derived line broke exactly
# there — claweed 0.2.5's kit topped at 0.2.0, migrations/upgrade.sh's equality
# check refused the tag's 0.2.5, and every `upgrade.sh | sh` failed AFTER a
# successful install. The MIGRATED expectation below pins the kit-derived
# value; rows are unordered on purpose (newest is computed, not positional).
cat > "${W}/kit/migrations/run.sh" <<'RUNSH'
#!/bin/sh
MIGRATIONS="
0.1.3 b.sh
0.1.2 a.sh
"
RUNSH
cp "${W}/kit/install.sh" "${W}/nokit/install.sh"
mkdir -p "${W}/emptykit/migrations"
cp "${W}/kit/install.sh" "${W}/emptykit/install.sh"
: > "${W}/emptykit/migrations/upgrade.sh"
# A ladder with no runner: upgrade.sh present and non-empty, migrations/run.sh
# absent — the mis-assembled shape the run.sh guard exists for.
mkdir -p "${W}/norun/migrations"
cp "${W}/kit/install.sh" "${W}/norun/install.sh"
cp "${W}/kit/migrations/upgrade.sh" "${W}/norun/migrations/upgrade.sh"
chmod +x "${W}/kit/install.sh" "${W}/kit/migrations/upgrade.sh" "${W}/nokit/install.sh" \
    "${W}/emptykit/install.sh" "${W}/emptykit/migrations/upgrade.sh" \
    "${W}/norun/install.sh" "${W}/norun/migrations/upgrade.sh"

sign_kit() {
    # sign_kit <src-dir> <serve-subdir> <zip-args...>
    local src="$1" sub="$2"
    shift 2
    mkdir -p "${W}/serve/${sub}"
    ( cd "${src}" && zip -qr "${W}/serve/${sub}/${ZIP}" "$@" ) || die "zip failed for ${sub}"
    ( cd "${W}/serve/${sub}" && ${SUMS} "${ZIP}" > SHA256SUMS.txt ) || die "checksum failed for ${sub}"
    ( cd "${W}/serve/${sub}" \
        && minisign -S -m SHA256SUMS.txt -s "${W}/test.key" \
            -c "clawee test release" -t "clawee ${COMP} ${STAMP}" >/dev/null 2>&1 ) \
        || die "minisign signing failed for ${sub}"
}
sign_kit "${W}/kit" kit install.sh migrations
sign_kit "${W}/nokit" nokit install.sh
sign_kit "${W}/emptykit" emptykit install.sh migrations
sign_kit "${W}/norun" norun install.sh migrations
unzip -Z1 "${W}/serve/kit/${ZIP}" | grep -qx 'migrations/upgrade.sh' \
    || die "the fabricated kit does not actually carry migrations/upgrade.sh — the fixture proves nothing"
unzip -Z1 "${W}/serve/nokit/${ZIP}" | grep -q 'migrations/' \
    && die "the fabricated no-ladder kit carries a migrations/ member"
pass "three signed kits under ${W}/serve"

# ---- (4) serve ----------------------------------------------------------------
say "SERVE: 127.0.0.1:${PORT}"
( cd "${W}/serve" && exec python3 -m http.server "${PORT}" --bind 127.0.0.1 ) >/dev/null 2>&1 &
SERVER_PID=$!
i=0
until curl -fsS "http://127.0.0.1:${PORT}/kit/${ZIP}" -o /dev/null 2>/dev/null; do
    i=$((i + 1))
    [ "${i}" -lt 60 ] || die "http server did not come up on ${PORT}"
    sleep 0.1
done
BASE_URL="http://127.0.0.1:${PORT}"
pass "server up"

# ---- (5) the scenarios ---------------------------------------------------------
RUN_OUT=""
RUN_CODE=0
INSTALL_CODE=0
LADDER_CODE=0
run_boot() {
    # run_boot <shell> <script> <serve-subdir> [args…] — runs the rendered
    # bootstrap with a fresh log and captures BOTH its output and its exit
    # code. The code is read straight off the command substitution, never
    # after a pipe.
    local sh="$1" script="$2" sub="$3"
    shift 3
    : > "${W}/log"
    set +e
    RUN_OUT="$(
        UB_LOG="${W}/log" \
            UB_INSTALL_CODE="${INSTALL_CODE}" \
            UB_LADDER_CODE="${LADDER_CODE}" \
            CLAWEE_GH_PROXY= \
            CLAWEE_DOWNLOADS_BASE= \
            CLAWEE_DL_BASE="${BASE_URL}/${sub}" \
            CLAWEE_CLAWEED_VERSION="${TAG}" \
            HOME="${W}/home" \
            "${sh}" "${script}" "$@" 2>&1
    )"
    RUN_CODE=$?
    set -e
}

RUN_STDOUT=""
RUN_STDERR=""
run_boot_split() {
    # run_boot_split <shell> <script> <serve-subdir> [args…] — like run_boot,
    # but captures stdout and stderr on SEPARATE variables (RUN_STDOUT /
    # RUN_STDERR) instead of merging them with 2>&1. A claim about which
    # stream something lands on (cli-help: usage on stdout exit 0, a refusal
    # on stderr non-zero) is not actually measured by a test that only ever
    # looks at a combined stream — this is what measures it.
    local sh="$1" script="$2" sub="$3"
    shift 3
    : > "${W}/log"
    set +e
    RUN_STDOUT="$(
        UB_LOG="${W}/log" \
            UB_INSTALL_CODE="${INSTALL_CODE}" \
            UB_LADDER_CODE="${LADDER_CODE}" \
            CLAWEE_GH_PROXY= \
            CLAWEE_DOWNLOADS_BASE= \
            CLAWEE_DL_BASE="${BASE_URL}/${sub}" \
            CLAWEE_CLAWEED_VERSION="${TAG}" \
            HOME="${W}/home" \
            "${sh}" "${script}" "$@" 2>"${W}/stderr"
    )"
    RUN_CODE=$?
    RUN_STDERR="$(cat "${W}/stderr")"
    set -e
}

want_code() {
    [ "${RUN_CODE}" = "$1" ] \
        || die "$2: exit ${RUN_CODE}, want $1\n--- output ---\n${RUN_OUT}\n--- log ---\n$(cat "${W}/log")"
}
want_log() {
    local got; got="$(cat "${W}/log")"
    [ "${got}" = "$1" ] \
        || die "$2: log is\n${got}\nwant\n$1\n--- output ---\n${RUN_OUT}"
}
want_out() {
    case "${RUN_OUT}" in
    *"$1"*) : ;;
    *) die "$2: output does not mention '$1'\n--- output ---\n${RUN_OUT}" ;;
    esac
}
want_no_out() {
    case "${RUN_OUT}" in
    *"$1"*) die "$2: output mentions '$1' and must not\n--- output ---\n${RUN_OUT}" ;;
    *) : ;;
    esac
}

INSTALLED="install prefix=${W}/home/.local/bin"
# arg1 is the KIT LEDGER's newest target, not the release line ${LINE} — see
# the fixture run.sh above for why the two deliberately differ here.
MIGRATED="migrate argc=1 arg1=0.1.3"

for SH in ${SHELLS}; do
    say "SCENARIOS under ${SH}"

    # (5a) The mode split is real, not cosmetic.
    INSTALL_CODE=0 LADDER_CODE=0
    run_boot "${SH}" "${INSTALL_SH}" kit
    want_code 0 "[${SH}] install.sh"
    want_log "${INSTALLED}" "[${SH}] install.sh must run ONLY the inner installer"
    pass "[${SH}] install.sh runs the installer and nothing else"

    # (5b) upgrade.sh runs the installer, THEN the migration.
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}"
    want_code 0 "[${SH}] upgrade.sh ${LINE}"
    want_log "${INSTALLED}
${MIGRATED}" "[${SH}] upgrade.sh must run the installer first and the ladder second"
    pass "[${SH}] upgrade.sh: install then migrate, in that order"

    # (5c) The <line> comes from the VERIFIED KIT'S LEDGER — not from the
    # argument, and not from the resolved tag (whose line, 0.1.15, differs
    # from the fixture ladder's 0.1.3 top precisely so a tag-derived value
    # cannot pass). With no argument at all the ladder still gets 0.1.3.
    run_boot "${SH}" "${UPGRADE_SH}" kit
    want_code 0 "[${SH}] upgrade.sh (no argument)"
    want_log "${INSTALLED}
${MIGRATED}" "[${SH}] the ladder's line must be derived from the kit's own ledger"
    pass "[${SH}] the line is derived from the kit's ledger, not the tag or the argument"

    # (5d) THE EXIT MAPPING. 2 = rungs ran = SUCCESS.
    LADDER_CODE=2
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}"
    want_code 0 "[${SH}] ladder exit 2 must be a bootstrap SUCCESS"
    want_out "migration ladder exited 2" "[${SH}] the mapping must be printed"
    pass "[${SH}] ladder 2 (rungs ran) -> bootstrap 0"

    LADDER_CODE=0
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}"
    want_code 0 "[${SH}] ladder exit 0"
    pass "[${SH}] ladder 0 (nothing applied) -> bootstrap 0"

    LADDER_CODE=1
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}"
    want_code 1 "[${SH}] ladder exit 1 must FAIL the bootstrap"
    want_out "migration ladder exited 1" "[${SH}] the mapping must be printed"
    pass "[${SH}] ladder 1 (refused/failed) -> bootstrap 1"

    LADDER_CODE=3
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}"
    want_code 3 "[${SH}] ladder exit 3 must reach the caller"
    pass "[${SH}] ladder 3 (receipt lost) -> bootstrap 3"

    LADDER_CODE=64
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}"
    want_code 64 "[${SH}] ladder exit 64 must reach the caller"
    pass "[${SH}] ladder 64 (usage) -> bootstrap 64"

    # The catch-all rung: an undocumented ladder exit is treated as a
    # failure (MAPPED=1), not silently passed through and not read as 0.
    LADDER_CODE=7
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}"
    want_code 1 "[${SH}] an undocumented ladder exit must map to 1"
    want_out "migration ladder exited 7" "[${SH}] the mapping must be printed"
    pass "[${SH}] ladder 7 (undocumented) -> bootstrap 1 (catch-all)"
    LADDER_CODE=0

    # (5e) The migration runs ONLY if the install succeeded.
    INSTALL_CODE=1
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}"
    [ "${RUN_CODE}" != 0 ] || die "[${SH}] a failed inner installer left the bootstrap green"
    want_log "${INSTALLED}" "[${SH}] the ladder must not run after a failed install"
    pass "[${SH}] a failed installer stops the run before the migration"
    INSTALL_CODE=0

    # (5f) A kit with no ladder: refuse, naming the component and the version.
    # "non-zero + names the component" is NOT enough on its own to prove this:
    # a bootstrap with no check at all would `sh ./migrations/upgrade.sh` into a
    # missing file, exit 127, map that to 1, and satisfy both — while printing a
    # shell error instead of saying what is wrong. So the refusal must also be
    # SPECIFIC, and must arrive BEFORE the ladder is invoked at all, which the
    # absence of the exit-mapping line is the observable proof of.
    run_boot "${SH}" "${UPGRADE_SH}" nokit "${LINE}"
    [ "${RUN_CODE}" != 0 ] || die "[${SH}] a kit with no migrations/upgrade.sh succeeded silently"
    want_out "ships no migrations/upgrade.sh" "[${SH}] the no-ladder refusal must say what is missing"
    want_out "${COMP}" "[${SH}] the no-ladder refusal must name the component"
    want_out "${TAG}" "[${SH}] the no-ladder refusal must name the version just installed"
    want_no_out "migration ladder exited" "[${SH}] the refusal must come BEFORE the ladder is invoked, not from its failure"
    want_log "${INSTALLED}" "[${SH}] the installer still ran before the no-ladder refusal"
    pass "[${SH}] a kit with no ladder refuses, naming the component and version"

    run_boot "${SH}" "${INSTALL_SH}" nokit
    want_code 0 "[${SH}] install.sh against a kit with no ladder is unaffected"
    pass "[${SH}] install.sh does not care that the kit has no ladder"

    run_boot "${SH}" "${UPGRADE_SH}" emptykit "${LINE}"
    [ "${RUN_CODE}" != 0 ] || die "[${SH}] an EMPTY migrations/upgrade.sh was reported as a successful migration (sh exits 2 on a script it cannot read, which is the ladder's success code)"
    want_out "ships no migrations/upgrade.sh" "[${SH}] an unusable ladder must refuse for the same stated reason"
    want_no_out "migration ladder exited" "[${SH}] an unusable ladder must be caught before it is invoked"
    pass "[${SH}] an empty/unreadable ladder refuses instead of passing as exit 2"

    # A kit whose ladder has no RUNNER: upgrade.sh is present, migrations/run.sh
    # is not — the derivation has nothing to read, and invoking the ladder
    # anyway would let ITS missing-runner refusal be reported as a fault of the
    # host instead of the release.
    run_boot "${SH}" "${UPGRADE_SH}" norun "${LINE}"
    [ "${RUN_CODE}" != 0 ] || die "[${SH}] a kit shipping upgrade.sh without migrations/run.sh succeeded"
    want_out "no migrations/run.sh" "[${SH}] the missing-runner refusal must say what is missing"
    want_no_out "migration ladder exited" "[${SH}] the missing runner must be caught before the ladder is invoked"
    pass "[${SH}] a ladder with no run.sh refuses, naming the release as mis-assembled"

    # (5g) THE COMMAND LINE, refused before the network is touched.
    run_boot "${SH}" "${UPGRADE_SH}" kit "${LINE}" extra
    want_code 64 "[${SH}] a second argument must be refused"
    want_log "" "[${SH}] a second argument must be refused before anything runs"
    pass "[${SH}] a second argument is refused before the network"

    # Same case, streams SEPARATED: a refusal is stderr + non-zero, not a
    # merged stream that happens to contain the message somewhere. (stdout is
    # not required to be EMPTY — the startup banner prints there before
    # arguments are validated — only that the refusal text itself is not on
    # it, which is the part cli-help actually cares about.)
    run_boot_split "${SH}" "${UPGRADE_SH}" kit "${LINE}" extra
    want_code 64 "[${SH}] a second argument must be refused (split streams)"
    case "${RUN_STDERR}" in
        *"unexpected extra argument"*) : ;;
        *) die "[${SH}] the refusal message must be on stderr\n--- stdout ---\n${RUN_STDOUT}\n--- stderr ---\n${RUN_STDERR}" ;;
    esac
    case "${RUN_STDOUT}" in
        *"unexpected extra argument"*) die "[${SH}] the refusal message must NOT be on stdout\n--- stdout ---\n${RUN_STDOUT}" ;;
        *) : ;;
    esac
    pass "[${SH}] a refused argument's message prints on stderr, not stdout"

    run_boot "${SH}" "${UPGRADE_SH}" kit "0.1.x"
    want_code 64 "[${SH}] a malformed version must be refused"
    want_log "" "[${SH}] a malformed version must be refused before anything runs"
    pass "[${SH}] a malformed version is refused before the network"

    run_boot "${SH}" "${INSTALL_SH}" kit "${LINE}"
    want_code 64 "[${SH}] install.sh takes no arguments and must reject them"
    want_log "" "[${SH}] install.sh must reject an argument before installing"
    pass "[${SH}] install.sh rejects arguments rather than discarding them"

    # (5h) The argument is checked against the resolved release: a mismatch
    # refuses, before anything is installed, naming both values.
    run_boot "${SH}" "${UPGRADE_SH}" kit "0.9.9"
    [ "${RUN_CODE}" != 0 ] || die "[${SH}] a line that does not match the resolved release was accepted"
    want_out "0.9.9" "[${SH}] the refusal must name what was asked for"
    want_out "${TAG}" "[${SH}] the refusal must name what was resolved"
    want_log "" "[${SH}] nothing may be installed when the line does not match"
    pass "[${SH}] a line/release mismatch refuses before installing anything"

    # (5i) Explicit help is STDOUT + exit 0 — measured on the SPLIT streams,
    # not a 2>&1 merge: a merged capture would pass this even if usage had
    # printed on stderr, which is the actual cli-help rule this is standing in
    # for.
    run_boot_split "${SH}" "${UPGRADE_SH}" kit --help
    want_code 0 "[${SH}] --help must exit 0"
    case "${RUN_STDOUT}" in
        *usage*) : ;;
        *) die "[${SH}] --help must print a usage on stdout\n--- stdout ---\n${RUN_STDOUT}\n--- stderr ---\n${RUN_STDERR}" ;;
    esac
    [ -z "${RUN_STDERR}" ] \
        || die "[${SH}] --help must print nothing on stderr\n--- stderr ---\n${RUN_STDERR}"
    want_log "" "[${SH}] --help must install nothing"
    pass "[${SH}] --help is stdout-only, exit 0, installs nothing"
done

printf '\n\xe2\x9c\x93 UPGRADE-BOOTSTRAP OK (shells:%s)\n' "${SHELLS}"
