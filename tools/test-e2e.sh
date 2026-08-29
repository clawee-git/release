#!/usr/bin/env bash
# test-e2e.sh — prove the whole release chain OFFLINE with the TEST key.
#
# No GitHub, no nsm, no real signing key. For each requested component
# (clawee | claweed | all) this:
#   1. dry-run-builds the release (signed by the TEST key) into dist/<stamp>/.
#   2. regenerates the outer bootstraps (baking the TEST pubkey).
#   3. runs verify-no-env on the freshly built binaries.
#   4. HAPPY PATH:
#        clawee   — serves dist/<stamp>/ over http and runs the REAL outer
#                   bootstrap against it; asserts the installed clawee reports
#                   the expected stamp (the burrowee-cli dependency step is
#                   exercised but tolerant — it's already installed on this box,
#                   or skipped if release.burrowee.com is unreachable).
#        claweed  — drives the SAME verification primitives the outer bootstrap
#                   uses (minisign -V → sha256 -c → unzip), then asserts the
#                   verified inner installer is the CANONICAL daemon template and
#                   both binaries (claweed + claweed-updater) are present and
#                   report the stamp — and that the zip's ENTRY SET is exactly
#                   those two plus install.sh (so the retired setuid
#                   clawee-spawn helper cannot return under any name), with no
#                   live setuid step inside. It does NOT execute the
#                   side-effecting daemon installer (sudo, launchd/systemd,
#                   gateway curl) — those are operator-gated steps proven in
#                   Phase C live re-install, not in an offline harness.
#   5. TAMPER PATH: flips one byte inside the zip and asserts the verification
#        gate ABORTS non-zero AND installs nothing.
#
# Exits 0 only if every requested component prints "HAPPY-PATH OK" and
# "TAMPER-ABORTED OK".
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# go on PATH (the Clawee per-dir hook strips /opt/homebrew/bin) ----------------
GO_BIN="${GO_BIN:-go}"
command -v "${GO_BIN}" >/dev/null 2>&1 || GO_BIN=/opt/homebrew/bin/go
export GO_BIN

# component source dirs — build from main checkout --------------------------------
export CLAWEE_SRC_CLAWEE="${CLAWEE_SRC_CLAWEE:-/Volumes/MacintoshED/Workstation/Coding/Clawee/cli/code/main}"
export CLAWEE_SRC_CLAWEED="${CLAWEE_SRC_CLAWEED:-/Volumes/MacintoshED/Workstation/Coding/Clawee/daemon/code/main}"

WHAT="${1:-all}"
case "${WHAT}" in
    clawee|claweed|all) ;;
    -h|--help) sed -n '2,40p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
    *) echo "✗ usage: test-e2e.sh <clawee|claweed|all>" >&2; exit 2 ;;
esac
if [ "${WHAT}" = all ]; then COMPONENTS=(clawee claweed); else COMPONENTS=("${WHAT}"); fi

PORT="${E2E_PORT:-8741}"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ E2E FAILED: %s\n' "$*" >&2; exit 1; }

# minisign / sha256 verifiers (mirror the outer bootstrap's primitives)
command -v minisign >/dev/null 2>&1 || die "minisign not found (brew install minisign)"
if command -v shasum >/dev/null 2>&1; then SHA_C="shasum -a 256 -c --ignore-missing";
elif command -v sha256sum >/dev/null 2>&1; then SHA_C="sha256sum -c --ignore-missing";
else die "neither shasum nor sha256sum found"; fi
TEST_PUB="${REPO_ROOT}/tools/testkeys/test.pub"
[ -f "${TEST_PUB}" ] || die "TEST pubkey missing: ${TEST_PUB} (minisign -G -p tools/testkeys/test.pub -s tools/testkeys/test.key)"
PUBKEY_LINE="$(grep -v '^untrusted comment:' "${TEST_PUB}" | grep -v '^[[:space:]]*$' | tail -n1)"

# host os/arch (the zip the bootstrap requests on this box)
case "$(uname -s)" in Darwin) OS=darwin ;; Linux) OS=linux ;; *) die "unsupported OS $(uname -s)" ;; esac
case "$(uname -m)" in arm64|aarch64) ARCH=arm64 ;; x86_64|amd64) ARCH=amd64 ;; *) die "unsupported arch $(uname -m)" ;; esac

