#!/usr/bin/env bash
# release.sh — cut a signed Clawee component release (clawee | claweed).
#
# Usage:
#   bash tools/release.sh <clawee|claweed|all> [--channel stable|beta] [--apple] [--vulncheck|--public] [--dry-run] [--bump-minor|--bump-major]
#   bash tools/release.sh --distribute-only <clawee|claweed> <stamp> [--channel stable|beta] [--dry-run]
#
# A CUT DOES NOT PUBLISH. It uploads to the PRIVATE staging bucket and records
#   a `staged` catalog row with the manage service; going live is a separate,
#   operator-only act (promote). There is no release tag, no GitHub Release, no
#   public-bucket write, no latest.json and no scp anywhere in this script.
#   See ~/.agents/guidelines/release-management.md.
#
# --distribute-only stages an already-built dist/<stamp>/ (produced by
#   `rkit build`, which owns steps 1-4 below: stamp/build/sum/sign) WITHOUT
#   building, signing, notarizing, or bumping a version — it runs only the
#   stage half (steps 5-7). See distribute_only() further down.
#
# --apple: Developer ID sign the darwin binaries (modernech-sign, Modernech LLC)
#   + notarize each darwin zip before publishing. WITHOUT it darwin bins are
#   ad-hoc signed (the default) — fine for curl-install (no quarantine xattr).
#   NOTE: --apple ALONE signs+notarizes but SKIPS the CVE gate; for a public,
#   browser-downloadable release use --public (signing + govulncheck).
#   --apple on its own is the conscious sign-only exception. Guideline:
#   ~/.claude/guidelines/APPLE-SIGNING.md.
#
# --vulncheck: hard-gate the cut on govulncheck — scans every shipped module
#   (clawee + claweed source, GOWORK=off) and aborts on any finding.
#   --public is shorthand for --apple --vulncheck (the standard ship path).
#   Neither flag + an interactive TTY prompts to cut a public release (both);
#   a non-interactive run or a "no" answer skips both.
#   (--public-release is kept as a back-compat alias for --public.)
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
#   5. (non-dry-run) uploads the artifacts to the PRIVATE staging bucket under
#      <comp>/<channel>/<stamp>/ — no manifest, so nothing public changes.
#   6. (non-dry-run) registers the `staged` catalog row with the manage service
#      (nonce -> sign with the release key -> POST) and reports the row's URL.
#   7. (non-dry-run) regenerates the bootstraps (they embed no version) and
#      records a [RELEASED: <comp>] … (staged) marker commit.
#
# On --dry-run steps 1-4 run for real, steps 5-6 run in THEIR dry-run modes
# (printing the staging keys and the register payload, touching nothing), and
# the version bump is REVERTED — the tree is left exactly as it was, just with
# throwaway artifacts under dist/<stamp>/.
#
# --channel stable|beta selects the staging prefix and the catalog row's
#   channel. It defaults to beta when the component source is on a beta branch
#   and stable otherwise; asking for --channel stable from a beta branch is
#   refused rather than quietly mislabelling the row.
#
# claweed inner installer: rendered at build time from the daemon repo's CANONICAL
# install/install.sh.in (the sudo-minimal installer), substituting the stamp. The
# release zip is NEVER allowed to ship a forked copy — it is always the daemon's
# template, version-stamped, so the served installer can't drift from source.
#
# Env (all optional — sane defaults below):
#   DP_DIR                 path to the release.dp secrets repo
#   SIGN_KEY               minisign secret key file (overrides the default resolution)
#   AGE_IDENTITY           age identity file used to decrypt the real signing key
#                          (default ~/.age/clawee-release.txt — created at activation)
#   CLAWEE_SRC_CLAWEE      clawee component source worktree (default: cli main worktree)
#   CLAWEE_SRC_CLAWEED     claweed component source worktree (default: daemon main worktree)
#   CLAWEE_RELEASE_YES     skip the interactive minor/major bump confirm
#   CLAWEE_R2_CONFIG       path to the R2 config TOML (r2_account_id, staging_bucket,
#                          manage_url) (default ~/.clawee/release/config.toml)
#   CLAWEE_R2_STAGING_BUCKET  override the PRIVATE staging bucket (default:
#                          staging_bucket from the R2 config). There is no
#                          fallback default: staging into a guessed bucket is
#                          either a failure or a public write.
#   CLAWEE_MANAGE_URL      override the manage service base URL (default:
#                          manage_url from the R2 config)
#   CLAWEE_R2_CREDS        path to the R2 S3 creds TOML
#                          (default ~/.clawee/release/r2.key — clawee's own copy of
#                          the token whose content is shared with burrowee)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${REPO_ROOT}"

