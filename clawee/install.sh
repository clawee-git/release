#!/bin/sh
# Clawee outer bootstrap — THE TRUST ANCHOR (POSIX sh, macOS + Linux).
#
#   curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/clawee/install.sh | sh
#
# This is the stable, curl'd-alone entry point for the `clawee` component. It
# NEVER runs an unverified byte: it downloads the release zip + SHA256SUMS.txt +
# its minisig, verifies the minisign signature with a baked-in PUBLIC key,
# verifies the zip's sha256 against the now-trusted sums file, and ONLY THEN
# unzips and execs the verified inner per-release install.sh. Any failure aborts
# before anything is installed.
#
# DO NOT EDIT generated copies (clawee/install.sh) by hand — they are produced
# from tools/bootstrap.template.sh by tools/gen-bootstraps.sh.
#
# Env vars:
#   <pin var>               pin a release tag (e.g. clawee/v0.1.1.…); default: latest
#                           (clawee → CLAWEE_VERSION; claweed → CLAWEE_CLAWEED_VERSION)
#   PREFIX                  install root (default $HOME/.local; bins at PREFIX/bin)
#   CLAWEE_UNINSTALL=1      clawee only — remove the installed bin
#   CLAWEE_RELEASE_REPO     GitHub repo serving releases (default clawee-git/release)
#   CLAWEE_DL_BASE          (test hook) download assets from this base instead of GitHub
#   CLAWEE_GH_PROXY         Space-separated list of GitHub HTTP mirrors, tried in order
#                           ONLY when github.com / api.github.com are unreachable
#                           (default: cdn.gh-proxy.org gh-proxy.org gh-proxy.com
#                           v6.gh-proxy.org; set empty to disable). minisign + sha256
#                           verified, so an untrusted mirror cannot tamper undetected.
#
# claweed note: the claweed inner installer is the canonical sudo-minimal daemon
# installer. It reads CLAWEE_PREFIX (set here from PREFIX), CLAWEE_DATA_DIR, and
# CLAWEE_REGISTER_SOCKET, escalates with sudo only for the setuid spawn helper,
# and cross-installs burrowee-gateway. To uninstall claweed, run its inner
# installer directly with the `uninstall` subcommand (not via this bootstrap).

set -eu

# ---- knobs --------------------------------------------------------------
COMP="clawee"
PUBKEY="RWTuO+iTqEyo52tDnuRxx1IsrARInzZbBSfgbj4r5jZusvksN2VHuY3E"
REPO="${CLAWEE_RELEASE_REPO:-clawee-git/release}"
PREFIX="${PREFIX:-$HOME/.local}"
DL_BASE="${CLAWEE_DL_BASE:-}"           # test hook (undocumented to users)
# GitHub HTTP mirrors, tried in order ONLY as a fallback when github.com /
# api.github.com are unreachable (e.g. networks that block or throttle GitHub).
# Each is tried as <mirror>/<original-https-github-url> until one succeeds; the
# downloaded bytes are still minisign- + sha256-verified below, so an untrusted
# mirror cannot inject tampered bytes undetected. Space-separated list.
# ${VAR-default} (not :-) lets `CLAWEE_GH_PROXY=` explicitly disable the mirrors
# while an unset value gets the default. Never used when DL_BASE is set.
GH_PROXIES="${CLAWEE_GH_PROXY-https://cdn.gh-proxy.org https://gh-proxy.org https://gh-proxy.com https://v6.gh-proxy.org}"

# Production downloads are pinned to HTTPS/TLS1.2 (--proto =https). The
# CLAWEE_DL_BASE test hook points at a local plain-HTTP server, so when it is
# set we drop the TLS-only flags (they'd reject http://); the version-pin guard
# below keeps even that path scheme-locked to the test base.
#
# --speed-limit/--speed-time abort a STALLED transfer (< ~4 KB/s for 20s) instead
# of hanging until --max-time. This matters for the gh-proxy mirror loop: a mirror
# that streams a few MB then stalls is abandoned in ~20s so the NEXT mirror is
# tried, rather than the install appearing stuck for the full 5-minute max-time.
if [ -n "$DL_BASE" ]; then
    CURL="curl -fsSL --connect-timeout 15 --max-time 300 --speed-limit 4096 --speed-time 20"
else
    CURL="curl -fsSL --proto =https --tlsv1.2 --connect-timeout 15 --max-time 300 --speed-limit 4096 --speed-time 20"
fi

# ---- helpers ------------------------------------------------------------
fail() { printf '\n  ✗ %s\n\n' "$*" >&2; exit 1; }
info() { printf '  → %s\n' "$*"; }
ok()   { printf '  ✓ %s\n' "$*"; }

