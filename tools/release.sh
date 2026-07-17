#!/usr/bin/env bash
# release.sh — cut a signed Clawee component release (clawee | claweed).
#
# Usage:
#   bash tools/release.sh <clawee|claweed|all> [--apple] [--dry-run] [--bump-minor|--bump-major]
#   bash tools/release.sh --distribute-only <clawee|claweed> <stamp> [--dry-run]
#
# --distribute-only publishes an already-staged dist/<stamp>/ (produced by
#   `rkit build`, which now owns steps 1-4 below: stamp/build/sum/sign) WITHOUT
#   building, signing, notarizing, or bumping a version — it runs only the
#   publish half (steps 5-7). See distribute_only() further down.
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
#   CLAWEE_R2_CONFIG       path to the R2 config TOML (r2_account_id/r2_bucket)
#                          (default ~/.clawee/release/config.toml)
#   CLAWEE_R2_BUCKET       override the R2 mirror bucket (default: r2_bucket from
#                          the R2 config, else clawee-downloads)
#   CLAWEE_R2_CREDS        path to the R2 S3 creds TOML
#                          (default ~/.clawee/release/r2.key — clawee's own copy of
#                          the token whose content is shared with burrowee)
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

# --distribute-only <comp> <stamp> [--dry-run]: takes its component + stamp as
# positional args right after the flag (not from the general WHAT/comp case
# below), so it's consumed here before the normal arg loop runs.
DISTRIBUTE_ONLY=0
DIST_COMP=""; DIST_STAMP=""
if [ "${1:-}" = "--distribute-only" ]; then
    DISTRIBUTE_ONLY=1
    shift
    DIST_COMP="${1:-}"; DIST_STAMP="${2:-}"
    [ -n "${DIST_COMP}" ] && [ -n "${DIST_STAMP}" ] \
        || { echo "✗ usage: release.sh --distribute-only <clawee|claweed> <stamp> [--dry-run]" >&2; exit 2; }
    shift 2
fi

# ---- args -------------------------------------------------------------------
WHAT=""
DRY_RUN=0
BUMP_KIND="patch"
APPLE_SIGN=""
SKIP_R2="${CLAWEE_SKIP_R2:-}"
for arg in "$@"; do
    # --distribute-only publishes an already-staged dist/ — it takes no
    # build/sign/notarize/bump flags (those already ran in `rkit build`).
    # Accepting them would silently set unused vars and imply behavior
    # (notarization, a version bump) that never happens under this mode.
    if [ "${DISTRIBUTE_ONLY}" = 1 ]; then
        case "${arg}" in
            --dry-run) DRY_RUN=1 ;;
            -h|--help) awk 'NR==1{next} !/^#/{exit} {sub(/^# ?/,""); print}' "$0"; exit 0 ;;
            *) echo "✗ --distribute-only accepts only --dry-run (got '${arg}')" >&2; exit 2 ;;
        esac
        continue
    fi
    case "${arg}" in
        clawee|claweed|all)   WHAT="${arg}" ;;
        --apple)              APPLE_SIGN=1 ;;
        --dry-run)            DRY_RUN=1 ;;
        --bump-minor)         BUMP_KIND="minor" ;;
        --bump-major)         BUMP_KIND="major" ;;
        --no-r2)              SKIP_R2=1 ;;
        # Print the whole header comment (line 2 → the first non-# line), so
        # added doc lines are never silently truncated by a hardcoded range.
        -h|--help)            awk 'NR==1{next} !/^#/{exit} {sub(/^# ?/,""); print}' "$0"; exit 0 ;;
        *) echo "✗ unknown argument: ${arg}" >&2; exit 2 ;;
    esac
done
if [ "${DISTRIBUTE_ONLY}" != 1 ]; then
    [ -n "${WHAT}" ] || { echo "✗ usage: release.sh <clawee|claweed|all> [--apple] [--dry-run] [--no-r2] [--bump-minor|--bump-major]" >&2; exit 2; }
