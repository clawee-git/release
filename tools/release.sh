#!/usr/bin/env bash
# release.sh — cut a signed Clawee component release (clawee | claweed).
#
# Usage:
#   bash tools/release.sh <clawee|claweed|all> [--apple] [--dry-run] [--bump-minor|--bump-major]
#
# --apple: Developer ID sign the darwin binaries (modernech-sign, Modernech LLC)
#   + notarize each darwin zip before publishing. WITHOUT it darwin bins are
#   ad-hoc signed (the default) — fine for curl-install (no quarantine xattr);
#   use --apple for release versions that may be browser-downloaded. Guideline:
#   ~/.claude/guidelines/APPLE-SIGNING.md.
#
# For each requested component this:
#   1. Stamps the version (bump unless --dry-run) via tools/version.sh.
#   2. Cross-compiles the component for darwin/{arm64,amd64} + linux/{arm64,amd64},
#      assembling each target into dist/<stamp>/clawee-<comp>-<os>-<arch>/ that
#      carries the component bins + the inner installer renamed to install.sh,
#      then `zip -j`s it.
#   3. Writes a sorted SHA256SUMS.txt over the four zips.
#   4. Signs SHA256SUMS.txt with minisign (real key from release.dp, or the TEST
#      key on --dry-run).
#   5. (non-dry-run) git-tags <comp>/<stamp> + publishes a GitHub Release.
#   6. (non-dry-run) regenerates the bootstraps and scp's the static surface to
#      the release host.
#   7. (non-dry-run) records a [RELEASED: <comp>] marker commit.
#
# On --dry-run only steps 1-4 run, and the version bump is REVERTED — the tree is
# left exactly as it was, just with throwaway artifacts under dist/<stamp>/.
#
# claweed inner installer: rendered at build time from the daemon repo's CANONICAL
# install/install.sh.in (the sudo-minimal installer), substituting the stamp. The
# release zip is NEVER allowed to ship a forked copy — it is always the daemon's
# template, version-stamped, so the served installer can't drift from source.
#
# Env (all optional — sane defaults below):
#   RELEASE_HOST           ssh alias for the nginx static host (default nsm.renative.com)
#   STATIC_DIR             absolute static dir on that host
#   DP_DIR                 path to the release.dp secrets repo
#   SIGN_KEY               minisign secret key file (overrides the default resolution)
#   AGE_IDENTITY           age identity file used to decrypt the real signing key
#                          (default ~/.age/clawee-release.txt — created at activation)
#   CLAWEE_SRC_CLAWEE      clawee component source worktree (default: cli main worktree)
#   CLAWEE_SRC_CLAWEED     claweed component source worktree (default: daemon main worktree)
#   CLAWEE_RELEASE_REPO    GitHub repo for releases (default clawee-git/release)
#   CLAWEE_RELEASE_YES     skip the interactive minor/major bump confirm
#   CLAWEE_R2_BUCKET       override the R2 mirror bucket (default: r2_bucket from
#                          DP_DIR/config.toml, else clawee-downloads)
#   CLAWEE_R2_CREDS        path to the shared R2 S3 creds TOML
#                          (default ~/.burrowee/release/r2.key — shared with burrowee)
#   CLAWEE_SKIP_R2=1 / --no-r2  skip the downloads.clawee.org R2 mirror entirely
#                          (the R2 mirror is also auto-skipped when unconfigured —
#                          GitHub Releases stay the primary, authoritative channel)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# ---- go on PATH (the Clawee per-dir hook strips /opt/homebrew/bin) -----------
GO_BIN="${GO_BIN:-go}"
command -v "${GO_BIN}" >/dev/null 2>&1 || GO_BIN=/opt/homebrew/bin/go
export GO_BIN

# ---- args -------------------------------------------------------------------
WHAT=""
DRY_RUN=0
BUMP_KIND="patch"
APPLE_SIGN=""
SKIP_R2="${CLAWEE_SKIP_R2:-}"
for arg in "$@"; do
    case "${arg}" in
        clawee|claweed|all)   WHAT="${arg}" ;;
        --apple)              APPLE_SIGN=1 ;;
        --dry-run)            DRY_RUN=1 ;;
        --bump-minor)         BUMP_KIND="minor" ;;
        --bump-major)         BUMP_KIND="major" ;;
        --no-r2)              SKIP_R2=1 ;;
        -h|--help)            sed -n '2,52p' "$0" | sed 's/^# \{0,1\}//'; exit 0 ;;
        *) echo "✗ unknown argument: ${arg}" >&2; exit 2 ;;
    esac
