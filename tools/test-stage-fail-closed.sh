#!/usr/bin/env bash
# test-stage-fail-closed.sh — unit-level pin for stage_to_staging's refusals.
#
# The predecessor of this file pinned mirror_to_r2, whose whole subtlety was
# WHEN a missing mirror was tolerable: R2 was a mirror behind GitHub Releases,
# so an unconfigured box could skip it and still have published something. That
# tolerance is exactly what shipped a withdrawn stamp's version.js on
# 2026-08-20 when a FAILED upload was let through as a warning.
#
# The private cut removes the question. The staging upload is the ONLY thing a
# cut publishes, so "skipped" and "failed" are the same outcome — a cut that
# produced nothing — and neither may return 0. Every branch below is a refusal;
# there is no warn-and-continue branch left to test.
#
# WHY a unit test on the function, not an end-to-end run: past the staging
# upload the cut regenerates bootstraps and makes a real marker `git commit` in
# REPO_ROOT. There is no throwaway checkout to run that in without polluting
# this repo. tools/test-e2e.sh stays on the --dry-run path;
# tools/test-cut-no-publish.sh greps the executed-command log. This test
# extracts the function verbatim with awk (not a hand-copy), so it breaks the
# moment that function's shape changes instead of testing a stale fork of it.
#
# Cases, each in a fresh subshell so env never leaks between them:
#   1. no r2_account_id            -> refuse
#   2. no staging_bucket           -> refuse, and say why there is no default
#   3. creds file absent           -> refuse
#   4. upload attempted and failed -> refuse, and state that nothing published
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_SH="${REPO_ROOT}/tools/release.sh"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# ---- extract toml_get() + stage_to_staging() verbatim from release.sh ------
# ASSUMPTION: each function's closing brace is alone on a column-0 line ("}")
# and nothing inside the function body opens a column-0 "{ ... }" group of its
# own — the awk range stops at the FIRST such line, so a future edit that adds
# one inside either function would silently truncate the extraction. The greps
# below only catch a missing or renamed opening line, not a truncated body.
FUNCS="${WORK}/funcs.sh"
{
    awk '/^toml_get\(\) \{/,/^}/' "${RELEASE_SH}"
    echo
    awk '/^stage_to_staging\(\) \{/,/^}/' "${RELEASE_SH}"
} > "${FUNCS}"
grep -q '^toml_get() {' "${FUNCS}" \
    || die "extraction found no toml_get() in ${RELEASE_SH} — release.sh's shape changed, update this test"
grep -q '^stage_to_staging() {' "${FUNCS}" \
    || die "extraction found no stage_to_staging() in ${RELEASE_SH} — release.sh's shape changed, update this test"

# fake GO_BIN standing in for `go run .` inside tools/r2-mirror. Only case 4
# ever reaches it (every other case refuses before calling it) and it always
# fails — this test is about the branching, not a working upload.
FAKE_GO="${WORK}/fake-go"
cat > "${FAKE_GO}" <<'EOF'
#!/usr/bin/env bash
exit 1
EOF
chmod +x "${FAKE_GO}"

run_case() {
    ( set -euo pipefail
      # shellcheck source=/dev/null
      source "${FUNCS}"
      R2_CONFIG="${CLAWEE_R2_CONFIG}"
      R2_CREDS="${CLAWEE_R2_CREDS}"
      GO_BIN="${FAKE_GO}"
      stage_to_staging clawee v0.0.0-test 0.0.0-test "${WORK}" stable )
}

CFG_FULL="${WORK}/config.toml"
printf 'r2_account_id = "acct"\nstaging_bucket = "staging-test"\n' > "${CFG_FULL}"
CFG_NO_ACCOUNT="${WORK}/config-no-account.toml"
printf 'staging_bucket = "staging-test"\n' > "${CFG_NO_ACCOUNT}"
CFG_NO_BUCKET="${WORK}/config-no-bucket.toml"
printf 'r2_account_id = "acct"\n' > "${CFG_NO_BUCKET}"
CREDS_OK="${WORK}/r2.key"; : > "${CREDS_OK}"

expect_refusal() {
    local what="$1" cfg="$2" creds="$3"; shift 3
    local out rc
    out="$(CLAWEE_R2_CONFIG="${cfg}" CLAWEE_R2_CREDS="${creds}" CLAWEE_R2_STAGING_BUCKET="" \
        REPO_ROOT="${REPO_ROOT}" run_case 2>&1)" && rc=0 || rc=$?
    [ "${rc}" -ne 0 ] || die "${what}: stage_to_staging returned 0 — a cut that published nothing must never report success"
    local needle
    for needle in "$@"; do
        echo "${out}" | grep -qi -- "${needle}" \
            || die "${what}: refusal does not mention '${needle}'; got:\n${out}"
    done
    say "${what} OK (rc=${rc})"
}

say "case 1: no r2_account_id -> refuse"
expect_refusal "no account" "${CFG_NO_ACCOUNT}" "${CREDS_OK}" 'r2_account_id'

say "case 2: no staging_bucket -> refuse and say why there is no default"
expect_refusal "no staging bucket" "${CFG_NO_BUCKET}" "${CREDS_OK}" 'staging_bucket' 'no default' 'PUBLIC'

say "case 3: creds file absent -> refuse"
expect_refusal "no creds" "${CFG_FULL}" "${WORK}/does-not-exist.key" 'creds not found'

say "case 4: upload attempted and FAILED -> refuse, and state nothing published"
expect_refusal "failed upload" "${CFG_FULL}" "${CREDS_OK}" \
    'FAILED' 'NOTHING was published' 'no catalog row' 'distribute-only'

printf '\n✓ stage-fail-closed PASSED\n'
