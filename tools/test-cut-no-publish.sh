#!/usr/bin/env bash
# test-cut-no-publish.sh — the cut does not publish, and this proves it twice.
#
# A cut now stages privately and registers a `staged` row; going live is
# promote, an operator act. The failure this guards against is the quiet one:
# somebody reintroduces a `gh release create`, a tag push, an scp or a write to
# the PUBLIC bucket, and every test still passes because those steps only run
# on a real cut nobody executes in CI.
#
# Two complementary checks, because neither alone is enough:
#
#   PART A — a STATIC scan of release.sh. It covers the code paths a test run
#   never enters (a real, non-dry-run cut) at the cost of being a grep: it can
#   only see verbs spelled literally. That is exactly the shape a regression
#   takes here, because these verbs are copied back in from the old code.
#
#   PART B — a RUN of the cut's stage half with a stubbed PATH that LOGS every
#   invocation, so a publish reached indirectly through a helper script is
#   caught even though the grep cannot see it. BE PRECISE ABOUT ITS REACH: it
#   runs `--distribute-only --dry-run`, so it covers the DRY-RUN chain only. The
#   non-dry-run path is covered by PART A's grep plus PART D, which executes a
#   real pre-flight; nothing here executes a full non-dry-run cut, because past
#   the staging upload it would make a real marker commit in this repo.
#
#   The stub set is command-shaped, which is a second limit worth naming: the
#   register POST is Go net/http inside cmd/clawee-release-register, NOT curl,
#   so the curl stub does not cover it and never will. What keeps that call
#   honest is internal/register's own tests against a fake service, plus PART
#   A's assertion that the only URL this script can reach is the manage one.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_SH="${REPO_ROOT}/tools/release.sh"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# ---- PART A: no publish verb survives in release.sh -------------------------
# Comment lines are stripped first: the header and several doc comments say
# these words precisely to explain that they are gone, and a scan that cannot
# tell an explanation from a call is a scan nobody will keep passing.
say "PART A: release.sh carries no publish verb"
CODE="${WORK}/release-code.sh"
sed -E 's/^[[:space:]]*#.*$//' "${RELEASE_SH}" > "${CODE}"

check_absent() {
    local pattern="$1" what="$2"
    if grep -nE -- "${pattern}" "${CODE}" >/dev/null; then
        printf '\n' >&2
        grep -nE -- "${pattern}" "${CODE}" >&2
        die "release.sh still ${what} — that is promote's job, not the cut's"
    fi
    printf '  ✓ no %s\n' "${what}"
}

check_absent 'release create'                'creates a GitHub Release'
check_absent '(^|[^-[:alnum:]_])gh(p)? '     'invokes the GitHub CLI'
check_absent 'git[[:space:]]+tag'            'creates a release tag (the tag is promote'"'"'s)'
check_absent 'push[[:space:]]+--tags'        'pushes tags'
check_absent '(^|[[:space:]])scp[[:space:]]' 'scp'"'"'s the static surface'
check_absent 'CLAWEE_R2_BUCKET|r2_bucket'    'reads the PUBLIC bucket'
check_absent 'latest\.json'                  'names the public channel manifest'
check_absent 'prune-releases'                'runs retention (that is promote'"'"'s)'
check_absent 'gen-version-jsonp'             'regenerates the public version badge'
check_absent 'RELEASE_HOST|STATIC_DIR'       'names a static host'

# ---- PART B: the executed command log ---------------------------------------
# The stage half is exercised through --distribute-only --dry-run over a
# fabricated dist/<stamp>/. Every command that could publish is stubbed to a
# logger that RECORDS and REFUSES, so reaching one is both visible and fatal.
say "PART B: the executed command log names no publish verb"

# The stage half shells out to `go run` for both tools. Honour an inherited
# GO_BIN (a harness PATH may not carry go) and say plainly when there is none,
# rather than reporting a missing toolchain as a publish-verb failure.
GO_BIN="${GO_BIN:-$(command -v go 2>/dev/null || true)}"
[ -x "${GO_BIN}" ] || die "no Go toolchain found (set GO_BIN) — PART B cannot run"
export GO_BIN

