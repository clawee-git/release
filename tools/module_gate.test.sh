#!/usr/bin/env bash
# module_gate.test.sh — unit tests for tools/module_gate.sh.
#
# Exercises module_gate() directly against a throwaway REPO_ROOT holding stub
# suites, so a suite's pass/fail is something the test decides rather than
# something it inherits from the real tools/. NO part of the release path runs:
# release.sh is never invoked, nothing is built, signed or published, and the
# real tools/test-*.sh are never executed.
#
# module_gate() aborts with `exit 1`, so every case runs it in a subshell and
# reads the subshell's status — asserting on exit codes the caller (release.sh)
# actually sees.
set -uo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

fail=0
check() { # check <label> <got> <want>
    if [ "$2" = "$3" ]; then echo "ok: $1"; else echo "FAIL: $1 — got '$2' want '$3'"; fail=1; fi
}
check_contains() { # check_contains <label> <haystack> <needle>
    case "$2" in
        *"$3"*) echo "ok: $1" ;;
        *) echo "FAIL: $1 — output does not contain '$3'"; fail=1 ;;
    esac
}
check_lacks() { # check_lacks <label> <haystack> <needle>
    case "$2" in
        *"$3"*) echo "FAIL: $1 — output unexpectedly contains '$3'"; fail=1 ;;
        *) echo "ok: $1" ;;
    esac
}

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# fake_repo <suite:exit>… — a REPO_ROOT whose tools/ holds one stub per named
# suite, each exiting with the given code and printing an identifiable line.
fake_repo() {
    local root="${WORK}/repo.$$.${RANDOM}" spec name code
    mkdir -p "${root}/tools"
    for spec in "$@"; do
        name="${spec%%:*}"; code="${spec##*:}"
        { echo '#!/bin/sh'
          echo "echo 'ran ${name}'"
          echo "echo 'detail line from ${name}' >&2"
          echo "exit ${code}"
        } > "${root}/tools/${name}"
        chmod 0755 "${root}/tools/${name}"
    done
    printf '%s\n' "${root}"
}

ALL_GREEN=(test-modules.sh:0 test-checksum-verify.sh:0 sync-modules.test.sh:0)

# ---- all three suites green -------------------------------------------------
root="$(fake_repo "${ALL_GREEN[@]}")"
out="$( REPO_ROOT="${root}"; source "${HERE}/module_gate.sh"; module_gate 2>&1 )"; rc=$?
check "all green: exit 0" "${rc}" "0"
# A passing suite's own chatter is swallowed — one ✓ line per suite is the
# contract, the same shape vulncheck_gate keeps, so a clean cut's log stays
# readable. The ✓ lines are therefore what proves each suite actually ran.
check_contains "all green: test-modules.sh ran and passed"        "${out}" "✓ module gate: test-modules.sh clean"
check_contains "all green: test-checksum-verify.sh ran and passed" "${out}" "✓ module gate: test-checksum-verify.sh clean"
check_contains "all green: sync-modules.test.sh ran and passed"   "${out}" "✓ module gate: sync-modules.test.sh clean"
check_lacks "all green: a passing suite's output is not echoed"   "${out}" "detail line from test-modules.sh"

# ---- a red suite aborts the cut ---------------------------------------------
# The point of the whole file: a non-zero suite must stop the cut, not warn.
root="$(fake_repo test-modules.sh:1 test-checksum-verify.sh:0 sync-modules.test.sh:0)"
out="$( REPO_ROOT="${root}"; source "${HERE}/module_gate.sh"; module_gate 2>&1 )"; rc=$?
check "red test-modules.sh: exit 1"           "${rc}" "1"
check_contains "red test-modules.sh: says which suite" "${out}" "✗ module gate: test-modules.sh failed"
check_contains "red test-modules.sh: carries the suite's own output" "${out}" "detail line from test-modules.sh"

# ---- a red suite stops the ones after it ------------------------------------
# Failing fast matters: the later suites are slower, and the first red one is
# already enough to refuse the cut.
check_lacks "red test-modules.sh: later suite did not run" "${out}" "ran test-checksum-verify.sh"

