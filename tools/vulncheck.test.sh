#!/usr/bin/env bash
# vulncheck.test.sh — unit tests for tools/vulncheck.sh.
# Ported from burrowee-git/release alongside the module-discovery change; clawee
# had the gate but none of its tests, so nothing here was pinned.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=/dev/null
source "${HERE}/vulncheck.sh"

fail=0
check() { # check <label> <got> <want>
    if [ "$2" = "$3" ]; then echo "ok: $1"; else echo "FAIL: $1 — got '$2' want '$3'"; fail=1; fi
}

# --- resolve_release_mode ---------------------------------------------------
check "apple-only"        "$(resolve_release_mode 1 '' '')"  "1|"
check "vulncheck-only"    "$(resolve_release_mode '' 1 '')"  "|1"
check "public (both set)" "$(resolve_release_mode 1 1 '')"   "1|1"
check "prompt yes"        "$(resolve_release_mode '' '' y)"  "1|1"
check "prompt Y"          "$(resolve_release_mode '' '' Y)"  "1|1"
check "prompt no"         "$(resolve_release_mode '' '' n)"  "|"
check "prompt empty"      "$(resolve_release_mode '' '' '')" "|"

# --- vulncheck_scan_dirs (discovery) ----------------------------------------
SCRATCH="$(mktemp -d)"
trap 'rm -rf "${SCRATCH}"' EXIT
SRC_CLAWEE="${SCRATCH}/src-clawee" SRC_CLAWEED="${SCRATCH}/src-claweed"
src_for() { case "$1" in
    clawee)  printf '%s' "$SRC_CLAWEE";;
    claweed) printf '%s' "$SRC_CLAWEED";;
esac; }

# Both components as they are today: one module each.
mkdir -p "${SRC_CLAWEE}" "${SRC_CLAWEED}"
: > "${SRC_CLAWEE}/go.mod"
: > "${SRC_CLAWEED}/go.mod"
COMPONENTS=(clawee claweed)
got="$(vulncheck_scan_dirs | tr '\t' '=' | paste -sd, -)"
check "scan-dirs both components" "${got}" "clawee=${SRC_CLAWEE},claweed=${SRC_CLAWEED}"

# A nested module must appear with NO edit to vulncheck.sh. This is the whole
# point of discovering the set: neither clawee component has a nested module
# today, so a declared list would look correct right up until one does, and the
# gate would report success while shipping it unscanned.
mkdir -p "${SRC_CLAWEED}/probe"
: > "${SRC_CLAWEED}/probe/go.mod"
COMPONENTS=(claweed)
got="$(vulncheck_scan_dirs | tr '\t' '=' | paste -sd, -)"
check "scan-dirs discovers a nested module" "${got}" \
    "claweed=${SRC_CLAWEED},claweed-probe=${SRC_CLAWEED}/probe"

# Nested deeper than one level keeps its full sub-path in the name, so two
# modules can never collide into one report file.
mkdir -p "${SRC_CLAWEED}/tools/gen"
: > "${SRC_CLAWEED}/tools/gen/go.mod"
got="$(vulncheck_scan_dirs | tr '\t' '=' | paste -sd, -)"
check "scan-dirs names a deep nested module by its path" "${got}" \
    "claweed=${SRC_CLAWEED},claweed-probe=${SRC_CLAWEED}/probe,claweed-tools-gen=${SRC_CLAWEED}/tools/gen"
rm -rf "${SRC_CLAWEED}/probe" "${SRC_CLAWEED}/tools"

# vendor/ is not a shipped module — scanning it would report a vendored
# dependency's own go.mod as if it were ours.
mkdir -p "${SRC_CLAWEED}/vendor/example.com/dep"
: > "${SRC_CLAWEED}/vendor/example.com/dep/go.mod"
got="$(vulncheck_scan_dirs | tr '\t' '=' | paste -sd, -)"
check "scan-dirs skips vendor/" "${got}" "claweed=${SRC_CLAWEED}"
rm -rf "${SRC_CLAWEED}/vendor"