STAMP="v0.0.0-test.2026.09.04.deadbeef"
STAGE="${REPO_ROOT}/dist/${STAMP}"
[ -e "${STAGE}" ] && die "refusing to overwrite an existing ${STAGE}"
mkdir -p "${STAGE}"
trap 'rm -rf "${WORK}" "${STAGE}"' EXIT
for f in clawee-clawee-darwin-arm64.zip clawee-clawee-darwin-amd64.zip \
         clawee-clawee-linux-arm64.zip clawee-clawee-linux-amd64.zip \
         SHA256SUMS.txt SHA256SUMS.txt.minisig; do
    printf 'test\n' > "${STAGE}/${f}"
done

# A throwaway component source worktree: the cut reads its branch (to derive
# the channel) and its HEAD, and nothing else. Pointing at the real one would
# make this test depend on which branch a developer happens to be on.
FAKE_SRC="${WORK}/src"; mkdir -p "${FAKE_SRC}"
git -C "${FAKE_SRC}" init -q -b main
git -C "${FAKE_SRC}" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init

LOG="${WORK}/commands.log"
: > "${LOG}"
STUBS="${WORK}/stubs"; mkdir -p "${STUBS}"
# Stub every publish-capable command. Each logs its argv and FAILS: a cut that
# reaches one of these has already lost, so letting it succeed would only hide
# how far it got.
for cmd in gh ghp scp ssh rsync curl aws; do
    cat > "${STUBS}/${cmd}" <<EOF
#!/usr/bin/env bash
echo "${cmd} \$*" >> "${LOG}"
echo "✗ stub: ${cmd} must never run during a cut" >&2
exit 97
EOF
    chmod +x "${STUBS}/${cmd}"
done
# git is logged but passes through: the cut legitimately reads git (branch,
# version stamps) and a stub that refused would fail for the wrong reason.
REAL_GIT="$(command -v git)"
cat > "${STUBS}/git" <<EOF
#!/usr/bin/env bash
echo "git \$*" >> "${LOG}"
exec "${REAL_GIT}" "\$@"
EOF
chmod +x "${STUBS}/git"

set +e
out="$(cd "${REPO_ROOT}" && PATH="${STUBS}:${PATH}" \
    CLAWEE_R2_CONFIG="${WORK}/no-such-config.toml" \
    CLAWEE_MANAGE_URL="https://manage.invalid" \
    CLAWEE_SRC_CLAWEE="${FAKE_SRC}" \
    bash "${RELEASE_SH}" --distribute-only clawee "${STAMP}" --dry-run 2>&1)"
rc=$?
set -e
printf '%s\n' "${out}"
[ "${rc}" -eq 0 ] || die "the dry-run stage half exited ${rc}"

for verb in 'gh ' 'ghp ' 'scp ' 'ssh ' 'rsync ' 'aws '; do
    grep -q "^${verb}" "${LOG}" && die "the cut executed '${verb%% *}' — that is promote's job"
done
grep -E '^git (tag|push)' "${LOG}" >/dev/null \
    && die "the cut ran a git tag/push: $(grep -E '^git (tag|push)' "${LOG}")"
printf '  ✓ command log clean (%d commands recorded)\n' "$(wc -l < "${LOG}" | tr -d ' ')"

# The dry-run must still SAY what it would do — a chain that publishes nothing
# and reports nothing is indistinguishable from one that silently did nothing.
echo "${out}" | grep -q "clawee/stable/${STAMP}/SHA256SUMS.txt" \
    || die "dry-run did not print the staging keys"
echo "${out}" | grep -qi 'no manifest' \
    || die "dry-run did not state that no manifest is written"
echo "${out}" | grep -q '"sums_key"' \
    || die "dry-run did not print the register payload"
echo "${out}" | grep -q '"channel": "stable"' \
    || die "dry-run payload carries no channel"
printf '  ✓ dry-run printed the staging keys and the register payload\n'

# ---- PART C: the channel is a claim, and a false one is refused -------------
# A beta tree cut as stable files beta bytes in the stable catalog, where
# retention and every installer treat them as the real thing. The branch
# decides by default; an explicit --channel stable over a beta branch is the
# one combination that is always a mistake.
say "PART C: --channel derives from the branch and refuses a false claim"
git -C "${FAKE_SRC}" checkout -q -b beta