fi
export APPLE_SIGN

# ---- config / defaults ------------------------------------------------------
RELEASE_HOST="${RELEASE_HOST:-nsm.renative.com}"
STATIC_DIR="${STATIC_DIR:-/ebs_storage/apps/release.clawee.org/static}"
RELEASE_REPO="${CLAWEE_RELEASE_REPO:-clawee-git/release}"

# ---- Cloudflare edge purge of version.js -------------------------------------
# The clawee.org version badge reads <comp>/version.js via JSONP, but the
# clawee.org CF zone caches .js (4h browser TTL) — without a purge the badge
# can show the previous release for hours. Best-effort: needs a CACHE-PURGE-
# capable CF token on the release host (the certbot DNS token at
# cloudflare-clawee.ini CANNOT purge — scope is DNS-only). Silently skipped
# when the token file is absent; a failed purge only warns (the badge
# self-heals when the TTL lapses).
CF_ZONE_CLAWEE_ORG="${CF_ZONE_CLAWEE_ORG:-34317b4a011c53b64f3a1fe45a651fee}"
CF_PURGE_TOKEN_FILE="${CF_PURGE_TOKEN_FILE:-/etc/cloud-certs/cloudflare-clawee-cache.ini}"
purge_cf_version_js() {
    local comp="$1"
    # shellcheck disable=SC2029  # comp/zone/file are local, controlled values — client-side expansion into the remote command is intended.
    ssh "${RELEASE_HOST}" "
        [ -f '${CF_PURGE_TOKEN_FILE}' ] || exit 0
        TOKEN=\$(grep -oE '[A-Za-z0-9_-]{30,}' '${CF_PURGE_TOKEN_FILE}' | head -1)
        [ -n \"\$TOKEN\" ] || exit 0
        curl -s -X POST -H \"Authorization: Bearer \$TOKEN\" -H 'Content-Type: application/json' \
            'https://api.cloudflare.com/client/v4/zones/${CF_ZONE_CLAWEE_ORG}/purge_cache' \
            --data '{\"files\":[\"https://release.clawee.org/${comp}/version.js\"]}' \
            | grep -q '\"success\":true' \
            && echo '✓ CF edge purge: ${comp}/version.js' \
            || echo '⚠ CF purge failed (token lacks Cache Purge scope?) — badge follows the 4h TTL'
    " >&2 || true
}
DP_DIR="${DP_DIR:-${REPO_ROOT}/../../../release.dp/code/release.dp}"
AGE_KEY_AGE="${DP_DIR}/clawee-release.key.age"
AGE_IDENTITY="${AGE_IDENTITY:-${HOME}/.age/clawee-release.txt}"

# ---- R2 mirror config (public downloads.clawee.org) -------------------------
# R2 is a MIRROR of the GitHub release — GitHub is the primary channel. clawee's
# R2 config lives OUTSIDE any repo at ~/.clawee/release/ (mirroring burrowee's
# ~/.burrowee/release/): config.toml holds the non-secret r2_account_id/r2_bucket,
# r2.key holds the S3 access_key_id/secret_access_key (clawee's own copy of the
# token whose CONTENT is shared with burrowee — rotate both copies together). Any
# missing piece → the mirror is SKIPPED, never fatal.
R2_CONFIG="${CLAWEE_R2_CONFIG:-${HOME}/.clawee/release/config.toml}"
R2_CREDS="${CLAWEE_R2_CREDS:-${HOME}/.clawee/release/r2.key}"

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
        clawee)   printf '%s' "clawee clawee-updater" ;;
        claweed)  printf '%s' "claweed clawee-spawn claweed-updater" ;;
    esac
}

GHP="$(command -v ghp 2>/dev/null || echo "${HOME}/.claude/bin/ghp")"