done
[ -n "${WHAT}" ] || { echo "✗ usage: release.sh <clawee|claweed|all> [--apple] [--dry-run] [--no-r2] [--bump-minor|--bump-major]" >&2; exit 2; }
export APPLE_SIGN

# ---- config / defaults ------------------------------------------------------
RELEASE_HOST="${RELEASE_HOST:-nsm.renative.com}"
STATIC_DIR="${STATIC_DIR:-/ebs_storage/apps/release.clawee.org/static}"
RELEASE_REPO="${CLAWEE_RELEASE_REPO:-clawee-git/release}"
DP_DIR="${DP_DIR:-${REPO_ROOT}/../../../release.dp/code/release.dp}"
AGE_KEY_AGE="${DP_DIR}/clawee-release.key.age"
AGE_IDENTITY="${AGE_IDENTITY:-${HOME}/.age/clawee-release.txt}"

# ---- R2 mirror config (public downloads.clawee.org) -------------------------
# R2 is a MIRROR of the GitHub release — GitHub is the primary channel. The
# account id + bucket are non-secret identifiers kept in the release.dp
# config.toml (same DP_DIR as the signing key); the S3 creds are SHARED with
# burrowee and live OUTSIDE any repo at ~/.burrowee/release/r2.key (referenced,
# never copied here). Any missing piece → the mirror is SKIPPED, never fatal.
R2_CONFIG="${DP_DIR}/config.toml"
R2_CREDS="${CLAWEE_R2_CREDS:-${HOME}/.burrowee/release/r2.key}"

# toml_get <file> <key> — first `key = "value"` / `key = value`, quotes stripped.
toml_get() {
    [ -f "$1" ] || return 1
    sed -n -E "s/^[[:space:]]*$2[[:space:]]*=[[:space:]]*\"?([^\"]*)\"?[[:space:]]*\$/\1/p" "$1" | head -n1
}

# component source worktrees (default: each component's MAIN worktree).
# clawee builds from the cli repo (cmd/clawee); claweed from the daemon repo.
CC="/Volumes/MacintoshED/Workstation/Coding/Clawee"
SRC_CLAWEE="${CLAWEE_SRC_CLAWEE:-${CC}/cli/code/cli}"
SRC_CLAWEED="${CLAWEE_SRC_CLAWEED:-${CC}/daemon/code/daemon}"

# the canonical claweed inner-installer template (rendered per-build with the stamp)
CLAWEED_INSTALLER_IN="${SRC_CLAWEED}/install/install.sh.in"

TARGETS=(
    "darwin arm64"
    "darwin amd64"
    "linux arm64"
    "linux amd64"
)

src_for() {
    case "$1" in
        clawee)   printf '%s' "${SRC_CLAWEE}" ;;
        claweed)  printf '%s' "${SRC_CLAWEED}" ;;
    esac
}

# binary list per component (used at assembly time to copy into the zip)
bins_for() {
    case "$1" in
        clawee)   printf '%s' "clawee" ;;
        claweed)  printf '%s' "claweed clawee-spawn" ;;
    esac
}

GHP="$(command -v ghp 2>/dev/null || echo "${HOME}/.claude/bin/ghp")"

# ---- pre-flight -------------------------------------------------------------
need() { command -v "$1" >/dev/null 2>&1 || { echo "✗ required tool not found: $1" >&2; exit 1; }; }
need zip
need unzip
need minisign
command -v "${GO_BIN}" >/dev/null 2>&1 || { echo "✗ go not found (tried '${GO_BIN}')" >&2; exit 1; }

# Apple-sign mode: resolve the shared Modernech signer + confirm the identity is
# installed. Exported so tools/build.sh signs the darwin bins with the same tool;
# darwin zips are notarized below after assembly.
if [ -n "${APPLE_SIGN}" ]; then
    [ "$(uname -s)" = Darwin ] || { echo "✗ --apple requires a macOS build host" >&2; exit 1; }
    SIGN_BIN="${MODERNECH_SIGN:-modernech-sign}"
    command -v "${SIGN_BIN}" >/dev/null 2>&1 || SIGN_BIN="${HOME}/bin/modernech-sign"
    command -v "${SIGN_BIN}" >/dev/null 2>&1 \
        || { echo "✗ --apple set but modernech-sign not found on PATH or ~/bin" >&2; exit 1; }
    security find-identity -v -p codesigning 2>/dev/null | grep -q "$("${SIGN_BIN}" id)" \
        || { echo "✗ Developer ID identity not in keychain: $("${SIGN_BIN}" id)" >&2; exit 1; }
    export MODERNECH_SIGN="${SIGN_BIN}"
    echo "→ --apple: Developer ID signing + notarization via ${SIGN_BIN}" >&2