# Extract the highest "<comp>/v<semver>" tag from a GitHub /releases JSON body
# read on stdin. The /releases order is by tag-commit date, NOT publish order,
# so it is unreliable for "latest" — pick the highest tag via version sort.
# Match only the real "tag_name" FIELD (line-anchored) so release-notes/body
# text that merely contains the literal `"tag_name"` can't spoof the tag.
# Prefer jq (structural); fall back to grep/sed. Used for both the direct
# api.github.com fetch and the GH_PROXY mirror retry.
latest_tag() {
    if command -v jq >/dev/null 2>&1; then
        jq -r '.[].tag_name // empty' 2>/dev/null
    else
        grep -E '^[[:space:]]*"tag_name"[[:space:]]*:' \
            | sed -E 's/.*"tag_name"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/'
    fi | grep -E "^${COMP}/v" | sort -V | tail -n1
}

# ---- platform detection -------------------------------------------------
case "$(uname -s)" in
    Darwin) OS=darwin ;;
    Linux)  OS=linux ;;
    *)      fail "unsupported OS: $(uname -s) (clawee ships darwin + linux only)" ;;
esac
case "$(uname -m)" in
    arm64|aarch64) ARCH=arm64 ;;
    x86_64|amd64)  ARCH=amd64 ;;
    *)             fail "unsupported arch: $(uname -m) (clawee ships arm64 + amd64 only)" ;;
esac

printf '\n  clawee %s installer  (%s/%s)\n\n' "$COMP" "$OS" "$ARCH"

# ---- guard against a TEMP / unbaked pubkey ------------------------------
case "$PUBKEY" in
    ""|*REPLACE*|*PLACEHOLDER*|*TEMP*)
        fail "this installer was built without a real signing key — refusing to verify against a placeholder (regenerate with tools/gen-bootstraps.sh)" ;;
esac

# ---- temp workspace -----------------------------------------------------
TMP="$(mktemp -d "${TMPDIR:-/tmp}/clawee-${COMP}-XXXXXX")" || fail "could not create temp dir"
trap 'rm -rf "$TMP"' EXIT INT TERM

# ---- version resolution -------------------------------------------------
# Read the per-component pin env var by name (no eval). $COMP is a baked
# literal, so a direct case over the known components is exhaustive.
case "$COMP" in
    clawee)   PIN="${CLAWEE_VERSION:-}" ;;
    claweed)  PIN="${CLAWEE_CLAWEED_VERSION:-}" ;;
    *)        fail "unknown component '$COMP' — cannot resolve its version pin" ;;
esac
if [ -n "$PIN" ]; then
    TAG="$PIN"
    info "using pinned version: $TAG"
else
    info "resolving latest ${COMP} release"
    api="https://api.github.com/repos/${REPO}/releases?per_page=100"
    # shellcheck disable=SC2086  # $CURL is an intentional space-split command string (flags + binary); POSIX sh has no arrays.
    body="$($CURL "$api" 2>/dev/null)" || true
    TAG="$(printf '%s' "$body" | latest_tag)" || true
    # GitHub API unreachable/empty — retry through each mirror in turn (no auth).
    # Skipped under the DL_BASE test hook and when mirrors are disabled (empty).
    if [ -z "$TAG" ] && [ -z "$DL_BASE" ] && [ -n "$GH_PROXIES" ]; then
        for _proxy in $GH_PROXIES; do
            info "GitHub API unreachable — retrying via mirror $_proxy"
            # shellcheck disable=SC2086  # intentional word-split of $CURL flags
            body="$($CURL "$_proxy/$api" 2>/dev/null)" || true
            TAG="$(printf '%s' "$body" | latest_tag)" || true
            if [ -n "$TAG" ]; then info "mirror resolved: $TAG"; break; fi
        done
    fi
    [ -n "$TAG" ] || fail "no published release found for ${COMP} on ${REPO} (GitHub and all mirrors [$GH_PROXIES] were unreachable)"
    info "latest: $TAG"
fi

# ---- download -----------------------------------------------------------
if [ -n "$DL_BASE" ]; then
    BASE="$DL_BASE"
else
    BASE="https://github.com/${REPO}/releases/download/${TAG}"
fi
ZIP="clawee-${COMP}-${OS}-${ARCH}.zip"
# gh-proxy mirrors route a release download by treating the release TAG as a
# SINGLE path segment. Our tags contain a slash (<comp>/v…), so a LITERAL slash
# splits the tag across two path segments and some mirror edges then fail to
# serve the asset (or return wrong bytes that later fail verification). Build a
# mirror-only base with the tag's slash percent-encoded (%2F) so the tag stays
# one segment. Direct GitHub ($BASE) keeps the literal slash (it 404s on %2F).
MIRROR_BASE="https://github.com/${REPO}/releases/download/$(printf '%s' "${TAG}" | sed 's#/#%2F#g')"