# ---- go on PATH (the Clawee per-dir hook strips /opt/homebrew/bin) -----------
GO_BIN="${GO_BIN:-go}"
command -v "${GO_BIN}" >/dev/null 2>&1 || GO_BIN=/opt/homebrew/bin/go
export GO_BIN

# shellcheck source=tools/vulncheck.sh
source "${REPO_ROOT}/tools/vulncheck.sh"
# shellcheck source=tools/module_gate.sh
source "${REPO_ROOT}/tools/module_gate.sh"

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
        || { echo "✗ usage: release.sh --distribute-only <clawee|claweed> <stamp> [--channel stable|beta] [--dry-run]" >&2; exit 2; }
    shift 2
fi

# ---- args -------------------------------------------------------------------
WHAT=""
DRY_RUN=0
BUMP_KIND="patch"

# --- Apple account plugin (project-specific) ---------------------------------
# Resolves the account plugin and exports APPLE_ACCOUNT, APPLE_HOME and
# APPLE_ACCOUNT_DIR for modernech-sign. Both values resolve the same way — an
# exported variable first, then a per-product config file:
#
#   account   APPLE_ACCOUNT → config/apple-account   (the plugin folder name)
#   home      APPLE_HOME    → config/apple-home      (the absolute plugin root)
#
# or APPLE_ACCOUNT_DIR names the account's folder directly and settles both.
# The config files are gitignored, which is what lets this PUBLIC repo resolve a
# machine path without carrying one. $HOME is never consulted: it is unset under
# launchd, cron and a detached harness session, where a $HOME-derived default
# silently goes RELATIVE. cmd/rkit/apple_account.go is the Go twin of this
# resolution and shares the same precedence.
#
# Called only when Apple signing was requested, so every unresolved state exits
# 1. This used to return 0 on a missing config and merely WARN on a missing
# plugin folder, then sign anyway — producing an AD-HOC cut the operator
# believes is Developer-ID signed and notarized.
_first_config_line() {
    [ -f "$1" ] || return 0
    sed -n '/^[[:space:]]*#/d;/^[[:space:]]*$/d;p;q' "$1" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//'
}

_apple_refuse() {
    echo "  refusing to continue: signing with no account plugin produces an AD-HOC build" >&2
    echo "  that looks Developer-ID signed." >&2
    exit 1
}