fi

# sha256 tool (shasum on mac, sha256sum on linux)
if command -v shasum >/dev/null 2>&1; then
    SHA256="shasum -a 256"
elif command -v sha256sum >/dev/null 2>&1; then
    SHA256="sha256sum"
else
    echo "✗ neither shasum nor sha256sum found" >&2; exit 1
fi

if [ "${DRY_RUN}" != 1 ]; then
    need age
    # ghp is intentionally NOT `need`-checked: the per-dir hook can strip it from
    # PATH, and GHP is resolved with a ~/.claude/bin fallback above — so validate
    # that RESOLVED path here instead of requiring ghp on PATH (a bare `need ghp`
    # checks PATH and spuriously fails the cut even though GHP is usable).
    [ -x "${GHP}" ] || { echo "✗ ghp wrapper not found at ${GHP}" >&2; exit 1; }
    "${GHP}" repo view "${RELEASE_REPO}" --json name >/dev/null 2>&1 \
        || { echo "✗ ghp cannot access ${RELEASE_REPO} — check gh.account + auth" >&2; exit 1; }
    ssh -o BatchMode=yes -o ConnectTimeout=5 "${RELEASE_HOST}" 'true' 2>/dev/null \
        || { echo "✗ cannot ssh to ${RELEASE_HOST}" >&2; exit 1; }
    [ -f "${AGE_KEY_AGE}" ] \
        || { echo "✗ release.dp signing key not found: ${AGE_KEY_AGE}" >&2; exit 1; }
fi

# components to cut
if [ "${WHAT}" = all ]; then COMPONENTS=(clawee claweed); else COMPONENTS=("${WHAT}"); fi

# per-component source-worktree cleanliness + branch (real releases must come
# from a clean `main`; dry-runs are lenient so they can run off a prep worktree).
for comp in "${COMPONENTS[@]}"; do
    src="$(src_for "${comp}")"
    [ -d "${src}" ] || { echo "✗ ${comp} source worktree missing: ${src}" >&2; exit 1; }
    git -C "${src}" rev-parse --git-dir >/dev/null 2>&1 \
        || { echo "✗ ${comp} source is not a git worktree: ${src}" >&2; exit 1; }
    if [ "${DRY_RUN}" != 1 ]; then
        br="$(git -C "${src}" rev-parse --abbrev-ref HEAD)"
        [ "${br}" = main ] || { echo "✗ ${comp} source not on main (on ${br}): ${src}" >&2; exit 1; }
        [ -z "$(git -C "${src}" status --porcelain)" ] \
            || { echo "✗ ${comp} source worktree is dirty: ${src}" >&2; exit 1; }
    fi
done

# ---- resolve the signing key ------------------------------------------------
# Sets SIGN_KEY. For the real key we age-decrypt into a chmod-600 tmpfile and
# trap-shred it on EXIT. The TEST key is used as-is for --dry-run.
SHRED_FILE=""
shred_key() {
    [ -n "${SHRED_FILE}" ] || return 0
    [ -f "${SHRED_FILE}" ] || return 0
    if command -v shred >/dev/null 2>&1; then
        shred -u "${SHRED_FILE}" 2>/dev/null || rm -f "${SHRED_FILE}"
    else
        # no shred on macOS — overwrite then unlink. The decrypted signing key
        # must NEVER survive on disk un-overwritten (rm alone leaves it
        # recoverable), so a dd failure aborts loudly instead of silently
        # rm'ing the still-readable key.
        if ! dd if=/dev/urandom of="${SHRED_FILE}" bs=1k count=2 conv=notrunc 2>/dev/null; then
            rm -f "${SHRED_FILE}"
            echo "✗ FAILED to overwrite decrypted signing key at ${SHRED_FILE} — it may be recoverable; investigate" >&2
            exit 1
        fi
        rm -f "${SHRED_FILE}"
    fi
    SHRED_FILE=""
}
trap 'shred_key' EXIT INT TERM

