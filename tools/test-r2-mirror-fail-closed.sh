#!/usr/bin/env bash
# test-r2-mirror-fail-closed.sh — unit-level pin for mirror_to_r2's fail-closed
# behavior (2026-08-20 incident: a FAILED R2 upload warned and returned 0,
# letting the distribute continue on a stale catalog — gen-version-jsonp.sh
# then read latest.json and published a WITHDRAWN stamp's version.js. See
# tools/release.sh's mirror_to_r2 doc comment).
#
# WHY a unit test on the function, not an end-to-end `release.sh
# --distribute-only` run: past the R2 mirror, distribute_only does a real
# `git tag` + `ghp release create` + a marker `git commit` against REPO_ROOT.
# There is no throwaway git repo to run that in here without either polluting
# this repo's real tags/commits or fabricating a full parallel checkout
# (versions/, install templates, gen-bootstraps/gen-version-jsonp inputs, …).
# tools/test-e2e.sh sidesteps the same problem by staying entirely on the
# `--dry-run` path, which never reaches mirror_to_r2 at all (its own doc
# comment: "Never called under --dry-run"). This test instead pins the exact
# unit that changed — mirror_to_r2's three-way branch — extracted verbatim
# from release.sh with awk (not hand-copied), so it breaks the moment that
# function's shape changes instead of silently testing a stale fork of it.
#
# Three cases, each in a fresh subshell so env never leaks between them:
#   1. FAILED  (r2_account_id set, creds file present, r2-mirror exits non-zero)
#      → mirror_to_r2 must return non-zero and its stderr must name: the
#        GitHub release is already published, what did NOT run, and the
#        by-hand recovery (tools/r2-mirror with the same args).
#   2. SKIP — no r2_account_id in config → must still return 0 (deliberate
#      no-mirror posture; GitHub stays primary).
#   3. SKIP — creds file absent → must still return 0.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_SH="${REPO_ROOT}/tools/release.sh"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# ---- extract toml_get() + mirror_to_r2() verbatim from release.sh -----------
# ASSUMPTION: each function's closing brace is alone on a column-0 line ("}")
# and nothing inside the function body opens a column-0 "{ ... }" group of its
# own — the awk range stops at the FIRST such line, so a future edit that adds
# one inside either function would silently truncate the extraction. The
# `^toml_get() {` / `^mirror_to_r2() {` grep guards below only catch a missing
# or renamed opening line, not a truncated body.
FUNCS="${WORK}/funcs.sh"
{
    awk '/^toml_get\(\) \{/,/^}/' "${RELEASE_SH}"
    echo
    awk '/^mirror_to_r2\(\) \{/,/^}/' "${RELEASE_SH}"
} > "${FUNCS}"
grep -q '^toml_get() {' "${FUNCS}" \
    || die "extraction found no toml_get() in ${RELEASE_SH} — release.sh's shape changed, update this test"
grep -q '^mirror_to_r2() {' "${FUNCS}" \
    || die "extraction found no mirror_to_r2() in ${RELEASE_SH} — release.sh's shape changed, update this test"

# fake GO_BIN standing in for `go run .` inside tools/r2-mirror. Only case 1
# ever reaches it (the SKIP cases return before calling it) and it always
# fails — this test is about mirror_to_r2's branching, not a working mirror.
FAKE_GO="${WORK}/fake-go"
cat > "${FAKE_GO}" <<'SH'
#!/usr/bin/env bash
exit 1
SH
chmod +x "${FAKE_GO}"

# run_case: source the extracted funcs into a FRESH subshell and call
# mirror_to_r2 there. REPO_ROOT/GO_BIN/CLAWEE_R2_CONFIG/CLAWEE_R2_CREDS come
# from the caller's environment (bash exports var=val prefixes to a function
# call, which the nested subshell then inherits).
run_case() {
    ( set -euo pipefail
      # shellcheck source=/dev/null
      source "${FUNCS}"
      R2_CONFIG="${CLAWEE_R2_CONFIG}"
      R2_CREDS="${CLAWEE_R2_CREDS}"
      GO_BIN="${FAKE_GO}"
      SKIP_R2=""
      mirror_to_r2 clawee v0.0.0-test 0.0.0-test "${WORK}" )
}

# ---- case 1: FAILED upload (creds present, r2-mirror exits non-zero) -------
say "case 1: FAILED upload -> must return non-zero + name the recovery steps"
CFG_OK="${WORK}/config.toml"; printf 'r2_account_id = "acct"\n' > "${CFG_OK}"
CREDS_OK="${WORK}/r2.key"; : > "${CREDS_OK}"
out="$(CLAWEE_R2_CONFIG="${CFG_OK}" CLAWEE_R2_CREDS="${CREDS_OK}" REPO_ROOT="${REPO_ROOT}" \
    run_case 2>&1)" && rc=0 || rc=$?
[ "${rc}" -ne 0 ] || die "mirror_to_r2 returned 0 on a FAILED upload (must fail closed)"
echo "${out}" | grep -qi 'GitHub release IS published' \
    || die "recovery message doesn't state the GitHub release is already published"
echo "${out}" | grep -qi 'did NOT' \
    || die "recovery message doesn't state what did NOT run"
echo "${out}" | grep -q 'tools/r2-mirror' \
    || die "recovery message doesn't name tools/r2-mirror for the by-hand re-run"
echo "${out}" | grep -qi 'will NOT safely' \
    || die "recovery message doesn't warn that re-running release.sh won't safely finish this"
echo "${out}" | grep -qi -- '--distribute-only' \
    || die "recovery message doesn't cover the --distribute-only caller"
echo "${out}" | grep -qi 'full cut' \
    || die "recovery message doesn't cover the do_release (full cut) caller"
say "case 1 OK (rc=${rc})"

# ---- case 2: SKIP — no r2_account_id in config ------------------------------
say "case 2: no r2_account_id configured -> must warn + return 0"
CFG_EMPTY="${WORK}/config-empty.toml"; : > "${CFG_EMPTY}"
out="$(CLAWEE_R2_CONFIG="${CFG_EMPTY}" CLAWEE_R2_CREDS="${CREDS_OK}" REPO_ROOT="${REPO_ROOT}" \
    run_case 2>&1)" && rc=0 || rc=$?
[ "${rc}" -eq 0 ] || die "mirror_to_r2 returned non-zero on unconfigured R2 (must stay warn+continue)"
echo "${out}" | grep -qi 'no r2_account_id' || die "missing-config skip lost its message"
say "case 2 OK (rc=0)"

# ---- case 3: SKIP — creds file absent ---------------------------------------
say "case 3: creds file absent -> must warn + return 0"
out="$(CLAWEE_R2_CONFIG="${CFG_OK}" CLAWEE_R2_CREDS="${WORK}/does-not-exist.key" REPO_ROOT="${REPO_ROOT}" \
    run_case 2>&1)" && rc=0 || rc=$?
[ "${rc}" -eq 0 ] || die "mirror_to_r2 returned non-zero on a missing creds file (must stay warn+continue)"
echo "${out}" | grep -qi 'creds not found' || die "missing-creds skip lost its message"
say "case 3 OK (rc=0)"

printf '\n✓ r2-mirror-fail-closed PASSED\n'