# THIS PART RUNS AGAINST A THROWAWAY CLONE, for the reason PART E does and one
# more: the beta cases need an OPEN CYCLE, and seeding versions/clawee.beta.stamp
# in this checkout would collide with a real one the moment an operator opens a
# cycle — the suite would go red for a correct tree. The clone's marker is ours
# to write and ours to throw away.
#
# The live tools/ and versions/ are copied over the clone's committed content so
# the cases drive the WORKING TREE's release.sh, and the fabricated dist/<stamp>/
# is copied in because --distribute-only reads it from its own repo root.
KIT_C="${WORK}/kit-c"
git clone -q --no-hardlinks "${REPO_ROOT}" "${KIT_C}" 2>/dev/null \
    || die "could not clone the repo into a throwaway kit for PART C"
cp -R "${REPO_ROOT}/tools/." "${KIT_C}/tools/"
cp -R "${REPO_ROOT}/versions/." "${KIT_C}/versions/"
mkdir -p "${KIT_C}/dist"
cp -R "${STAGE}" "${KIT_C}/dist/"
BETA_STAMP_FILE="${KIT_C}/versions/clawee.beta.stamp"
rm -f "${BETA_STAMP_FILE}"

run_cut() {
    ( cd "${KIT_C}" && PATH="${STUBS}:${PATH}" \
        CLAWEE_R2_CONFIG="${WORK}/no-such-config.toml" \
        CLAWEE_MANAGE_URL="https://manage.invalid" \
        CLAWEE_SRC_CLAWEE="${FAKE_SRC}" \
        bash "${KIT_C}/tools/release.sh" --distribute-only clawee "${STAMP}" "$@" --dry-run 2>&1 )
}

# A beta cut needs an OPEN CYCLE. The permanent `beta` branch says nothing
# about whether one is open; versions/<comp>.beta.stamp is what does (beta.md
# §3), so a beta cut without it must refuse rather than invent a line.
set +e
out="$(run_cut)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "a beta cut with no open cycle succeeded:
${out}"
echo "${out}" | grep -q -- '--seed-beta' \
    || die "the closed-cycle refusal does not name the operator's way in:
${out}"
printf '  ✓ a beta cut with no open cycle is refused
'

# Open one for the rest of PART C. It lives in the clone, which the EXIT trap
# already removes with ${WORK}.
printf '0.3.0\n' > "${BETA_STAMP_FILE}"

out="$(run_cut)" || die "a beta-branch dry-run failed: ${out}"
echo "${out}" | grep -q "clawee/beta/${STAMP}/" \
    || die "a cut from a beta branch did not default to the beta channel:\n${out}"
printf '  ✓ beta branch defaults to the beta channel\n'

set +e
out="$(run_cut --channel stable)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "--channel stable from a beta branch was accepted"
echo "${out}" | grep -qi 'mislabel' \
    || die "the refusal does not say what goes wrong:\n${out}"
printf '  ✓ --channel stable from a beta branch is refused\n'

set +e
out="$(run_cut --channel beta)"; rc=$?
set -e
[ "${rc}" -eq 0 ] || die "--channel beta from a beta branch was refused: ${out}"
echo "${out}" | grep -q '"channel": "beta"' \
    || die "explicit --channel beta did not reach the payload:\n${out}"
printf '  ✓ explicit --channel beta is honoured\n'

# The beta cut reads the BETA line, not the stable one. Reading versions/clawee
# for a beta re-stage would file the row under the beta channel carrying the
# stable line's semver — a row nobody could match to a build.
echo "${out}" | grep -q '"version": "0.3.0"' \
    || die "the beta payload does not carry the beta line's semver (0.3.0):\n${out}"
printf '  ✓ the beta cut reads versions/clawee.beta.stamp\n'

set +e
out="$(run_cut --channel nonsense)"; rc=$?
set -e
[ "${rc}" -eq 2 ] || die "--channel nonsense should be a usage error (got ${rc})"
printf '  ✓ an unknown channel is a usage error\n'

# ---- shared setup for the non-dry-run pre-flight cases (PARTs D and E) -----
# The pre-flight only checks that these commands and the age-sealed key FILE
# exist; none of them runs before the refusals under test, so a stub that exits
# 0 is enough and nothing is ever built.
PREFLIGHT_STUBS="${WORK}/preflight"; mkdir -p "${PREFLIGHT_STUBS}"
for cmd in zip unzip minisign age; do
    printf '#!/usr/bin/env bash\nexit 0\n' > "${PREFLIGHT_STUBS}/${cmd}"
    chmod +x "${PREFLIGHT_STUBS}/${cmd}"