resolve_sign_key() {
    if [ -n "${SIGN_KEY:-}" ]; then
        [ -f "${SIGN_KEY}" ] || { echo "✗ SIGN_KEY not found: ${SIGN_KEY}" >&2; exit 1; }
        echo "→ signing with provided SIGN_KEY: ${SIGN_KEY}" >&2
        return 0
    fi
    if [ "${DRY_RUN}" = 1 ]; then
        SIGN_KEY="${REPO_ROOT}/tools/testkeys/test.key"
        [ -f "${SIGN_KEY}" ] \
            || { echo "✗ TEST signing key missing: ${SIGN_KEY} (generate it: minisign -G -p tools/testkeys/test.pub -s tools/testkeys/test.key)" >&2; exit 1; }
        echo "→ dry-run: signing with the TEST key (${SIGN_KEY})" >&2
        return 0
    fi
    # real release: decrypt the age-sealed signing key to a 600 tmpfile.
    [ -f "${AGE_IDENTITY}" ] || { echo "✗ age identity not found: ${AGE_IDENTITY}" >&2; exit 1; }
    SHRED_FILE="$(mktemp "${TMPDIR:-/tmp}/clawee-release-key.XXXXXX")"
    chmod 600 "${SHRED_FILE}"
    age -d -i "${AGE_IDENTITY}" -o "${SHRED_FILE}" "${AGE_KEY_AGE}" \
        || { echo "✗ failed to decrypt ${AGE_KEY_AGE}" >&2; exit 1; }
    SIGN_KEY="${SHRED_FILE}"
    echo "→ signing with the real key (decrypted from release.dp)" >&2
}
resolve_sign_key

# ---- inner installer resolution ---------------------------------------------
# clawee ships the repo-committed inner/clawee/install.sh. claweed ships the
# daemon repo's canonical install/install.sh.in, rendered per-build with the
# stamp — never the repo-committed inner/claweed copy (which is only kept current
# for shellcheck + reference). render_inner <comp> <stamp> <dest> writes install.sh.
render_inner() {
    local comp="$1" stamp="$2" dest="$3"
    case "${comp}" in
        clawee)
            cp "${REPO_ROOT}/inner/clawee/install.sh" "${dest}"
            ;;
        claweed)
            [ -f "${CLAWEED_INSTALLER_IN}" ] \
                || { echo "✗ canonical claweed installer template missing: ${CLAWEED_INSTALLER_IN} (set CLAWEE_SRC_CLAWEED)" >&2; exit 1; }
            sed "s/__CLAWEED_VERSION__/${stamp}/g" "${CLAWEED_INSTALLER_IN}" > "${dest}"
            ;;
    esac
    chmod 0755 "${dest}"
}

# ---- R2 mirror --------------------------------------------------------------
# mirror_to_r2 <comp> <stamp> <semver> <stage_dir>
#
# Publishes a just-released component's staged artifacts to the PUBLIC R2 bucket
# behind downloads.clawee.org (the install-time fallback). GRACEFUL: any missing
# config (no account id / no creds file) → warn + skip; an upload failure → warn
# loudly but still return 0. The GitHub release is already published by the time
# this runs, so a broken mirror must NEVER abort the release. clawee's R2 is a
# plain public bucket — no console, no presigning. Never called under --dry-run.
mirror_to_r2() {
    local comp="$1" stamp="$2" semver="$3" stage="$4"
    if [ -n "${SKIP_R2}" ]; then
        echo "→ R2 mirror skipped (--no-r2 / CLAWEE_SKIP_R2)"
        return 0
    fi
    local account bucket
    account="$(toml_get "${R2_CONFIG}" r2_account_id || true)"
    bucket="${CLAWEE_R2_BUCKET:-$(toml_get "${R2_CONFIG}" r2_bucket || true)}"
    [ -n "${bucket}" ] || bucket="clawee-downloads"
    if [ -z "${account}" ]; then
        echo "⚠ R2 mirror skipped: no r2_account_id in ${R2_CONFIG} (R2 is only a mirror; GitHub is primary)" >&2
        return 0
    fi
    if [ ! -f "${R2_CREDS}" ]; then
        echo "⚠ R2 mirror skipped: creds not found at ${R2_CREDS} (set CLAWEE_R2_CREDS; GitHub release is published)" >&2
        return 0
    fi
    echo "→ mirroring ${comp} ${stamp} → R2 bucket ${bucket} (downloads.clawee.org)"
    if ( cd "${REPO_ROOT}/tools/r2-mirror" && "${GO_BIN}" run . \
            --account "${account}" --bucket "${bucket}" \
            --stage-dir "${stage}" --comp "${comp}" \
            --version "${semver}" --stamp "${stamp}" \
            --creds "${R2_CREDS}" ); then
        echo "✓ mirrored ${comp} to R2 (downloads.clawee.org/${comp}/latest.json)"
    else
        echo "⚠ R2 mirror FAILED for ${comp} ${stamp} — GitHub release is published; re-run the mirror by hand (tools/r2-mirror)" >&2
    fi
    return 0
}

