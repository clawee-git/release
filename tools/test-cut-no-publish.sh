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
#   invocation. It proves the executed chain, not the source text, and it is
#   what catches a publish reached indirectly through a helper script.
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

run_cut() {
    ( cd "${REPO_ROOT}" && PATH="${STUBS}:${PATH}" \
        CLAWEE_R2_CONFIG="${WORK}/no-such-config.toml" \
        CLAWEE_MANAGE_URL="https://manage.invalid" \
        CLAWEE_SRC_CLAWEE="${FAKE_SRC}" \
        bash "${RELEASE_SH}" --distribute-only clawee "${STAMP}" "$@" --dry-run 2>&1 )
}

out="$(run_cut)" || die "a beta-branch dry-run failed: ${out}"
echo "${out}" | grep -q "clawee/beta/${STAMP}/" \
    || die "a cut from a beta branch did not default to the beta channel:\n${out}"
printf '  ✓ beta branch defaults to the beta channel\n'

set +e
out="$(run_cut --channel stable)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "--channel stable from a beta branch was accepted"
echo "${out}" | grep -qi 'mislabelled' \
    || die "the refusal does not say what goes wrong:\n${out}"
printf '  ✓ --channel stable from a beta branch is refused\n'

set +e
out="$(run_cut --channel beta)"; rc=$?
set -e
[ "${rc}" -eq 0 ] || die "--channel beta from a beta branch was refused: ${out}"
echo "${out}" | grep -q '"channel": "beta"' \
    || die "explicit --channel beta did not reach the payload:\n${out}"
printf '  ✓ explicit --channel beta is honoured\n'

set +e
out="$(run_cut --channel nonsense)"; rc=$?
set -e
[ "${rc}" -eq 2 ] || die "--channel nonsense should be a usage error (got ${rc})"
printf '  ✓ an unknown channel is a usage error\n'

printf '\n✓ cut-no-publish PASSED\n'