done
FAKE_DP="${WORK}/dp"; mkdir -p "${FAKE_DP}"
: > "${FAKE_DP}/clawee-release.key.age"

# ---- PART D: the NON-dry-run pre-flight actually runs ----------------------
# Every dry-run case above leaves the pre-flight's non-dry branch unexecuted,
# and that is how a real cut shipped broken: require_manage_url was called from
# the pre-flight and defined 180 lines below it, so bash had not read the name
# yet and every non-dry-run cut died "command not found", exit 127, before the
# first build. A grep cannot see that — the call and the definition both exist,
# only their order is wrong — so this case EXECUTES the pre-flight and demands
# the refusal it is supposed to produce.
#
# The manage-URL check is the LAST pre-flight step before the component loop, so
# refusing there proves the whole pre-flight ran and nothing was built.
say "PART D: a non-dry-run pre-flight reaches the manage-URL refusal"

set +e
out="$(cd "${REPO_ROOT}" && PATH="${PREFLIGHT_STUBS}:${STUBS}:${PATH}" \
    DP_DIR="${FAKE_DP}" \
    CLAWEE_R2_CONFIG="${WORK}/no-such-config.toml" \
    CLAWEE_MANAGE_URL="" \
    CLAWEE_SRC_CLAWEE="${FAKE_SRC}" \
    bash "${RELEASE_SH}" clawee </dev/null 2>&1)"
rc=$?
set -e

echo "${out}" | grep -qi 'command not found' \
    && die "the pre-flight called a function defined later in the file:
${out}"
[ "${rc}" -ne 127 ] || die "the cut exited 127 (unresolved command) — a function is used before it is defined:
${out}"
[ "${rc}" -ne 0 ] || die "a cut with no manage URL configured succeeded"
echo "${out}" | grep -qi 'no manage service URL configured' \
    || die "the pre-flight did not reach the manage-URL refusal (rc=${rc}):
${out}"
echo "${out}" | grep -q 'manage_url' \
    || die "the refusal does not name the config key:
${out}"
printf '  ✓ non-dry-run pre-flight refuses on the manage URL, naming the key\n'

# ---- PART E: the cut origin is channel-aware -------------------------------
# The guard used to be `branch = main`, full stop, which made the entire beta
# path unreachable on a full cut: resolve_channel could derive `beta` and
# --channel beta was accepted, but the only branch either could have come from
# was rejected. `main` is not THE cut origin, it is the STABLE one.
#
# BOTH SIDES OF THE GUARD ARE FORCED STRUCTURALLY. The guard reads two
# directories — the component source and the release repo (REPO_ROOT) — and
# REPO_ROOT is derived from release.sh's own path, so running the checked-out
# copy would make every outcome depend on the branch the reviewer happens to be
# on. It did: an earlier version of case 3 "passed" only because this worktree's
# project branch failed the release-repo guard, and on a checkout of `beta` both
# guards would have passed and the suite would have walked on into real builds
# with a passthrough git stub. So these cases run a THROWAWAY CLONE of the repo
# whose branch each case sets, exactly as they already do for the source.
#
# A URL is supplied so the pre-flight gets past PART D's subject; the refusal
# under test here is the branch one.
say "PART E: stable cuts from main, beta from beta, nothing cuts from elsewhere"

# The clone carries the repo's committed content; the live tools/ and versions/
# are copied over it so the case drives the WORKING TREE's release.sh rather
# than the last commit's.
KIT="${WORK}/kit"
git clone -q --no-hardlinks "${REPO_ROOT}" "${KIT}" 2>/dev/null \
    || die "could not clone the repo into a throwaway kit"
cp -R "${REPO_ROOT}/tools/." "${KIT}/tools/"
cp -R "${REPO_ROOT}/versions/." "${KIT}/versions/"