# ---- per-component release --------------------------------------------------
do_release() {
    local comp="$1"
    local src; src="$(src_for "${comp}")"
    local bins; bins="$(bins_for "${comp}")"

    echo
    echo "=== clawee ${comp} release ==="

    # (1) stamp — bump unless dry-run.
    local old_semver new_semver stamp
    old_semver="$(SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --semver)"
    if [ "${DRY_RUN}" = 1 ]; then
        stamp="$(SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --stamp)"
        new_semver="${old_semver}"
    else
        case "${BUMP_KIND}" in
            patch) SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --bump-patch >/dev/null ;;
            minor) SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --bump-minor >/dev/null ;;
            major) SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --bump-major >/dev/null ;;
        esac
        new_semver="$(SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --semver)"
        stamp="$(SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --stamp)"
    fi

    # From here the versions/<comp> file may be modified. Any failure (or the
    # dry-run completion) reverts it.
    revert_version() {
        git restore --staged "versions/${comp}" 2>/dev/null || true
        git checkout -- "versions/${comp}" 2>/dev/null || true
    }
    trap 'revert_version; shred_key' ERR

    echo "Bump    : ${BUMP_KIND} (${old_semver} → ${new_semver})"
    echo "Stamp   : ${stamp}"
    echo "Source  : ${src} @ $(git -C "${src}" rev-parse --short=8 HEAD)"
    echo "Dry-run : ${DRY_RUN}"

    local stage="${REPO_ROOT}/dist/${stamp}"
    rm -rf "${stage}"
    mkdir -p "${stage}"

    # (2) per-target build + assemble + zip.
    local zips=() pair os arch out_bins assemble asset b
    for pair in "${TARGETS[@]}"; do
        read -r os arch <<<"${pair}"
        out_bins="${stage}/.bins-${os}-${arch}"
        mkdir -p "${out_bins}"

        # component bins
        COMP="${comp}" SRC_DIR="${src}" TARGETOS="${os}" TARGETARCH="${arch}" \
            STAMP="${stamp}" OUT_DIR="${out_bins}" GO_BIN="${GO_BIN}" \
            bash "${REPO_ROOT}/tools/build.sh" >&2

        # assemble: component bins + inner installer (→ install.sh)
        assemble="${stage}/clawee-${comp}-${os}-${arch}"
        rm -rf "${assemble}"
        mkdir -p "${assemble}"
        # shellcheck disable=SC2086  # ${bins} is an intentional space-list of bin names from bins_for(); word-splitting is the point.
        for b in ${bins}; do cp "${out_bins}/${b}" "${assemble}/${b}"; done
        render_inner "${comp}" "${stamp}" "${assemble}/install.sh"

        asset="clawee-${comp}-${os}-${arch}.zip"
        rm -f "${stage}/${asset}"
        ( cd "${assemble}" && zip -j -q "${stage}/${asset}" ./* )

        # Apple-sign mode: notarize the darwin zips (binaries were Developer ID
        # signed by build.sh). Submitting doesn't alter the zip, so the later
        # SHA256SUMS + minisign still cover these exact bytes. Bare-binary zips
        # can't be stapled — the ticket lives in Apple's online DB. linux: skip.
        if [ -n "${APPLE_SIGN}" ] && [ "${os}" = darwin ]; then
            "${SIGN_BIN}" notarize "${stage}/${asset}" >&2
        fi

        zips+=("${asset}")
        rm -rf "${out_bins}"
    done

    # (3) sums over the four zips.
    # shellcheck disable=SC2086  # ${SHA256} is an intentional space-split command string ("shasum -a 256" | "sha256sum"); word-splitting is the point.
    ( cd "${stage}" && ${SHA256} clawee-"${comp}"-*.zip | sort > SHA256SUMS.txt )

    # (4) sign.
    ( cd "${stage}" && minisign -S -s "${SIGN_KEY}" -m SHA256SUMS.txt \
        -t "clawee ${comp} ${stamp}" >/dev/null )

    echo "Built ${#zips[@]} zips + SHA256SUMS.txt + SHA256SUMS.txt.minisig:"
    # shellcheck disable=SC2012  # cosmetic listing of our own controlled asset names (no untrusted filenames); ls keeps the plain one-per-line format.
    ( cd "${stage}" && ls -1 clawee-"${comp}"-*.zip SHA256SUMS.txt SHA256SUMS.txt.minisig | sed 's/^/    /' )

    if [ "${DRY_RUN}" = 1 ]; then
        echo "✓ dry-run ${comp}: artifacts under ${stage}/ (version bump reverted; no tag/release/scp)"
        revert_version
        trap shred_key ERR
        return 0
    fi

    # (5) tag + GitHub Release.
    # Change summary: component commits since the previous release's source sha.
    # The stamp's trailing field IS the 8-char source sha, so the previous
    # release's sha is the suffix of the highest existing <comp>/v… tag.
    local prev_tag prev_sha changes
    prev_tag="$(/usr/bin/git tag -l "${comp}/v*" --sort=version:refname | tail -n1)"
    prev_sha="${prev_tag##*.}"
    if [ -n "${prev_sha}" ] && git -C "${src}" cat-file -e "${prev_sha}^{commit}" 2>/dev/null; then
        changes="$(git -C "${src}" log --oneline --no-merges "${prev_sha}..HEAD" 2>/dev/null)"
        [ -n "${changes}" ] || changes="No code changes since ${prev_tag} (re-release)."
    else
        changes="Initial release."
    fi

    local tag="${comp}/${stamp}"
    if git rev-parse "refs/tags/${tag}" >/dev/null 2>&1; then
        echo "✗ tag ${tag} already exists locally — reverting version" >&2; exit 1
    fi
    git tag -a "${tag}" -m "clawee ${comp} ${stamp}"

    local notes; notes="${stage}/release-notes.md"
    cat > "${notes}" <<NOTES
clawee ${comp} ${stamp} — $(date -u +%Y-%m-%d)

## Changes
${changes}

Install:
  curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/${comp}/install.sh | sh

Pin this version:
  CLAWEE_$(printf '%s' "${comp}" | tr '[:lower:]' '[:upper:]')_VERSION=${tag} \\
    curl -fsSL https://release.clawee.org/${comp}/install.sh | sh

Verify by hand:
  minisign -Vm SHA256SUMS.txt -P "\$(cat clawee-release.pub | tail -n1)"
  shasum -a 256 -c SHA256SUMS.txt
NOTES

    ( cd "${stage}" && "${GHP}" -R "${RELEASE_REPO}" release create "${tag}" \
        --title "${comp} ${stamp}" --notes-file "${notes}" \
        clawee-"${comp}"-*.zip SHA256SUMS.txt SHA256SUMS.txt.minisig )

    # Past the tag/release — clear the version-revert trap.
    trap shred_key ERR

    # (5b) mirror the published artifacts to the public R2 bucket
    # (downloads.clawee.org) as the install-time fallback. Non-fatal by design.
    mirror_to_r2 "${comp}" "${stamp}" "${new_semver}" "${stage}"

    # (6) regenerate bootstraps + scp the static surface.
    bash "${REPO_ROOT}/tools/gen-bootstraps.sh" >&2

    # shellcheck disable=SC2029  # ${STATIC_DIR}/${comp} are local, controlled values — expanding client-side into the remote command is intended.
    ssh "${RELEASE_HOST}" "mkdir -p '${STATIC_DIR}/${comp}'"
    scp -q "${REPO_ROOT}/${comp}/install.sh" "${RELEASE_HOST}:${STATIC_DIR}/${comp}/install.sh"
    if [ -f "${REPO_ROOT}/clawee-release.pub" ]; then
        scp -q "${REPO_ROOT}/clawee-release.pub" "${RELEASE_HOST}:${STATIC_DIR}/clawee-release.pub"
    fi
    if [ -f "${REPO_ROOT}/site/index.html" ]; then
        scp -q "${REPO_ROOT}/site/index.html" "${RELEASE_HOST}:${STATIC_DIR}/index.html"
    fi

    # (7) marker commit.
    git add "versions/${comp}" "${comp}/install.sh"
    git commit -m "[RELEASED: ${comp}] $(date -u +%Y-%m-%d) ${stamp}"

    echo "✓ released ${tag}"
    echo "  Release: https://github.com/${RELEASE_REPO}/releases/tag/${tag}"
}

for comp in "${COMPONENTS[@]}"; do
    do_release "${comp}"
done

echo
echo "✓ done (${WHAT}${DRY_RUN:+, dry-run=${DRY_RUN}})"
