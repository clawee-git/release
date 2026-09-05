#!/usr/bin/env bash
# test-beta-cut.sh — a FULL cut on --channel beta mints a beta stamp, stages it
# under the beta prefix, ships the twin binaries and the twin-rendered inner
# installer, and leaves the stable line completely alone.
#
# WHY A SECOND CUT TEST. test-cut-no-publish.sh PART C exercises
# --distribute-only, which starts from an already-built dist/ dir: it proves the
# channel reaches the staging prefix and the register payload, and nothing about
# what was BUILT. Everything this feature added lives upstream of that — which
# version file is read, what the binaries are called, what the inner installer
# says — so this one runs do_release's whole chain.
#
# The compiler is a stub (there is no cli worktree to build here, and what is
# under test is what the cut ASKS for, not whether Go compiles). `go run` is NOT
# stubbed: cmd/channel-names is this kit's own code and the single source the
# whole test exists to pin.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
RELEASE_SH="${REPO_ROOT}/tools/release.sh"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

REAL_GO="${GO_BIN:-$(command -v go 2>/dev/null || true)}"
[ -x "${REAL_GO}" ] || die "no Go toolchain found (set GO_BIN)"

WORK="$(mktemp -d)"
BETA_STAMP_FILE="${REPO_ROOT}/versions/clawee.beta.stamp"
[ -e "${BETA_STAMP_FILE}" ] && die "refusing to overwrite an existing ${BETA_STAMP_FILE}"
# The stable line must be byte-identical afterwards — record it before anything
# runs, and compare at the end. This is the assertion the whole beta design is
# for: a cycle moves its own line and nothing else.
STABLE_BEFORE="$(cat "${REPO_ROOT}/versions/clawee")"
cleanup() { rm -rf "${WORK}"; rm -f "${BETA_STAMP_FILE}"; rm -rf "${REPO_ROOT}/dist"; }
trap cleanup EXIT

# ---- a component source worktree on `beta`, the beta cut origin -------------
SRC="${WORK}/src"; mkdir -p "${SRC}"
git -C "${SRC}" init -q -b main
git -C "${SRC}" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
git -C "${SRC}" checkout -q -b beta
SHA="$(git -C "${SRC}" rev-parse --short=8 HEAD)"
TODAY="$(date -u +%Y.%m.%d)"
STAMP="v0.3.0.beta.${TODAY}.${SHA}"

# ---- stubs ------------------------------------------------------------------
LOG="${WORK}/commands.log"; : > "${LOG}"
STUBS="${WORK}/stubs"; mkdir -p "${STUBS}"

# go: answers `build` (logging argv, creating the -o target), forwards the rest.
cat > "${STUBS}/go" <<EOF
#!/usr/bin/env bash
if [ "\$1" = build ]; then
    echo "go \$*" >> "${LOG}"
    out=""; prev=""
    for a in "\$@"; do [ "\${prev}" = -o ] && out="\${a}"; prev="\${a}"; done
    [ -n "\${out}" ] && printf 'stub binary\n' > "\${out}"
    exit 0
fi
exec "${REAL_GO}" "\$@"
EOF
chmod +x "${STUBS}/go"

# codesign / minisign: the dry-run signs with the TEST key, but this host may
# not have minisign and signing is not what is under test.
cat > "${STUBS}/minisign" <<'EOF'
#!/usr/bin/env bash
out=""; prev=""
for a in "$@"; do [ "${prev}" = -m ] && out="${a}"; prev="${a}"; done
[ -n "${out}" ] && printf 'stub signature\n' > "${out}.minisig"
exit 0
EOF
chmod +x "${STUBS}/minisign"
printf '#!/usr/bin/env bash\nexit 0\n' > "${STUBS}/codesign"; chmod +x "${STUBS}/codesign"

# Nothing may publish. Each of these logs and fails.
#
# curl is deliberately NOT in this set. The cut's pre-flight runs the module
# gate, which runs tools/test-install-minisign.sh, which legitimately fetches —
# a refusing curl stub fails the cut for a reason that has nothing to do with
# the channel. Publication over curl would be caught by
# test-cut-no-publish.sh, whose whole job that is.
for cmd in gh ghp scp ssh rsync aws; do
    cat > "${STUBS}/${cmd}" <<EOF
#!/usr/bin/env bash
echo "${cmd} \$*" >> "${LOG}"
exit 97
EOF
    chmod +x "${STUBS}/${cmd}"
done

# The repo's TEST minisign key is not committed (it is generated locally), and
# signing is stubbed anyway — give the cut a key FILE so key resolution passes.
FAKE_KEY="${WORK}/test.key"; printf 'stub key\n' > "${FAKE_KEY}"

# ---- open the cycle and cut -------------------------------------------------
say "a beta cut of an open cycle"
printf '0.3.0\n' > "${BETA_STAMP_FILE}"

set +e
out="$( cd "${REPO_ROOT}" && PATH="${STUBS}:${PATH}" \
    GO_BIN="${STUBS}/go" \
    CLAWEE_R2_CONFIG="${WORK}/no-such-config.toml" \
    CLAWEE_MANAGE_URL="https://manage.invalid" \
    CLAWEE_SRC_CLAWEE="${SRC}" \
    SIGN_KEY="${FAKE_KEY}" \
    bash "${RELEASE_SH}" clawee --channel beta --dry-run 2>&1 )"
rc=$?
set -e
[ "${rc}" -eq 0 ] || { printf '%s\n' "${out}"; die "the beta dry-run cut exited ${rc}"; }