load_apple_account() {
    if [ -n "${APPLE_ACCOUNT_DIR:-}" ]; then
        if [ ! -d "${APPLE_ACCOUNT_DIR}" ]; then
            echo "✗ Apple signing requested but APPLE_ACCOUNT_DIR points at" >&2
            echo "  ${APPLE_ACCOUNT_DIR}, which is not a directory" >&2
            _apple_refuse
        fi
        export APPLE_ACCOUNT_DIR
        echo "→ Apple account dir (from APPLE_ACCOUNT_DIR): ${APPLE_ACCOUNT_DIR}" >&2
        return 0
    fi

    local conf="${REPO_ROOT}/config/apple-account"
    [ -f "$conf" ] || conf="${REPO_ROOT}/config/apple.account"
    export APPLE_ACCOUNT="${APPLE_ACCOUNT:-$(_first_config_line "$conf")}"
    if [ -z "${APPLE_ACCOUNT}" ]; then
        echo "✗ Apple signing requested but APPLE_ACCOUNT is unresolved" >&2
        echo "  supply it as one of:" >&2
        echo "    ${REPO_ROOT}/config/apple-account" >&2
        echo "        one line: the Apple account plugin folder name" >&2
        echo "    \$APPLE_ACCOUNT      the same value, from the environment" >&2
        echo "    \$APPLE_ACCOUNT_DIR  the account's folder itself, settling both" >&2
        _apple_refuse
    fi

    local home_conf="${REPO_ROOT}/config/apple-home"
    [ -f "$home_conf" ] || home_conf="${REPO_ROOT}/config/apple.home"
    export APPLE_HOME="${APPLE_HOME:-$(_first_config_line "$home_conf")}"
    if [ -z "${APPLE_HOME}" ]; then
        echo "✗ Apple signing requested but APPLE_HOME is unresolved" >&2
        echo "  supply it as one of:" >&2
        echo "    ${REPO_ROOT}/config/apple-home" >&2
        echo "        one line: the absolute directory holding one folder per Apple account" >&2
        echo "    \$APPLE_HOME         the same value, from the environment" >&2
        echo "    \$APPLE_ACCOUNT_DIR  the account's folder itself, settling both" >&2
        _apple_refuse
    fi
    case "${APPLE_HOME}" in
        /*) ;;
        *)  echo "✗ Apple signing requested but APPLE_HOME resolved to" >&2
            echo "  \"${APPLE_HOME}\", which is not an absolute path" >&2
            _apple_refuse ;;
    esac

    export APPLE_ACCOUNT_DIR="${APPLE_HOME}/${APPLE_ACCOUNT}"
    if [ ! -d "$APPLE_ACCOUNT_DIR" ]; then
        echo "✗ Apple signing requested but the account plugin folder points at" >&2
        echo "  ${APPLE_ACCOUNT_DIR}, which is not a directory" >&2
        _apple_refuse
    fi
    echo "→ Apple account: $APPLE_ACCOUNT ($APPLE_ACCOUNT_DIR)" >&2
}

APPLE_SIGN=""
VULNCHECK=""
# CHANNEL empty means "derive from the component source branch" (resolve_channel
# below). An explicit --channel is a claim about what is being cut, which is why
# an explicit `stable` from a beta branch is refused rather than honoured.
CHANNEL=""
CHANNEL_EXPLICIT=0
_want_channel=0
for arg in "$@"; do
    if [ "${_want_channel}" = 1 ]; then
        CHANNEL="${arg}"; CHANNEL_EXPLICIT=1; _want_channel=0
        case "${CHANNEL}" in
            stable|beta) ;;
            *) echo "✗ --channel must be stable or beta (got '${CHANNEL}')" >&2; exit 2 ;;
        esac
        continue
    fi
    # --distribute-only publishes an already-staged dist/ — it takes no
    # build/sign/notarize/bump flags (those already ran in `rkit build`).
    # Accepting them would silently set unused vars and imply behavior
    # (notarization, a version bump) that never happens under this mode.
    if [ "${DISTRIBUTE_ONLY}" = 1 ]; then
        case "${arg}" in
            --dry-run) DRY_RUN=1 ;;
            --channel) _want_channel=1 ;;
            -h|--help) awk 'NR==1{next} !/^#/{exit} {sub(/^# ?/,""); print}' "$0"; exit 0 ;;
            *) echo "✗ --distribute-only accepts only --channel and --dry-run (got '${arg}')" >&2; exit 2 ;;
        esac
        continue
    fi
    case "${arg}" in
        clawee|claweed|all)   WHAT="${arg}" ;;
        --apple)              APPLE_SIGN=1 ;;
        --vulncheck)          VULNCHECK=1 ;;
        --public)             APPLE_SIGN=1; VULNCHECK=1 ;;
        --public-release)     APPLE_SIGN=1; VULNCHECK=1 ;;  # back-compat alias for --public
        --dry-run)            DRY_RUN=1 ;;
        --bump-minor)         BUMP_KIND="minor" ;;
        --bump-major)         BUMP_KIND="major" ;;
        --channel)            _want_channel=1 ;;
        # Print the whole header comment (line 2 → the first non-# line), so
        # added doc lines are never silently truncated by a hardcoded range.
        -h|--help)            awk 'NR==1{next} !/^#/{exit} {sub(/^# ?/,""); print}' "$0"; exit 0 ;;
        *) echo "✗ unknown argument: ${arg}" >&2; exit 2 ;;
    esac
done
[ "${_want_channel}" = 0 ] || { echo "✗ --channel needs a value (stable | beta)" >&2; exit 2; }
if [ "${DISTRIBUTE_ONLY}" != 1 ]; then
    [ -n "${WHAT}" ] || { echo "✗ usage: release.sh <clawee|claweed|all> [--channel stable|beta] [--apple] [--vulncheck|--public] [--dry-run] [--bump-minor|--bump-major]" >&2; exit 2; }
    # When neither signing nor the CVE gate was requested and we're interactive,
    # offer the --public path (both). Non-TTY or a "no" answer → dev/testing.
    # --distribute-only never signs or CVE-gates (that already ran upstream in
    # `rkit build`), so it skips this prompt entirely.
    PROMPT_ANS=""
    if [ -z "${APPLE_SIGN}" ] && [ -z "${VULNCHECK}" ] && [ -t 0 ]; then
        printf 'Cut a PUBLIC release? — Developer-ID signing + CVE gate  [y/N] ' >&2
        read -r PROMPT_ANS || PROMPT_ANS=""
    fi
    _mode="$(resolve_release_mode "${APPLE_SIGN}" "${VULNCHECK}" "${PROMPT_ANS}")"
    APPLE_SIGN="${_mode%%|*}"; VULNCHECK="${_mode#*|}"
fi
export APPLE_SIGN VULNCHECK
[ -n "${APPLE_SIGN}" ] && load_apple_account

# ---- config / defaults ------------------------------------------------------
# RELEASE_HOST, STATIC_DIR and RELEASE_REPO are deliberately GONE. The cut
# touches neither the static surface nor GitHub: regenerating version.js,
# scp'ing install.sh and creating the release are go-live acts, and they moved
# to promote. Nothing here may name a host or a GitHub repo.
DP_DIR="${DP_DIR:-${REPO_ROOT}/../../../release.dp/code/main}"
AGE_KEY_AGE="${DP_DIR}/clawee-release.key.age"
AGE_IDENTITY="${AGE_IDENTITY:-${HOME}/.age/clawee-release.txt}"

# ---- staging store + manage service config ----------------------------------
# clawee's release config lives OUTSIDE any repo at ~/.clawee/release/ (mirroring
# burrowee's ~/.burrowee/release/): config.toml holds the non-secret
# r2_account_id, the PRIVATE staging_bucket and the manage_url; r2.key holds the
# S3 access_key_id/secret_access_key (clawee's own copy of the token whose
# CONTENT is shared with burrowee — rotate both copies together).
#
# A MISSING PIECE IS NOW FATAL, which is the opposite of the old mirror posture
# and deliberately so: the staging upload is the cut's ONLY publication. Under
# the old shape R2 was a mirror behind GitHub Releases, so an unconfigured box
# could skip it and still have produced a reachable release. There is no such
# fallback any more — a skipped upload is a cut that produced nothing, and a
# skipped registration is bytes nobody can find.
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
SRC_CLAWEE="${CLAWEE_SRC_CLAWEE:-${CC}/cli/code/main}"
SRC_CLAWEED="${CLAWEE_SRC_CLAWEED:-${CC}/daemon/code/main}"

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

# binary list per component (used at assembly time to copy into the zip).
# Must match tools/build.sh's MAP exactly — a name here that build.sh does not
# produce fails the cut at the copy step, after the version bump. claweed is TWO
# binaries: the setuid-root clawee-spawn helper is retired (the daemon runs as
# root and forks its own per-user children) and its package is gone.
bins_for() {
    case "$1" in
        clawee)   printf '%s' "clawee clawee-updater" ;;
        claweed)  printf '%s' "claweed claweed-updater" ;;
    esac
}

# ---- staging + registration -------------------------------------------------
# THIS BLOCK MUST STAY ABOVE THE PRE-FLIGHT. The pre-flight calls
# require_manage_url, and bash resolves a function name only if it has already
# been read: defined below, every non-dry-run cut died with "command not found"
# (exit 127) before the first build, and no suite caught it because every suite
# stayed on the --dry-run path. tools/test-cut-no-publish.sh PART D now runs a
# stubbed non-dry-run pre-flight for exactly this reason.
# resolve_channel <comp> — echo the channel this cut belongs to.
#
# An explicit --channel wins EXCEPT when it claims `stable` for a source tree
# sitting on a beta branch: that combination is always a mistake, and honouring
# it files beta bytes in the stable catalog where retention and every installer
# treat them as the real thing. Without a flag, the branch decides.
resolve_channel() {
    local comp="$1" src br derived
    src="$(src_for "${comp}")"
    br="$(git -C "${src}" rev-parse --abbrev-ref HEAD 2>/dev/null || echo "")"
    case "${br}" in
        beta|beta-*) derived="beta" ;;
        *)           derived="stable" ;;
    esac
    if [ "${CHANNEL_EXPLICIT}" = 1 ]; then
        if [ "${CHANNEL}" = stable ] && [ "${derived}" = beta ]; then
            echo "✗ --channel stable requested but ${comp} source is on branch '${br}'" >&2
            echo "  A beta tree cut into the stable channel is a mislabelled row: retention" >&2
            echo "  and every installer would treat it as a stable release. Cut it as beta," >&2
            echo "  or cut from a non-beta branch." >&2
            exit 2
        fi
        printf '%s' "${CHANNEL}"
        return 0
    fi
    printf '%s' "${derived}"
}

# require_manage_url — resolve MANAGE_URL or refuse, naming the key.
#
# Called before the upload, never after: a refusal here costs nothing, the same
# refusal after the upload costs a stranded artifact.
MANAGE_URL=""
require_manage_url() {
    MANAGE_URL="${CLAWEE_MANAGE_URL:-$(toml_get "${R2_CONFIG}" manage_url || true)}"
    [ -n "${MANAGE_URL}" ] && return 0
    echo "✗ no manage service URL configured — refusing to cut." >&2
    echo "  A cut uploads to the PRIVATE staging bucket and then registers a" >&2
    echo "  'staged' catalog row. Without the row the uploaded bytes are a" >&2
    echo "  stranded artifact: nothing lists them and nothing can promote them." >&2
    echo "  Set it as one of:" >&2
    echo "    manage_url = \"<manage URL>\"   in ${R2_CONFIG}" >&2
    echo "    \$CLAWEE_MANAGE_URL             the same value, from the environment" >&2
    exit 1
}

# stage_to_staging <comp> <stamp> <semver> <stage_dir> <channel> [--dry-run]
#
# Uploads the four zips + SHA256SUMS.txt + its signature to the PRIVATE staging
# bucket under <comp>/<channel>/<stamp>/, with --no-manifest so nothing names
# the new stamp as current. Every unresolved piece is FATAL — see the config
# block's note: this upload is the cut's only publication, so "skipped" and
# "failed" are the same outcome and neither may pass for success.
#
# Touching this function's branches → run tools/test-stage-fail-closed.sh.
stage_to_staging() {
    local comp="$1" stamp="$2" semver="$3" stage="$4" channel="$5" dry="${6:-}"
    local account bucket
    account="$(toml_get "${R2_CONFIG}" r2_account_id || true)"
    bucket="${CLAWEE_R2_STAGING_BUCKET:-$(toml_get "${R2_CONFIG}" staging_bucket || true)}"
    if [ -n "${dry}" ]; then
        # --dry-run needs neither creds nor an account: it prints the keys the
        # cut WOULD write and uploads nothing.
        ( cd "${REPO_ROOT}/tools/r2-mirror" && "${GO_BIN}" run . \
            --bucket "${bucket:-<staging bucket>}" --prefix "${channel}" --no-manifest \
            --stage-dir "${stage}" --comp "${comp}" \
            --version "${semver}" --stamp "${stamp}" --dry-run )
        return $?
    fi
    if [ -z "${account}" ]; then
        echo "✗ no r2_account_id in ${R2_CONFIG} — cannot reach the staging bucket." >&2
        return 1
    fi
    if [ -z "${bucket}" ]; then
        echo "✗ no staging_bucket in ${R2_CONFIG} (or \$CLAWEE_R2_STAGING_BUCKET)." >&2
        echo "  There is deliberately no default: a guessed bucket name is either a" >&2
        echo "  failed upload or a write to the PUBLIC bucket, and the second one" >&2
        echo "  publishes a build nobody approved." >&2
        return 1
    fi
    if [ ! -f "${R2_CREDS}" ]; then
        echo "✗ R2 creds not found at ${R2_CREDS} (set \$CLAWEE_R2_CREDS)." >&2
        return 1
    fi
    echo "→ staging ${comp} ${stamp} → ${bucket}:${comp}/${channel}/${stamp}/ (private)"
    if ( cd "${REPO_ROOT}/tools/r2-mirror" && "${GO_BIN}" run . \
            --account "${account}" --bucket "${bucket}" \
            --prefix "${channel}" --no-manifest \
            --stage-dir "${stage}" --comp "${comp}" \
            --version "${semver}" --stamp "${stamp}" \
            --creds "${R2_CREDS}" ); then
        return 0
    fi
    echo "✗ staging upload FAILED for ${comp} ${stamp} — stopping here." >&2
    echo "  State: NOTHING was published (the cut never publishes), no catalog row" >&2
    echo "  was registered, the bootstraps were not regenerated and no [RELEASED]" >&2
    echo "  marker was committed. Any objects that did land under" >&2
    echo "  ${comp}/${channel}/${stamp}/ are unreferenced and harmless." >&2
    echo "  Re-run the same cut: it is idempotent up to the version bump, and" >&2
    echo "  --distribute-only ${comp} ${stamp} re-stages the existing dist/ dir" >&2
    echo "  without rebuilding." >&2
    return 1
}

# register_staged <comp> <stamp> <semver> <stage_dir> <channel> [--dry-run]
#
# Fails the cut on a refusal — AFTER the upload, deliberately. The uploaded
# bytes are inert without a row (nothing lists a bucket the manage service does
# not know about), so the failure to surface is the missing row, not the
# objects.
register_staged() {
    local comp="$1" stamp="$2" semver="$3" stage="$4" channel="$5" dry="${6:-}"
    local args=(--comp "${comp}" --channel "${channel}" --version "${semver}"
                --stamp "${stamp}" --stage-dir "${stage}")
    if [ -n "${dry}" ]; then
        ( cd "${REPO_ROOT}" && "${GO_BIN}" run ./cmd/clawee-release-register \
            "${args[@]}" --manage-url "${MANAGE_URL:-<manage URL>}" --dry-run )
        return $?
    fi
    if ( cd "${REPO_ROOT}" && "${GO_BIN}" run ./cmd/clawee-release-register \
            "${args[@]}" --manage-url "${MANAGE_URL}" --key "${SIGN_KEY}" ); then
        return 0
    fi
    echo "✗ registering ${comp} ${stamp} with ${MANAGE_URL} FAILED." >&2
    echo "  The artifacts ARE in the staging bucket under ${comp}/${channel}/${stamp}/," >&2
    echo "  but no catalog row names them, so nothing can list or promote them." >&2
    echo "  Nothing public changed and no marker commit was made." >&2
    echo "  Fix the service (or the URL) and re-run:" >&2
    echo "    bash tools/release.sh --distribute-only ${comp} ${stamp} --channel ${channel}" >&2
    return 1
}

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
    #
    # LOOK WHERE cargo PUTS IT, not only on PATH. `cargo install apple-codesign`
    # lands rcodesign in ~/.cargo/bin, which is on an interactive shell's PATH via
    # the cargo env snippet and absent from a harness/SSH environment — exactly the
    # session this check was written to keep working. A bare `command -v` therefore
    # reported "no backend possible" on a host where the disk-key backend was
    # installed and functional, and refused a cut that would have succeeded. The
    # message was true ("not on PATH") and the conclusion it justified was not.
    # Same two-step shape modernech-sign gets above and govulncheck gets in
    # vulncheck.sh: probe PATH, then the canonical install location.
    RCODESIGN="${RCODESIGN:-rcodesign}"
    command -v "${RCODESIGN}" >/dev/null 2>&1 || RCODESIGN="${HOME}/.cargo/bin/rcodesign"
    if ! { command -v "${RCODESIGN}" >/dev/null 2>&1 || [ -x "${RCODESIGN}" ]; } \
        && ! security find-identity -v -p codesigning 2>/dev/null | grep -q "$("${SIGN_BIN}" id)"; then
        echo "✗ Developer ID identity unreachable: $("${SIGN_BIN}" id)" >&2
        echo "  rcodesign (disk-key backend) not found on PATH nor at ${HOME}/.cargo/bin/rcodesign," >&2
        echo "  and the identity is not in this session's keychain." >&2
        echo "  Install it (cargo install apple-codesign), set RCODESIGN=/path/to/rcodesign," >&2
        echo "  or sign from a GUI Terminal session whose login keychain is reachable." >&2
        exit 1
    fi
    # HAND IT DOWN BY PATH, because modernech-sign execs `rcodesign` BY NAME.
    # Satisfying the check above is not enough: this script resolves an absolute
    # path, the child does not, and a child that cannot find it silently falls back
    # to the keychain backend ("headless: signing via throwaway keychain") and dies
    # with "no identity found" — a signing failure two steps removed from its cause.
    #
    # Guard on the bare NAME, never on "${RCODESIGN}": `command -v` on an absolute
    # path succeeds whether or not that directory is on PATH, so testing the
    # resolved value makes this branch dead and reintroduces the bug it exists to
    # fix. (It did, in the first cut of this fix.)
    if ! command -v rcodesign >/dev/null 2>&1 && [ -x "${RCODESIGN}" ]; then
        export PATH="$(dirname "${RCODESIGN}"):${PATH}"
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
    # No ghp check any more: the cut talks to GitHub not at all. The release tag
    # and the GitHub Release are created at PROMOTE, by an operator.
    [ -f "${AGE_KEY_AGE}" ] \
        || { echo "✗ release.dp signing key not found: ${AGE_KEY_AGE}" >&2; exit 1; }
    # Refuse BEFORE anything is built or uploaded when the row cannot be
    # registered. Bytes in the staging bucket with no catalog row are a
    # stranded artifact — nobody can list them and nobody can promote them —
    # and the only cheap moment to say so is before the upload.
    require_manage_url
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

# Outer-bootstrap trust-chain gate (every cut, including --dry-run): the modules
# are locked, their dependencies ordered, and the committed bootstraps are what
# the generator writes. Runs before the first build for the same reason
# vulncheck_gate does — a tree whose install chain is not what it claims must
# never mint an artifact. Which suites are in the set, and why the others are
# not, is documented in tools/module_gate.sh.
module_gate

# CVE hard gate (public releases only): scan every module we're about to build.
# Runs before the first build so a vulnerable cut never produces a binary.
# No-op unless --vulncheck / --public set VULNCHECK.
vulncheck_gate

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
# stamp — it is the ONLY source, so the served installer cannot drift. There is
# deliberately no inner/claweed copy in this repo: one used to sit there
# "kept current for shellcheck + reference", nothing could enforce that from a
# repo that cannot see the private canonical file, and it drifted 600+ lines
# while still documenting a retired setuid tier. A second copy of a file this
# repo does not own is a lie waiting to happen — read the daemon repo instead.
# render_inner <comp> <stamp> <dest> writes install.sh.
# stage_migrations_for <comp> <assemble-dir> — put claweed's migration ladder in
# the zip, beside install.sh.
#
# WHY THIS EXISTS. The daemon's installer calls "$SELF_DIR/migrations/run.sh"
# when present; it now SKIPS migration when the ladder is absent, rather than
# refusing the install outright, so an operator on an old kit is never hard
# blocked. That tolerance is exactly why staging cannot be skipped here: a
# skip-if-absent installer will happily install an incomplete kit and never
# say so — staging is what actually SHIPS the ladder, and the assert below is
# the build-time completeness gate that catches its absence before an
# operator ever sees the kit. v0.2.0.2026.08.13.89abc56b shipped exactly
# that way once already: three top-level files, no ladder, dead on arrival.
#
# HOW IT WAS MISSED, so the next person does not repeat it. The daemon's
# build-local.sh stages the ladder and a gate pins that call site — but the
# release assembles its own zips here, and nothing watched this path. Two
# staging routes, one guarded. The rule for WHAT ships therefore lives in ONE
# place, the daemon's stage_migrations.sh, sourced by both callers: a second copy
# of the rule here could drift from the one the daemon tests.
#
# clawee has no ladder; only claweed stages one.
stage_migrations_for() {
    local comp="$1" assemble="$2" src rule n
    [ "${comp}" = claweed ] || return 0
    src="${SRC_CLAWEED}/install/migrations"
    rule="${SRC_CLAWEED}/install/stage_migrations.sh"
    [ -f "${rule}" ] \
        || { echo "✗ ${rule} missing — cannot stage the migration ladder (set CLAWEE_SRC_CLAWEED)" >&2; exit 1; }
    # shellcheck source=/dev/null
    . "${rule}"
    n="$(stage_migrations "${src}" "${assemble}/migrations")" \
        || { echo "✗ staging the migration ladder from ${src} failed" >&2; exit 1; }
    [ -x "${assemble}/migrations/run.sh" ] \
        || { echo "✗ staged ${n} migration file(s) but migrations/run.sh is missing or not executable" >&2; exit 1; }
    echo "✓ migrations/ (${n} files) → ${assemble##*/}" >&2
}

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
        stage_migrations_for "${comp}" "${assemble}"

        asset="clawee-${comp}-${os}-${arch}.zip"
        rm -f "${stage}/${asset}"
        # -r, NOT -j. `-j` junks paths, which flattens migrations/run.sh to a
        # top-level run.sh — the installer looks for "$SELF_DIR/migrations/run.sh"
        # and would refuse a kit whose files were all present but unstructured.
        # Everything else in the assemble dir is top-level either way.
        ( cd "${assemble}" && zip -q -r "${stage}/${asset}" . )

        # ASSERT ON THE ARTIFACT, IN THE CUT PATH. stage_migrations_for's own
        # checks fire when staging goes wrong; nothing fires if the CALL is
        # deleted — which is how the ladder went missing in the first place, and
        # test-e2e.sh's entry-set assertion cannot help because no cut path runs
        # it (`grep -n e2e tools/release.sh cmd/rkit/*.go` finds nothing). This
        # reads the zip that is about to be signed and published.
        # Ask the archive for the ONE entry, rather than grepping a listing:
        # `unzip -l <zip> <name>` exits non-zero when it matches nothing, so the
        # test does not depend on column widths, spacing, or line anchoring in
        # unzip's human-readable table.
        if [ "${comp}" = claweed ] \
            && ! unzip -l "${stage}/${asset}" 'migrations/run.sh' >/dev/null 2>&1; then
            echo "✗ ${asset} has no migrations/run.sh — the installer refuses a kit without it" >&2
            echo "  assemble dir holds: $(ls -A "${assemble}" | tr '\n' ' ')" >&2
            exit 1
        fi

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

    local channel; channel="$(resolve_channel "${comp}")"

    if [ "${DRY_RUN}" = 1 ]; then
        echo
        echo "--- staging keys (${channel}) ---"
        stage_to_staging "${comp}" "${stamp}" "${new_semver}" "${stage}" "${channel}" --dry-run
        echo "--- register payload ---"
        register_staged "${comp}" "${stamp}" "${new_semver}" "${stage}" "${channel}" --dry-run
        echo "✓ dry-run ${comp}: artifacts under ${stage}/ (version bump reverted; nothing uploaded, nothing registered)"
        revert_version
        trap shred_key ERR
        return 0
    fi

    # (5) stage to the PRIVATE bucket. This is the cut's only publication and
    # it publishes nothing: no manifest is written, so no installer can see it.
    if ! stage_to_staging "${comp}" "${stamp}" "${new_semver}" "${stage}" "${channel}"; then
        # The version bump is still staged in the tree at this point and the
        # ERR trap does not fire on an explicit exit — revert it by hand, or
        # the next cut double-bumps over a stamp that was never staged.
        revert_version
        exit 1
    fi

    # (6) register the row. Past this point the cut has produced something an
    # operator can find, so the version bump stands.
    if ! register_staged "${comp}" "${stamp}" "${new_semver}" "${stage}" "${channel}"; then
        exit 1
    fi
    trap shred_key ERR

    # (7) regenerate the bootstraps and record the marker. The bootstraps embed
    # no version — they resolve one at install time — so regenerating them here
    # is bookkeeping, not publication, and nothing is copied anywhere: serving
    # them is promote's job (a `publish-static` verb, not this script).
    #
    # version.js is NOT regenerated: it is derived from the PUBLIC catalog,
    # which a cut does not touch. Writing it here would announce a staged build
    # on the website's version badge.
    bash "${REPO_ROOT}/tools/gen-bootstraps.sh" >&2

    git add "versions/${comp}" "${comp}/install.sh" "${comp}/upgrade.sh"
    git commit -m "[RELEASED: ${comp}] $(date -u +%Y-%m-%d) ${stamp} (staged)"

    echo "✓ staged ${comp} ${stamp} on the ${channel} channel"
    echo "  Promote it from the manage service: ${MANAGE_URL}"
}