# ---- pre-flight -------------------------------------------------------------
# Skipped entirely under --distribute-only: no build/sign/notarize happens
# there (that already ran upstream in `rkit build`), so none of zip/unzip/
# minisign/age, Apple-sign resolution, the per-component source-cleanliness/
# branch checks, or signing-key resolution are needed. distribute_only()
# (further down) does its own light `src` existence check + the ghp checks it
# actually needs.
if [ "${DISTRIBUTE_ONLY}" != 1 ]; then
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
    # Assert the identity is REACHABLE, not that it sits in the keychain: since
    # 2026-07-17 modernech-sign's default `auto` mode prefers its rcodesign
    # disk-key backend (decrypting the age-sealed .p12 at sign time), where the
    # identity never enters a keychain at all. A keychain-presence assertion is
    # therefore wrong under the mode we normally sign in, and it hard-fails every
    # cut from a harness/SSH session, whose macOS security session is detached (its
    # keychain search list is System-only, so the login keychain is unreachable).
    # modernech-sign stays the source of truth for WHICH backend runs; this only
    # fails fast when neither backend could possibly work.
    if ! command -v rcodesign >/dev/null 2>&1 \
        && ! security find-identity -v -p codesigning 2>/dev/null | grep -q "$("${SIGN_BIN}" id)"; then
        echo "✗ Developer ID identity unreachable: $("${SIGN_BIN}" id)" >&2
        echo "  rcodesign (disk-key backend) is not on PATH and the identity is not in this session's keychain." >&2
        echo "  Install rcodesign (cargo install apple-codesign) or sign from a GUI Terminal session." >&2
        exit 1
    fi
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
fi # DISTRIBUTE_ONLY != 1 (pre-flight)

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
    local zips=() pair os arch out_bins assemble asset b guard_paths
    for pair in "${TARGETS[@]}"; do
        read -r os arch <<<"${pair}"
        out_bins="${stage}/.bins-${os}-${arch}"
        mkdir -p "${out_bins}"

        # component bins
        COMP="${comp}" SRC_DIR="${src}" TARGETOS="${os}" TARGETARCH="${arch}" \
            STAMP="${stamp}" OUT_DIR="${out_bins}" GO_BIN="${GO_BIN}" \
            bash "${REPO_ROOT}/tools/build.sh" >&2

        # env-config guard: no freshly built binary may embed a forbidden
        # config-env literal (CLAWEE_DATA_DIR/CLAWEE_SOCKET/CLAWEE_SPAWN_HELPER/
        # mustEnv — see tools/verify-no-env.sh). A hit aborts the cut here,
        # before anything is signed or published (the ERR trap reverts the
        # version bump).
        guard_paths=()
        # shellcheck disable=SC2086  # ${bins} is an intentional space-list of bin names from bins_for(); word-splitting is the point.
        for b in ${bins}; do guard_paths+=("${out_bins}/${b}"); done
        bash "${REPO_ROOT}/tools/verify-no-env.sh" "${guard_paths[@]}" >&2

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
        # Explicit revert: plain `exit 1` fires only the EXIT trap (shred_key) —
        # the ERR trap does NOT run on `exit`, so without this the bumped
        # versions/<comp> would stay staged and the next cut would double-bump.
        echo "✗ tag ${tag} already exists locally — reverting version" >&2
        revert_version
        exit 1
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

    # (6) regenerate bootstraps + the version JSONP, then scp the static surface.
    bash "${REPO_ROOT}/tools/gen-bootstraps.sh" >&2
    # Sources the just-published version from the R2 catalog (mirrored in 5b above).
    bash "${REPO_ROOT}/tools/gen-version-jsonp.sh" "${comp}" >&2

    # shellcheck disable=SC2029  # ${STATIC_DIR}/${comp} are local, controlled values — expanding client-side into the remote command is intended.
    ssh "${RELEASE_HOST}" "mkdir -p '${STATIC_DIR}/${comp}'"
    scp -q "${REPO_ROOT}/${comp}/install.sh" "${RELEASE_HOST}:${STATIC_DIR}/${comp}/install.sh"
    scp -q "${REPO_ROOT}/${comp}/version.js" "${RELEASE_HOST}:${STATIC_DIR}/${comp}/version.js"
    purge_cf_version_js "${comp}"
    if [ -f "${REPO_ROOT}/clawee-release.pub" ]; then
        scp -q "${REPO_ROOT}/clawee-release.pub" "${RELEASE_HOST}:${STATIC_DIR}/clawee-release.pub"
    fi
    if [ -f "${REPO_ROOT}/site/index.html" ]; then
        scp -q "${REPO_ROOT}/site/index.html" "${RELEASE_HOST}:${STATIC_DIR}/index.html"
    fi

    # (7) marker commit.
    git add "versions/${comp}" "${comp}/install.sh" "${comp}/version.js"
    git commit -m "[RELEASED: ${comp}] $(date -u +%Y-%m-%d) ${stamp}"

    echo "✓ released ${tag}"
    echo "  Release: https://github.com/${RELEASE_REPO}/releases/tag/${tag}"
}