SERVER_PID=""
cleanup() { [ -n "${SERVER_PID}" ] && kill "${SERVER_PID}" 2>/dev/null || true; }
trap cleanup EXIT INT TERM

# render the outer bootstraps once (TEST pubkey) ------------------------------
say "gen-bootstraps.sh (bake TEST pubkey)"
CLAWEE_PUBKEY_FILE="${TEST_PUB}" sh tools/gen-bootstraps.sh

run_component() {
    local comp="$1" src zip stamp serve_dir pin
    case "${comp}" in
        clawee)   src="${CLAWEE_SRC_CLAWEE}" ;;
        claweed)  src="${CLAWEE_SRC_CLAWEED}" ;;
    esac

    say "release.sh ${comp} --dry-run (TEST-key signed build)"
    bash tools/release.sh "${comp}" --dry-run

    stamp="$(SRC_DIR="${src}" bash tools/version.sh "${comp}" --stamp)"
    serve_dir="${REPO_ROOT}/dist/${stamp}"
    [ -d "${serve_dir}" ] || die "expected dist dir not found: ${serve_dir}"
    pin="${comp}/${stamp}"
    zip="clawee-${comp}-${OS}-${ARCH}.zip"
    [ -f "${serve_dir}/${zip}" ] || die "host zip not present: ${serve_dir}/${zip}"
    say "${comp} stamp = ${stamp}  (pin = ${pin})"

    # ---- verify-no-env on the freshly built binaries (unzip a copy) ---------
    local envchk; envchk="$(mktemp -d)"
    unzip -q -o "${serve_dir}/${zip}" -d "${envchk}"
    case "${comp}" in
        clawee)   "${REPO_ROOT}/tools/verify-no-env.sh" "${envchk}/clawee" ;;
        claweed)  "${REPO_ROOT}/tools/verify-no-env.sh" "${envchk}/claweed" "${envchk}/claweed-updater" ;;
    esac
    rm -rf "${envchk}"
    echo "ENV-GUARD OK (${comp})"

    if [ "${comp}" = clawee ]; then
        run_clawee "${comp}" "${serve_dir}" "${zip}" "${stamp}" "${pin}"
    else
        run_claweed "${comp}" "${serve_dir}" "${zip}" "${stamp}"
    fi
}

# ----- clawee: real outer bootstrap against a local http server -------------
run_clawee() {
    local comp="$1" serve_dir="$2" zip="$3" stamp="$4" pin="$5"
    local happy="${TMPDIR:-/tmp}/e2e-${comp}-prefix" tamper="${TMPDIR:-/tmp}/e2e-${comp}-prefix-tamper"
    rm -rf "${happy}" "${tamper}"

    say "serving ${serve_dir} on 127.0.0.1:${PORT}"
    ( cd "${serve_dir}" && exec python3 -m http.server "${PORT}" --bind 127.0.0.1 ) >/dev/null 2>&1 &
    SERVER_PID=$!
    local i=0
    until curl -fsS "http://127.0.0.1:${PORT}/${zip}" -o /dev/null 2>/dev/null; do
        i=$((i+1)); [ "${i}" -lt 50 ] || die "http server did not come up on ${PORT}"
        sleep 0.1
    done
    say "server up (serving ${zip})"

    local dl_base="http://127.0.0.1:${PORT}"
    run_install() {
        CLAWEE_DL_BASE="${dl_base}" \
        CLAWEE_VERSION="${pin}" \
        PREFIX="$1" \
            sh "${REPO_ROOT}/${comp}/install.sh"
    }

    say "HAPPY PATH — install into ${happy}"
    run_install "${happy}" || die "happy-path install exited non-zero (expected success)"
    local bin="${happy}/bin/clawee"
    [ -x "${bin}" ] || die "clawee not installed at ${bin}"
    local got; got="$("${bin}" --version 2>&1 || true)"
    say "installed clawee version → ${got}"
    case "${got}" in
        *"${stamp}"*) printf '\nHAPPY-PATH OK (%s)\n' "${comp}" ;;
        *) die "version mismatch: expected stamp '${stamp}' in output, got: ${got}" ;;
    esac

    say "TAMPER PATH — flip one byte inside the served ${zip}"
    local zip_path="${serve_dir}/${zip}" backup="${serve_dir}/${zip}.orig"
    cp "${zip_path}" "${backup}"
    python3 - "${zip_path}" <<'PY'