# ---- the GENERATOR hint appears only for test-modules.sh --------------------
check_contains "red test-modules.sh: points at the dirty tree" "${out}" "git diff"
root="$(fake_repo test-modules.sh:0 test-checksum-verify.sh:1 sync-modules.test.sh:0)"
out="$( REPO_ROOT="${root}"; source "${HERE}/module_gate.sh"; module_gate 2>&1 )"; rc=$?
check "red checksum suite: exit 1" "${rc}" "1"
check_contains "red checksum suite: names itself" "${out}" "✗ module gate: test-checksum-verify.sh failed"
check_lacks "red checksum suite: no GENERATOR hint" "${out}" "git diff"

# ---- a red third suite is still caught --------------------------------------
# sync-modules.test.sh is last; a gate that only ever checked the first suite
# would pass this repo, so pin it explicitly.
root="$(fake_repo test-modules.sh:0 test-checksum-verify.sh:0 sync-modules.test.sh:1)"
out="$( REPO_ROOT="${root}"; source "${HERE}/module_gate.sh"; module_gate 2>&1 )"; rc=$?
check "red sync-modules.test.sh: exit 1" "${rc}" "1"
check_contains "red sync-modules.test.sh: names itself" "${out}" "✗ module gate: sync-modules.test.sh failed"

# ---- a missing suite is a failure, not a silent skip ------------------------
# The failure mode this guards: someone renames or drops a suite and the gate
# quietly stops covering it while still printing success.
root="$(fake_repo test-checksum-verify.sh:0 sync-modules.test.sh:0)"
out="$( REPO_ROOT="${root}"; source "${HERE}/module_gate.sh"; module_gate 2>&1 )"; rc=$?
check "missing suite: exit 1" "${rc}" "1"
check_contains "missing suite: names the missing file" "${out}" "test-modules.sh is missing"

# ---- the real repo wires exactly the green set ------------------------------
# Pins the decision, so adding a suite to release.sh's gate without deciding it
# here fails the build.
gate_list="$(sed -n 's/.*for suite in \(.*\); do.*/\1/p' "${HERE}/module_gate.sh")"
check "wired set is the green set" "${gate_list}" "test-modules.sh test-checksum-verify.sh sync-modules.test.sh"
# test-upgrade-bootstrap.sh passes in this repo and is still excluded: the set is
# identical in all three release repos on purpose, and widening it here is how
# they drift apart. See the header of module_gate.sh.
for red in test-upgrade-bootstrap.sh test-e2e.sh sync-modules.sh; do
    check_lacks "not wired: ${red}" " ${gate_list} " " ${red} "
done

# ---- release.sh calls it, before the first artifact -------------------------
# The finding this whole task exists for was six gates that passed and were
# never invoked. A gate nothing calls is the same bug wearing a new name.
rel="${HERE}/release.sh"
if [ -f "${rel}" ]; then
    check_contains "release.sh sources the gate" "$(cat "${rel}")" 'source "${REPO_ROOT}/tools/module_gate.sh"'
    gate_line="$(grep -n '^module_gate$' "${rel}" | cut -d: -f1)"
    vuln_line="$(grep -n '^vulncheck_gate$' "${rel}" | cut -d: -f1)"
    check "release.sh calls module_gate exactly once" "$(grep -c '^module_gate$' "${rel}")" "1"
    if [ -n "${gate_line}" ] && [ -n "${vuln_line}" ]; then
        if [ "${gate_line}" -lt "${vuln_line}" ]; then
            echo "ok: module_gate runs before vulncheck_gate (both pre-build)"
        else
            echo "FAIL: module_gate must run before vulncheck_gate"; fail=1
        fi
    else
        echo "FAIL: could not locate module_gate/vulncheck_gate call sites in release.sh"; fail=1
    fi
else
    echo "FAIL: release.sh not found beside module_gate.sh"; fail=1
fi

echo
[ "${fail}" = 0 ] && { echo "ALL OK"; exit 0; }
echo "FAILURES"; exit 1