# A root carrying a trailing slash must still name its ROOT module plainly.
# The component source dirs are operator-overridable (CLAWEE_SRC_*), so a
# trailing slash is an ordinary way to spell the same path — and dirname never
# produces one, so the root module would otherwise fail to prefix-match itself.
COMPONENTS=(claweed)
src_for() { case "$1" in claweed) printf '%s/' "$SRC_CLAWEED";; esac; }
got="$(vulncheck_scan_dirs | tr '\t' '=' | paste -sd, -)"
check "scan-dirs normalises a trailing-slash root" "${got}" "claweed=${SRC_CLAWEED}"
src_for() { case "$1" in
    clawee)  printf '%s' "$SRC_CLAWEE";;
    claweed) printf '%s' "$SRC_CLAWEED";;
esac; }

# A root with no go.mod is still emitted, so the gate tries it and reports it
# unscannable. Dropping it here would ship an unchecked module while the gate
# reported success.
rm -f "${SRC_CLAWEED}/go.mod"
got="$(vulncheck_scan_dirs | tr '\t' '=' | paste -sd, -)"
check "scan-dirs emits a root with no go.mod" "${got}" "claweed=${SRC_CLAWEED}"
: > "${SRC_CLAWEED}/go.mod"

# --- vulncheck_gate (stubbed govulncheck) -----------------------------------
REPO_ROOT="$(mktemp -d)"
STUB_DIR="$(mktemp -d)"
trap 'rm -rf "${SCRATCH}" "${REPO_ROOT}" "${STUB_DIR}"' EXIT

# clean stub → gate passes
cat > "${STUB_DIR}/govulncheck" <<'STUB'
#!/usr/bin/env bash
exit 0
STUB
chmod +x "${STUB_DIR}/govulncheck"
if VULNCHECK=1 GOVULNCHECK="${STUB_DIR}/govulncheck" bash -c '
    source '"${HERE}"'/vulncheck.sh
    src_for() { [ "$1" = clawee ] && printf %s '"${SRC_CLAWEE}"'; }
    COMPONENTS=(clawee); REPO_ROOT='"${REPO_ROOT}"'
    vulncheck_gate'; then echo "ok: gate clean passes"; else echo "FAIL: gate clean rejected"; fail=1; fi

# finding stub (exit 3) → gate aborts nonzero
cat > "${STUB_DIR}/govulncheck" <<'STUB'
#!/usr/bin/env bash
echo "Vulnerability #1: GO-2099-9999"; exit 3
STUB
chmod +x "${STUB_DIR}/govulncheck"
if VULNCHECK=1 GOVULNCHECK="${STUB_DIR}/govulncheck" bash -c '
    source '"${HERE}"'/vulncheck.sh
    src_for() { [ "$1" = clawee ] && printf %s '"${SRC_CLAWEE}"'; }
    COMPONENTS=(clawee); REPO_ROOT='"${REPO_ROOT}"'
    vulncheck_gate' 2>/dev/null; then echo "FAIL: gate passed a finding"; fail=1; else echo "ok: gate aborts on finding"; fi
[ -s "${REPO_ROOT}/dist/vulncheck/clawee.txt" ] && echo "ok: report written" || { echo "FAIL: no report"; fail=1; }

# Unset VULNCHECK is a no-op, NOT a silent pass of a scan that never ran: the
# gate must not touch govulncheck at all when the cut is not gated.
cat > "${STUB_DIR}/govulncheck" <<'STUB'
#!/usr/bin/env bash
echo "govulncheck must not run when VULNCHECK is unset" >&2; exit 99
STUB
chmod +x "${STUB_DIR}/govulncheck"
if GOVULNCHECK="${STUB_DIR}/govulncheck" bash -c '
    source '"${HERE}"'/vulncheck.sh
    src_for() { [ "$1" = clawee ] && printf %s '"${SRC_CLAWEE}"'; }
    COMPONENTS=(clawee); REPO_ROOT='"${REPO_ROOT}"'
    vulncheck_gate'; then echo "ok: ungated cut skips the scan"; else echo "FAIL: ungated cut ran the scan"; fail=1; fi

[ "${fail}" = 0 ] && echo "ALL OK" || { echo "TESTS FAILED"; exit 1; }