import sys
p = sys.argv[1]; off = 256
with open(p, "r+b") as f:
    f.seek(off); b = f.read(1)
    if not b: raise SystemExit("zip too small to tamper at offset %d" % off)
    f.seek(off); f.write(bytes([b[0] ^ 0xFF]))
print("flipped byte at offset %d (0x%02x -> 0x%02x)" % (off, b[0], b[0] ^ 0xFF))
PY
    say "TAMPER PATH — rerun the SAME install into ${tamper} (must abort)"
    set +e
    run_install "${tamper}"
    local rc=$?
    set -e
    mv -f "${backup}" "${zip_path}"
    [ "${rc}" -ne 0 ] || die "tampered install returned 0 — verification gate FAILED to abort"
    [ ! -e "${tamper}/bin/clawee" ] || die "tampered install left a binary — must install nothing"
    say "tampered install aborted with rc=${rc} and installed nothing"
    printf '\nTAMPER-ABORTED OK (%s)\n' "${comp}"

    kill "${SERVER_PID}" 2>/dev/null || true; SERVER_PID=""
}

# ----- claweed: verify the trust chain WITHOUT side-effecting install -------
run_claweed() {
    local comp="$1" serve_dir="$2" zip="$3" stamp="$4"
    local sums="${serve_dir}/SHA256SUMS.txt" sig="${serve_dir}/SHA256SUMS.txt.minisig"
    [ -f "${sums}" ] && [ -f "${sig}" ] || die "missing SHA256SUMS/sig in ${serve_dir}"

    say "HAPPY PATH (${comp}) — minisign -V → sha256 -c → unzip → assert canonical inner + binaries"
    minisign -V -P "${PUBKEY_LINE}" -m "${sums}" -x "${sig}" >/dev/null \
        || die "minisign signature verification failed on a good release"
    grep -qF "${zip}" "${sums}" || die "no checksum entry for ${zip}"
    # shellcheck disable=SC2086  # ${SHA_C} is an intentional space-split command string; word-splitting is the point.
    ( cd "${serve_dir}" && ${SHA_C} SHA256SUMS.txt >/dev/null ) || die "checksum mismatch on a good release"

    local x; x="$(mktemp -d)"
    unzip -q -o "${serve_dir}/${zip}" -d "${x}" || die "unzip failed"
    [ -f "${x}/install.sh" ] || die "release zip missing inner install.sh"
    # Assert the inner installer IS the canonical daemon template (sentinel lines).
    grep -q 'claweed installer' "${x}/install.sh"      || die "inner install.sh is not the claweed installer"
    grep -q 'SUDO-MINIMAL' "${x}/install.sh"           || die "inner install.sh missing the sudo-minimal model marker"
    grep -qF "${stamp}" "${x}/install.sh"              || die "inner install.sh not version-stamped with ${stamp}"
    # claweed and claweed-updater both answer `--version`.
    for pair in "claweed --version" "claweed-updater --version"; do
        b="${pair%% *}"; vflag="${pair#* }"
        [ -x "${x}/${b}" ] || die "release zip missing executable ${b}"
        got="$("${x}/${b}" "${vflag}" 2>&1 || true)"
        case "${got}" in *"${stamp}"*) ;; *) die "${b} version mismatch: want stamp ${stamp}, got: ${got}" ;; esac
    done
    # The setuid-root clawee-spawn helper is RETIRED — the daemon runs as root and
    # forks its own per-user children. Two assertions guard that.
    #
    # (a) The zip's entry SET, not the absence of one name. Checking only for
    # "clawee-spawn" would pass for the same helper re-added under any other
    # name; an exact set fails on any addition at all, and forces whoever adds a
    # binary to come here and say so.
    local got_entries want_entries
    # shellcheck disable=SC2012  # entries are OUR release artifacts (plain ASCII names); ls is what makes the want-string readable, and a surprising name is exactly what this assertion should fail on.
    got_entries="$(cd "${x}" && ls -A | LC_ALL=C sort | tr '\n' ' ')"
    want_entries="claweed claweed-updater install.sh migrations "
    [ "${got_entries}" = "${want_entries}" ] \
        || die "claweed zip entry set changed: got [${got_entries}] want [${want_entries}]"
    # (a2) migrations/ is not decoration: install.sh calls
    # "$SELF_DIR/migrations/run.sh" and REFUSES the install when it is absent, so
    # a kit without it is uninstallable rather than degraded. v0.2.0.2026.08.13
    # shipped exactly that — three top-level files, no ladder — because the
    # release assembles its own zips and only the daemon's build-local.sh staging
    # was guarded. Assert the runner is present, executable, and accompanied by
    # the file every rung sources; and that no test harness rode along.
    [ -x "${x}/migrations/run.sh" ] \
        || die "claweed zip has migrations/ but no executable migrations/run.sh"
    [ -f "${x}/migrations/lib.sh" ] \
        || die "claweed zip is missing migrations/lib.sh — every rung sources it, so no rung can run"
    if ls "${x}/migrations/"*_test.sh >/dev/null 2>&1; then
        die "claweed zip ships a migrations test harness (*_test.sh) — release kits carry the ladder, not its suite"
    fi
    #
    # (b) No LIVE setuid step in the shipped inner installer. Only EXECUTABLE
    # lines are read: the canonical template still explains the retirement in
    # comments that name mode 4755, and that history is worth keeping (no live
    # POSIX-sh statement can begin with #, so stripping whole-line comments
    # cannot hide one). The match covers `chmod …+s` and any -m/--mode with the
    # setuid/setgid bit set (4755, 6755, 4711, …), not just the one literal.
    #
    # This is a TEXT check on a shipped artifact, so it is a tripwire, not a
    # proof: a mode computed at runtime would slip past it. It also runs only
    # when an operator runs test-e2e.sh — `grep -n e2e tools/release.sh
    # cmd/rkit/*.go` finds nothing, so neither cut path invokes it. Assertion (a)
    # is the load-bearing one.
    if grep -v '^[[:space:]]*#' "${x}/install.sh" \
        | grep -Eq 'chmod[^;|&]*\+s|(-m|--mode)[= ]*0*[4-7][0-7]{3}'; then
        die "inner install.sh has a live setuid/setgid step — the spawn tier was retired"
    fi
    rm -rf "${x}"
    printf '\nHAPPY-PATH OK (%s)\n' "${comp}"

    say "TAMPER PATH (${comp}) — flip one byte; minisign/sha must reject"
    local zip_path="${serve_dir}/${zip}" backup="${serve_dir}/${zip}.orig"
    cp "${zip_path}" "${backup}"
    python3 - "${zip_path}" <<'PY'
import sys
p = sys.argv[1]; off = 256
with open(p, "r+b") as f:
    f.seek(off); b = f.read(1)
    if not b: raise SystemExit("zip too small to tamper at offset %d" % off)
    f.seek(off); f.write(bytes([b[0] ^ 0xFF]))
print("flipped byte at offset %d" % off)
PY
    # The sums file still verifies (untouched), but the zip's sha no longer matches.
    set +e
    ( cd "${serve_dir}" && ${SHA_C} SHA256SUMS.txt >/dev/null 2>&1 )
    local rc=$?
    set -e
    mv -f "${backup}" "${zip_path}"
    [ "${rc}" -ne 0 ] || die "tampered ${zip} still passed sha256 -c — verification gate FAILED"
    say "tampered ${zip} correctly failed checksum (rc=${rc})"
    printf '\nTAMPER-ABORTED OK (%s)\n' "${comp}"
}

for comp in "${COMPONENTS[@]}"; do
    run_component "${comp}"
done

printf '\n✓ E2E PASSED (%s) — happy path + tamper-abort\n' "${WHAT}"