# run_preflight <src-branch> <repo-branch> [extra release.sh args...]
run_preflight() {
    local src_branch="$1" repo_branch="$2"; shift 2
    git -C "${FAKE_SRC}" checkout -q -B "${src_branch}"
    git -C "${KIT}" checkout -q -B "${repo_branch}"
    ( cd "${KIT}" && PATH="${PREFLIGHT_STUBS}:${STUBS}:${PATH}" \
        DP_DIR="${FAKE_DP}" \
        CLAWEE_R2_CONFIG="${WORK}/no-such-config.toml" \
        CLAWEE_MANAGE_URL="https://manage.invalid" \
        CLAWEE_SRC_CLAWEE="${FAKE_SRC}" \
        bash "${KIT}/tools/release.sh" clawee "$@" </dev/null 2>&1 )
}

# 1. The SOURCE is on a branch that maps to no channel. The repo is on `main`,
#    so it is not what refuses — the source is.
set +e
out="$(run_preflight feature-x main)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "a cut from a source branch that is no cut origin succeeded"
echo "${out}" | grep -q 'clawee source is on branch' \
    || die "the SOURCE guard is not what refused (repo was on main):
${out}"
echo "${out}" | grep -q 'no cut origin at all' \
    || die "the refusal does not say the branch is no cut origin:
${out}"
printf '  ✓ a source branch that is neither main nor beta is refused\n'

# 2. The flag contradicts the source branch. resolve_channel refuses before
#    either guard runs, so the repo's branch is irrelevant here.
set +e
out="$(run_preflight beta main --channel stable)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "--channel stable from a beta source passed the pre-flight"
echo "${out}" | grep -qi 'mislabel' \
    || die "the channel/branch mismatch refusal lost its explanation:
${out}"
printf '  ✓ --channel stable from a beta source is refused before any build\n'

# 3. The source is a correct beta origin and the REPO is not. This is the half
#    that used to depend on the reviewer's branch; the repo is now pinned to
#    `main`, which is the stable origin, so the mismatch is forced.
set +e
out="$(run_preflight beta main --channel beta)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "a beta cut from a repo checked out on main succeeded"
echo "${out}" | grep -q 'the release repo is on branch' \
    || die "the release repo's own branch is not guarded:
${out}"
echo "${out}" | grep -q 'the stable cut origin' \
    || die "the refusal does not name which channel the repo's branch IS for:
${out}"
echo "${out}" | grep -q 'clawee source is on branch' \
    && die "the source guard refused a correctly-paired beta branch:
${out}"
printf '  ✓ a beta source with the repo on main is refused by the repo guard\n'

# 4. A project worktree can never cut: its branch maps to no channel. Same
#    guard, the other flavour of refusal, and the case AGENTS.md calls out.
set +e
out="$(run_preflight main 2026-09-04-some-project)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "a cut from a project worktree succeeded"
echo "${out}" | grep -q 'the release repo is on branch' \
    || die "a project branch was not refused by the release-repo guard:
${out}"
echo "${out}" | grep -q 'no cut origin at all' \
    || die "a project branch should be no cut origin at all:
${out}"
printf '  ✓ a project worktree can never cut\n'

# 5. THE POSITIVE PATH. Both sides correctly paired for beta, so both guards
#    must PASS — and the run must still stop before it builds anything, because
#    this suite never executes a real cut. The dirty-source check is the very
#    next step after the guards, so a deliberately dirty source is what stops
#    it: reaching THAT refusal is proof both guards let the run through.
# The source must be made dirty BEFORE the run, not after: a clean, correctly
# paired source would sail past the guards and on into module_gate and a real
# build, which is precisely what this suite must never do.
printf 'dirt\n' > "${FAKE_SRC}/dirty-on-purpose"
set +e
out="$(run_preflight beta beta --channel beta)"; rc=$?
set -e
rm -f "${FAKE_SRC}/dirty-on-purpose"
[ "${rc}" -ne 0 ] || die "the run continued past the pre-flight — this suite must never execute a real cut"
echo "${out}" | grep -q 'cut origin' \
    && die "a correctly-paired beta cut was refused by a cut-origin guard:
${out}"
echo "${out}" | grep -qi 'worktree is dirty' \
    || die "expected the dirty-source refusal (proving both guards passed), got:
${out}"
printf '  ✓ beta/beta passes both guards and stops at the next check\n'

git -C "${FAKE_SRC}" checkout -q -B beta

printf '\n✓ cut-no-publish PASSED\n'