dl() {
    # dl <remote-name> <local-name>  (local goes under $TMP)
    #
    # Primary: $BASE (GitHub release or $CLAWEE_DL_BASE test hook). Mirror fallback:
    # if the primary fails, retry the %2F-encoded GitHub URL ($MIRROR_BASE) through
    # each GH_PROXIES HTTP mirror in turn (no auth, helps GitHub-blocked networks).
    # minisign + sha256 verification below is unchanged regardless of source, so an
    # untrusted mirror cannot inject tampered bytes undetected.
    # shellcheck disable=SC2086  # $CURL is an intentional space-split command string (flags + binary); POSIX sh has no arrays.
    if $CURL -o "$TMP/$2" "$BASE/$1" 2>/dev/null; then
        return 0
    fi
    if [ -z "$DL_BASE" ] && [ -n "$GH_PROXIES" ]; then
        for _proxy in $GH_PROXIES; do
            info "primary download failed for $1; retrying via mirror $_proxy"
            # shellcheck disable=SC2086  # intentional word-split of $CURL flags
            if $CURL -o "$TMP/$2" "$_proxy/$MIRROR_BASE/$1" 2>/dev/null; then
                ok "downloaded $1 via mirror $_proxy"
                return 0
            fi
        done
    fi
    fail "download failed: $1 (from $BASE; mirrors: $GH_PROXIES) — refusing to install unverified bytes"
}
info "downloading $ZIP"
dl "$ZIP" "$ZIP"
info "downloading SHA256SUMS.txt + signature"
dl "SHA256SUMS.txt"         "SHA256SUMS.txt"
dl "SHA256SUMS.txt.minisig" "SHA256SUMS.txt.minisig"

# ---- require minisign ---------------------------------------------------
# minisign is the trust root: it must already be on PATH from a trusted source
# (your package manager). We never auto-fetch the verifier — a binary pulled
# over the network and run unverified would itself become an unverified trust
# root, defeating the whole signature chain. Verification is mandatory and is
# only ever performed by a minisign the operator already trusts.
if command -v minisign >/dev/null 2>&1; then
    MINISIGN=minisign
else
    case "$OS" in
        darwin) hint="brew install minisign" ;;
        *)      hint="apt-get install minisign  (or your distro's package manager)" ;;
    esac
    fail "minisign is required and is not installed — install it and re-run.
    $hint
    upstream: https://github.com/jedisct1/minisign
    Verification is mandatory; this installer will NOT run an unverified verifier."
fi

# ---- VERIFY (the trust gate) --------------------------------------------
info "verifying signature"
# 1) signature over the sums file, using the baked pubkey (inline, no key fetch)
"$MINISIGN" -V -P "$PUBKEY" -m "$TMP/SHA256SUMS.txt" -x "$TMP/SHA256SUMS.txt.minisig" >/dev/null \
    || fail "signature verification failed — aborting (refusing to install unverified bytes)"
ok "minisign signature valid"

info "verifying checksum"
# 2) the zip's checksum against the now-trusted sums file
grep -qF "$ZIP" "$TMP/SHA256SUMS.txt" \
    || fail "no checksum entry for $ZIP — release incomplete or tampered; aborting"
if command -v shasum >/dev/null 2>&1; then
    ( cd "$TMP" && shasum -a 256 -c --ignore-missing SHA256SUMS.txt >/dev/null ) \
        || fail "checksum mismatch — aborting (zip tampered or download corrupted)"
elif command -v sha256sum >/dev/null 2>&1; then
    ( cd "$TMP" && sha256sum -c --ignore-missing SHA256SUMS.txt >/dev/null ) \
        || fail "checksum mismatch — aborting (zip tampered or download corrupted)"
else
    fail "neither shasum nor sha256sum found — cannot verify; aborting"
fi
ok "checksum verified"

# ---- unzip + exec the verified inner installer --------------------------
command -v unzip >/dev/null 2>&1 \
    || fail "unzip not found — install it (\`brew install unzip\` / \`apt-get install unzip\`) and retry"
unzip -q -o "$TMP/$ZIP" -d "$TMP/x" || fail "zip extraction failed — corrupt download?"
[ -f "$TMP/x/install.sh" ] || fail "release zip missing inner install.sh — aborting"

ok "verified — running inner installer"
# Run with cwd = the unzipped dir: the inner installer resolves the binaries
# relative to its own location (./clawee, ./claweed, ./clawee-spawn).
#
# The two components have DIFFERENT inner-installer contracts:
#   clawee   — simple bin-placer: reads PREFIX + CLAWEE_UNINSTALL.
#   claweed  — canonical sudo-minimal daemon installer: reads CLAWEE_PREFIX
#              (mapped from PREFIX here), runs interactively, escalates with sudo
#              only for the setuid spawn helper, cross-installs burrowee-gateway.
case "$COMP" in
    clawee)
        ( cd "$TMP/x" && PREFIX="$PREFIX" CLAWEE_UNINSTALL="${CLAWEE_UNINSTALL:-}" sh ./install.sh )
        ;;
    claweed)
        ( cd "$TMP/x" && CLAWEE_PREFIX="${CLAWEE_PREFIX:-$PREFIX/bin}" sh ./install.sh )
        ;;
    *)
        fail "unknown component '$COMP' — no inner-exec contract"
        ;;
esac