# ---- the stamp shape --------------------------------------------------------
say "the stamp is a beta stamp"
echo "${out}" | grep -qF "Stamp   : ${STAMP}" \
    || die "the cut did not mint ${STAMP}:
${out}"
# The catalog's betaStampRe is vX.Y.Z.beta.YYYY.MM.DD.<hash8>; a stamp that
# missed the segment would be filed as stable by every reader.
printf '%s' "${STAMP}" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+\.beta\.[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9a-f]{8}$' \
    || die "the stamp under test is not catalog-shaped: ${STAMP}"
printf '  ✓ %s\n' "${STAMP}"

# ---- the staging prefix -----------------------------------------------------
say "the bytes stage under <comp>/beta/<stamp>/"
echo "${out}" | grep -qF "clawee/beta/${STAMP}/SHA256SUMS.txt" \
    || die "the staging keys are not under the beta prefix:
${out}"
echo "${out}" | grep -q '"channel": "beta"' \
    || die "the register payload is not beta:
${out}"
echo "${out}" | grep -q '"version": "0.3.0"' \
    || die "the register payload does not carry the beta line's semver:
${out}"
printf '  ✓ clawee/beta/%s/, channel beta, version 0.3.0\n' "${STAMP}"

# ---- the twin binaries ------------------------------------------------------
say "the zips carry the twin binaries, built as beta"
grep -q -- '-X main.channel=beta' "${LOG}" || die "no -X main.channel=beta in the build log:
$(cat "${LOG}")"
grep -q -- '-o .*/claweeb ' "${LOG}" || grep -qE -- '-o [^ ]*/claweeb$' "${LOG}" \
    || die "the cut never built claweeb:
$(cat "${LOG}")"
! grep -qE -- '-o [^ ]*/clawee( |$)' "${LOG}" \
    || die "a beta cut built the STABLE basename clawee:
$(cat "${LOG}")"

ZIP="${REPO_ROOT}/dist/${STAMP}/clawee-clawee-linux-arm64.zip"
[ -f "${ZIP}" ] || die "no zip at ${ZIP}"
entries="$(unzip -Z1 "${ZIP}" | sort | tr '\n' ' ')"
case "${entries}" in
    *claweeb-updater*) ;;
    *) die "the zip has no claweeb-updater: ${entries}" ;;
esac
case "${entries}" in
    *' clawee '*|'clawee '*) die "the zip carries the stable binary name: ${entries}" ;;
esac
printf '  ✓ %s\n' "${entries}"

# ---- the inner installer ----------------------------------------------------
# The zip's install.sh is what actually places the binaries on the host. A
# stable-rendered one in a beta kit installs nothing (its BINS name files the
# zip does not contain) — or worse, overwrites the stable binary.
say "the inner installer is rendered for the twin"
UNZ="${WORK}/unz"; mkdir -p "${UNZ}"
unzip -q -o "${ZIP}" -d "${UNZ}"
INNER="${UNZ}/install.sh"
[ -f "${INNER}" ] || die "no install.sh in the beta zip"
grep -q 'BINS="claweeb claweeb-updater"' "${INNER}" \
    || die "the inner installer does not install the twins:
$(grep -n '^BINS=' "${INNER}")"
! grep -qE '@[A-Z_]+@|__[A-Z_]+__' "${INNER}" \
    || die "the inner installer still carries an unsubstituted placeholder:
$(grep -nE '@[A-Z_]+@|__[A-Z_]+__' "${INNER}" | head -3)"
# No STABLE binary name may survive anywhere a host would act on it. The word
# appears legitimately in comments and in CLAWEE_* env names, so this looks at
# the code only.
CODE="${WORK}/inner-code.sh"
sed -E 's/^[[:space:]]*#.*$//' "${INNER}" > "${CODE}"
if grep -nE '(^|[^a-zA-Z_-])clawee([^a-zA-Z_-]|$)' "${CODE}" | grep -v CLAWEE_ >/dev/null; then
    grep -nE '(^|[^a-zA-Z_-])clawee([^a-zA-Z_-]|$)' "${CODE}" | grep -v CLAWEE_ >&2
    die "the beta inner installer still names the stable binary"
fi
printf '  ✓ BINS="claweeb claweeb-updater", no stable name, no placeholder\n'

# ---- nothing published, nothing stable touched ------------------------------
say "nothing was published and the stable line did not move"
for verb in 'gh ' 'ghp ' 'scp ' 'ssh ' 'rsync ' 'aws '; do
    grep -q "^${verb}" "${LOG}" && die "the beta cut executed '${verb%% *}'"
done
[ "$(cat "${REPO_ROOT}/versions/clawee")" = "${STABLE_BEFORE}" ] \
    || die "the beta cut moved the stable line to $(cat "${REPO_ROOT}/versions/clawee")"
[ -z "$(git -C "${REPO_ROOT}" status --porcelain versions/clawee)" ] \
    || die "the beta cut left versions/clawee modified or staged"
# The stable bootstraps are regenerated by gen-bootstraps.sh on a real cut, but
# a dry-run must not have touched anything committed at all.
[ -z "$(git -C "${REPO_ROOT}" status --porcelain clawee/install.sh clawee/upgrade.sh)" ] \
    || die "the beta dry-run modified the stable bootstraps"
printf '  ✓ stable line still %s, stable bootstraps untouched\n' "${STABLE_BEFORE}"

printf '\n✓ test-beta-cut PASSED\n'