# ---- distribute_only: distribution-only mode over an already-staged
# dist/<stamp>/ (produced by `rkit build` — the produce half lives there now).
# Runs ONLY: tag + GitHub Release -> mirror_to_r2 -> gen-bootstraps.sh ->
# gen-version-jsonp.sh -> scp install.sh/version.js/pubkey/site to the release
# host -> [RELEASED] marker commit. No build, no sign, no notarize, no version
# bump — all of that already happened upstream in `rkit build`. clawee has no
# console/register step (unlike Burrowee) — nothing to skip there.
#
# The tag/notes/`ghp release create` block below is a DELIBERATE COPY of
# do_release()'s step 5, not a shared helper: do_release's tag-exists path
# does an EXPLICIT `revert_version` (see the comment there — a plain `exit 1`
# only fires the EXIT trap, not ERR) to undo its version bump. distribute_only
# never bumps a version, so that revert is meaningless here; factoring a
# shared helper would either strip the revert out of do_release (a behavior
# change to the existing full-cut path — risky) or carry dead revert-adjacent
# logic into distribute_only. A copy keeps both paths exactly as narrow as
# they need to be, and do_release is untouched.
#
# On --dry-run: validates the staged dir + component, then prints "would: ..."
# for every publish action and returns — no ghp/git/ssh/scp/network writes.
distribute_only() {
    local comp="$1" stamp="$2"
    case "${comp}" in
        clawee|claweed) ;;
        *) echo "✗ unknown component: ${comp}" >&2; exit 1 ;;
    esac

    local stage="${REPO_ROOT}/dist/${stamp}"
    [ -d "${stage}" ] || { echo "✗ staged dir missing: ${stage} (run rkit build first)" >&2; exit 1; }
    for f in SHA256SUMS.txt SHA256SUMS.txt.minisig; do
        [ -f "${stage}/${f}" ] || { echo "✗ missing ${f} in ${stage} (rkit build must produce it)" >&2; exit 1; }
    done
    compgen -G "${stage}/clawee-${comp}-*.zip" >/dev/null \
        || { echo "✗ no clawee-${comp}-*.zip found in ${stage} (rkit build must produce it)" >&2; exit 1; }

    local src semver
    src="$(src_for "${comp}")"
    [ -d "${src}" ] || { echo "✗ ${comp} source worktree missing: ${src}" >&2; exit 1; }
    semver="$(SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --semver)"

    if [ "${DRY_RUN}" = 1 ]; then
        echo "would: gh release create ${comp}/${stamp} (GitHub Release, public) via ghp"
        echo "would: mirror_to_r2 ${comp} ${stamp} ${semver} (downloads.clawee.org)"
        echo "would: gen-bootstraps.sh (regenerate ${comp}/install.sh)"
        echo "would: gen-version-jsonp.sh ${comp} (regenerate ${comp}/version.js)"
        echo "would: scp install.sh/version.js/clawee-release.pub/site/index.html to ${RELEASE_HOST}:${STATIC_DIR}/${comp}/"
        echo "would: marker commit [RELEASED: ${comp}] ${stamp}"
        echo "✓ dry-run distribute-only: no real writes"
        return 0
    fi

    command -v ghp >/dev/null 2>&1 || { echo "✗ required tool not found: ghp" >&2; exit 1; }
    [ -x "${GHP}" ] || { echo "✗ ghp wrapper not found at ${GHP}" >&2; exit 1; }
    "${GHP}" repo view "${RELEASE_REPO}" --json name >/dev/null 2>&1 \
        || { echo "✗ ghp cannot access ${RELEASE_REPO} — check gh.account + auth" >&2; exit 1; }
    # Same upfront reachability check the full-cut path runs (see the
    # pre-flight block above) — without it a down host fails fast only at the
    # scp below, AFTER the tag + GitHub Release + R2 mirror already published.
    ssh -o BatchMode=yes -o ConnectTimeout=5 "${RELEASE_HOST}" 'true' 2>/dev/null \
        || { echo "✗ cannot ssh to ${RELEASE_HOST}" >&2; exit 1; }

    # (1) tag + GitHub Release — copied from do_release's step 5 (see the doc
    # comment above for why this isn't a shared helper).
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
        echo "✗ tag ${tag} already exists locally" >&2
        exit 1
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

    # (2) mirror to R2 (non-fatal by design — see mirror_to_r2's doc comment).
    mirror_to_r2 "${comp}" "${stamp}" "${semver}" "${stage}"

    # (3) regenerate bootstraps + version JSONP, then scp the static surface —
    # mirrors do_release()'s step 6 verbatim.
    bash "${REPO_ROOT}/tools/gen-bootstraps.sh" >&2
    bash "${REPO_ROOT}/tools/gen-version-jsonp.sh" "${comp}" >&2

    # shellcheck disable=SC2029  # ${STATIC_DIR}/${comp} are local, controlled values — expanding client-side into the remote command is intended.
    ssh "${RELEASE_HOST}" "mkdir -p '${STATIC_DIR}/${comp}'"
    scp -q "${REPO_ROOT}/${comp}/install.sh" "${RELEASE_HOST}:${STATIC_DIR}/${comp}/install.sh"
    scp -q "${REPO_ROOT}/${comp}/version.js" "${RELEASE_HOST}:${STATIC_DIR}/${comp}/version.js"
    purge_cf_version_js "${comp}"
    if [ -f "${REPO_ROOT}/clawee-release.pub" ]; then
        scp -q "${REPO_ROOT}/clawee-release.pub" "${RELEASE_HOST}:${STATIC_DIR}/clawee-release.pub"
    fi
    if [ -f "${REPO_ROOT}/site/index.html" ]; then
        scp -q "${REPO_ROOT}/site/index.html" "${RELEASE_HOST}:${STATIC_DIR}/index.html"
    fi

    # (4) marker commit.
    git add "versions/${comp}" "${comp}/install.sh" "${comp}/version.js"
    git commit -m "[RELEASED: ${comp}] $(date -u +%Y-%m-%d) ${stamp}"

    echo "✓ distributed ${tag}"
    echo "  Release: https://github.com/${RELEASE_REPO}/releases/tag/${tag}"
}

if [ "${DISTRIBUTE_ONLY}" = 1 ]; then
    distribute_only "${DIST_COMP}" "${DIST_STAMP}"
    exit 0
fi

for comp in "${COMPONENTS[@]}"; do
    do_release "${comp}"
done

echo
echo "✓ done (${WHAT}${DRY_RUN:+, dry-run=${DRY_RUN}})"