# ---- distribute_only: stage-only mode over an already-built dist/<stamp>/
# (produced by `rkit build` — the produce half lives there now).
#
# Runs ONLY: stage to the private bucket -> register the row -> gen-bootstraps.sh
# -> [RELEASED … (staged)] marker commit. No build, no sign, no notarize, no
# version bump — all of that already happened upstream in `rkit build`. It is
# also the re-run path when a cut got as far as building and then failed at the
# upload or the registration.
#
# The tag + `ghp release create` block that used to live here (a deliberate copy
# of do_release's step 5) is GONE from both paths: the release tag is created at
# PROMOTE, so a cut leaves no tag behind and re-staging the same stamp is no
# longer refused by a tag that already exists. That removed the only reason the
# two paths could not share the staging half, which they now do.
#
# On --dry-run: validates the staged dir + component, prints the staging keys
# and the register payload, and returns — no network, no git, no writes.
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

    local src semver channel
    src="$(src_for "${comp}")"
    [ -d "${src}" ] || { echo "✗ ${comp} source worktree missing: ${src}" >&2; exit 1; }
    semver="$(SRC_DIR="${src}" bash "${REPO_ROOT}/tools/version.sh" "${comp}" --semver)"
    channel="$(resolve_channel "${comp}")"

    if [ "${DRY_RUN}" = 1 ]; then
        echo "--- staging keys (${channel}) ---"
        stage_to_staging "${comp}" "${stamp}" "${semver}" "${stage}" "${channel}" --dry-run
        echo "--- register payload ---"
        register_staged "${comp}" "${stamp}" "${semver}" "${stage}" "${channel}" --dry-run
        echo "✓ dry-run distribute-only: nothing uploaded, nothing registered, no writes"
        return 0
    fi

    # Refuse before uploading, for the same reason the full cut does: bytes
    # with no row are a stranded artifact.
    require_manage_url

    # The signing key is needed to sign the catalog row, and --distribute-only
    # skips the pre-flight that normally resolves it. Registration is
    # machine-authentication with the SAME key that signed SHA256SUMS.txt, so
    # there is no second key to configure — just this decrypt.
    resolve_sign_key

    stage_to_staging "${comp}" "${stamp}" "${semver}" "${stage}" "${channel}" || exit 1
    register_staged "${comp}" "${stamp}" "${semver}" "${stage}" "${channel}" || exit 1

    bash "${REPO_ROOT}/tools/gen-bootstraps.sh" >&2

    git add "versions/${comp}" "${comp}/install.sh" "${comp}/upgrade.sh"
    git commit -m "[RELEASED: ${comp}] $(date -u +%Y-%m-%d) ${stamp} (staged)"

    echo "✓ staged ${comp} ${stamp} on the ${channel} channel"
    echo "  Promote it from the manage service: ${MANAGE_URL}"
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
