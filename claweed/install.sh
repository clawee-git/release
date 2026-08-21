#!/bin/sh
# Clawee outer bootstrap — THE TRUST ANCHOR (POSIX sh, macOS + Linux).
#
#   curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/claweed/install.sh | sh
#   curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/claweed/upgrade.sh | sh -s -- 0.1.15
#
# This is the stable, curl'd-alone entry point for the `claweed` component. It
# NEVER runs an unverified byte: it downloads the release zip + SHA256SUMS.txt +
# its minisig, verifies the minisign signature with a baked-in PUBLIC key,
# verifies the zip's sha256 against the now-trusted sums file, and ONLY THEN
# unzips and execs the verified inner per-release install.sh. Any failure aborts
# before anything is installed.
#
# TWO MODES, ONE TEMPLATE. The mode placeholder below is substituted at
# render time and decides whether this file stops after the inner installer
# (install.sh) or goes on to force `migrations/upgrade.sh <line>` out of the
# SAME verified kit (upgrade.sh):
#
#   install.sh   resolve + verify + unzip  →  ./install.sh
#   upgrade.sh   resolve + verify + unzip  →  ./install.sh  →  ./migrations/upgrade.sh <line>
#
# The composition lives HERE, one layer above both, so neither the installer nor
# the migration script grows a second job: install.sh is still the only thing
# that places binaries, and migrations/upgrade.sh is still migrations-only. And
# it is the SAME FILE, not a fork: the baked pubkey and the minisign gate are
# the same lines for both modes, because a copy of a trust anchor is a copy
# that drifts from it.
#
# IT IS RENDERED FOR EVERY COMPONENT, not only claweed — the only one whose kit
# ships a migration ladder today (clawee, the terminal client, ships none).
# Which kits carry migrations/ is decided in the COMPONENT repos at their
# build; this repo renders a static file at ITS cut and serves it from a URL we
# advertise. A conditional render would put a "does claweed have a ladder"
# belief in this repo that nothing keeps in step with the zips, and the first
# time it was wrong the URL would 404. So the file always exists, and a kit
# with no migrations/upgrade.sh is a RUNTIME refusal naming the component and
# the version just installed — a message an operator can act on.
#
# DO NOT EDIT generated copies (claweed/install.sh, claweed/upgrade.sh) by hand —
# they are produced from tools/bootstrap.template.sh by tools/gen-bootstraps.sh.
#
# Arguments (upgrade.sh only; install.sh takes none and REJECTS any):
#   <line>                   the release line to force migrations for, e.g.
#                            0.1.15. Optional — absent, latest is resolved
#                            exactly as install.sh does. Present, it is checked
#                            against the RESOLVED release: a mismatch refuses
#                            rather than forcing a different line's migrations
#                            under a wrong banner.
#
# Exit codes (upgrade.sh):
#   0   installed; the ladder applied nothing (its 0) or its rungs RAN (its 2)
#   1   installed, but the ladder refused or failed (its 1) — or any other abort
#   3   installed, the ladder ran, but a receipt was lost (its 3) — re-runnable
#  64   the command line was wrong, or the ladder rejected the one built for it
#
# Env vars:
#   <pin var>               pin a release tag (e.g. claweed/v0.1.1.…); default: latest
#                           (clawee → CLAWEE_VERSION; claweed → CLAWEE_CLAWEED_VERSION)
#   PREFIX                  install root (default $HOME/.local; bins at PREFIX/bin)
#   CLAWEE_UNINSTALL=1      clawee only — remove the installed bin
#   CLAWEE_RELEASE_REPO     GitHub repo serving releases (default clawee-git/release)
#   CLAWEE_DL_BASE          (test hook) download assets from this base instead of GitHub
#   CLAWEE_GH_PROXY         Space-separated list of GitHub HTTP mirrors, tried in order
#                           ONLY when github.com / api.github.com are unreachable
#                           (default: gh-proxy.org cdn.gh-proxy.org v6.gh-proxy.org
#                           gh-proxy.com; set empty to disable). minisign + sha256
#                           verified, so an untrusted mirror cannot tamper undetected.
#                           For VERSION RESOLUTION they are only consulted after the
#                           operator-controlled downloads mirror (see below).
#   CLAWEE_DOWNLOADS_BASE   Operator-controlled public R2 mirror base (default
#                           https://downloads.clawee.org; set empty to disable).
#                           Serves <comp>/<stamp>/<file> + <comp>/latest.json.
#                           When GitHub is unreachable, VERSION RESOLUTION prefers
#                           its latest.json BEFORE the third-party gh-proxy mirrors
#                           (anti-rollback: a stale/hostile mirror could otherwise
#                           pin fresh installs to an older, genuinely-signed
#                           release). Byte DOWNLOADS still use it last-resort;
#                           bytes from any source are minisign + sha256 verified.
#
# claweed note: the claweed inner installer is the canonical sudo-minimal daemon
# installer. It escalates with sudo only for the steps that genuinely need root
# (the root-owned daemon binaries, the boot unit, and the root-owned spawn
# policy files) and cross-installs burrowee-gateway. There is NO setuid tier any
# more — the setuid-root clawee-spawn helper is retired; the daemon runs as root
# and forks its own per-user children. To uninstall claweed, run its inner
# installer directly with the `uninstall` subcommand (not via this bootstrap).

set -eu

# ---- knobs --------------------------------------------------------------
COMP="claweed"
# "install" or "upgrade" — see the two-modes note in the header. Baked, never
# read from the environment: the mode is a property of the URL the operator
# curl'd, and a runtime override would make one file behave as the other.
MODE="install"
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
GH_PROXIES="${CLAWEE_GH_PROXY-https://gh-proxy.org https://cdn.gh-proxy.org https://v6.gh-proxy.org https://gh-proxy.com}"
# Public R2 mirror (downloads.clawee.org) — a plain public bucket the operator
# controls (TLS to a clawee-owned domain): <comp>/<stamp>/<file> mirrors the
# GitHub release assets 1:1, and <comp>/latest.json carries the newest stamp.
# Role differs by use: for VERSION RESOLUTION it is preferred over the
# third-party gh-proxy mirrors when GitHub is unreachable (anti-rollback — see
# the resolution section below); for byte DOWNLOADS it stays the last-resort
# fallback after GitHub and every gh-proxy mirror. Bytes are still minisign +
# sha256 verified below regardless of source. ${VAR-default} (not :-) lets
# `CLAWEE_DOWNLOADS_BASE=` explicitly disable it. Never used when DL_BASE (the
# test hook) is set.
DOWNLOADS_BASE="${CLAWEE_DOWNLOADS_BASE-https://downloads.clawee.org}"

# Production downloads are pinned to HTTPS/TLS1.2 (--proto =https). The
# CLAWEE_DL_BASE test hook points at a local plain-HTTP server, so when it is
# set we drop the TLS-only flags (they'd reject http://). That relaxed mode
# stays locked to the test base BY CONSTRUCTION (no separate guard check):
# whenever DL_BASE is set, every dl() fetch uses $BASE=$DL_BASE and the
# gh-proxy / downloads-mirror fallbacks (resolution AND download) are skipped.
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

# Extract the "stamp" field from a downloads.clawee.org <comp>/latest.json body
# read on stdin. Prefer jq (structural — reads only the top-level "stamp"); fall
# back to a line-anchored grep/sed so a "stamp":"…" buried in other text can't
# spoof it. The caller re-checks the value looks like a v… stamp.
latest_stamp() {
    if command -v jq >/dev/null 2>&1; then
        jq -r '.stamp // empty' 2>/dev/null
    else
        grep -E '^[[:space:]]*"stamp"[[:space:]]*:' \
            | sed -E 's/.*"stamp"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' \
            | head -n1
    fi
}

# semver_of <tag-or-stamp> — the leading X.Y.Z of "<comp>/v<semver>.<date>.<sha8>"
# or a bare "v<semver>…" stamp. Used (upgrade mode only) to read the release
# line off the RESOLVED tag, never off the operator's argument.
semver_of() {
    printf '%s' "${1#v}" | cut -d. -f1-3
}

# is_semver <x> — true only for a bare numeric X.Y.Z. Everything else (empty, a
# malformed field) is NOT a version and must never be compared as one.
is_semver() {
    case "$1" in
        [0-9]*.[0-9]*.[0-9]*) ;;
        *) return 1 ;;
    esac
    case "$1" in
        *[!0-9.]*) return 1 ;;
    esac
    return 0
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

# ---- guard against an unbaked mode --------------------------------------
# Fails closed for the same reason the pubkey guard does: an unsubstituted
# mode placeholder would fall through every `[ "$MODE" = upgrade ]` test
# below, so an upgrade.sh rendered by a broken generator would install and
# then silently skip the migration half it exists for.
case "$MODE" in
    install|upgrade) : ;;
    *) fail "this bootstrap was generated without a mode (got \"$MODE\") — regenerate with tools/gen-bootstraps.sh" ;;
esac

# ---- the command line -----------------------------------------------------
# EVALUATED BEFORE THE NETWORK IS TOUCHED. A refusal that arrives after the
# resolver has already walked GitHub is a refusal that already spent a network
# round trip on an argument that was never going to be accepted.
#
# install.sh takes NO arguments and rejects them rather than discarding them: a
# verb that silently drops what it was given is what a mistyped subcommand
# becomes, and `| sh -s -- 0.1.15` against install.sh is exactly that mistype.
#
# upgrade.sh takes at most one — the release line. It is optional (absent,
# latest is resolved as always) and it is not a label: see the cross-check
# after version resolution for what "checked against the resolved release"
# means.
usage() {
    printf 'usage: curl -fsSL https://release.clawee.org/%s/%s.sh | sh' "$COMP" "$MODE"
    if [ "$MODE" = upgrade ]; then
        printf ' -s -- [<line>]\n\n'
        printf 'Install the %s release and then FORCE this line'"'"'s state migrations from the\n' "$COMP"
        printf 'same verified kit. <line> is MAJOR.MINOR.PATCH (e.g. 0.1.15); a leading "v" and a\n'
        printf 'release stamp'"'"'s trailing .date.sha are accepted. Given, it is checked against the\n'
        printf 'resolved release; omitted, the latest release is used and the line is read off it.\n\n'
        printf 'exit: 0 installed (ladder applied nothing, or its rungs ran) · 1 the ladder\n'
        printf 'refused or failed · 3 the ladder ran but a receipt was lost · 64 bad command line.\n'
    else
        printf '\n\nInstall the latest %s release. Takes no arguments; pin a specific release with\n' "$COMP"
        printf 'this component'"'"'s version-pin env var (see the README). To force this line'"'"'s\n'
        printf 'state migrations as well, use upgrade.sh instead.\n'
    fi
}

# usage_error — stderr and 64 (EX_USAGE). 64 rather than 1 so a typo can never
# be read as "the ladder refused", and rather than 0 so a script does not pass
# on a mistyped argument.
usage_error() {
    printf '\n  \342\234\227 %s\n\n' "$1" >&2
    usage >&2
    exit 64
}

# norm_line <string> — MAJOR.MINOR.PATCH, or non-zero when the value is not
# something that may be compared as a version. Deliberately stricter than
# semver_of — it rejects a non-numeric field outright rather than reading it
# as 0, so a typo is refused here instead of silently comparing wrong later.
norm_line() {
    _nl="${1##*/}"
    _nl="${_nl#v}"
    case "$_nl" in
        *.*.*) ;;
        *) return 1 ;;
    esac
    _nl_major="${_nl%%.*}"
    _nl_rest="${_nl#*.}"
    _nl_minor="${_nl_rest%%.*}"
    _nl_rest="${_nl_rest#*.}"
    _nl_patch="${_nl_rest%%.*}"
    for _nl_f in "$_nl_major" "$_nl_minor" "$_nl_patch"; do
        case "$_nl_f" in
            ''|*[!0-9]*) return 1 ;;
        esac
    done
    printf '%s.%s.%s' "$_nl_major" "$_nl_minor" "$_nl_patch"
}

LINE=""
while [ $# -gt 0 ]; do
    case "$1" in
        -h|--help|help)
            usage
            exit 0 ;;
        -*)
            usage_error "unknown option '$1'" ;;
        *)
            [ "$MODE" = upgrade ] \
                || usage_error "$COMP/install.sh takes no arguments, and was given '$1' — did you mean upgrade.sh, which takes the release line?"
            [ -z "$LINE" ] \
                || usage_error "unexpected extra argument '$1' — upgrade.sh takes at most one, the release line"
            LINE="$(norm_line "$1")" \
                || usage_error "'$1' is not a release line this bootstrap can compare — expected MAJOR.MINOR.PATCH, all numeric (0.1.15, v0.1.15, or a release stamp like v0.1.15.2026.06.14.86f2a984)"
            shift ;;
    esac
done

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
    # GitHub API unreachable/empty — resolve "latest" from the OPERATOR-CONTROLLED
    # R2 mirror's latest.json FIRST (no auth). Anti-rollback: which TAG is "latest"
    # decides which (genuinely-signed) release gets installed, so an on-path
    # attacker who blocks GitHub must not be able to steer resolution to a
    # stale/hostile third-party gh-proxy mirror serving an old /releases JSON and
    # freeze fresh installs on an older release. downloads.clawee.org is TLS to a
    # clawee-owned domain and its catalog is written by release.sh at cut time.
    # Skipped under the DL_BASE test hook and when the mirror is disabled (empty).
    if [ -z "$TAG" ] && [ -z "$DL_BASE" ] && [ -n "$DOWNLOADS_BASE" ]; then
        info "GitHub API unreachable — trying $DOWNLOADS_BASE/$COMP/latest.json"
        # shellcheck disable=SC2086  # intentional word-split of $CURL flags
        lj="$($CURL "$DOWNLOADS_BASE/$COMP/latest.json" 2>/dev/null)" || true
        st="$(printf '%s' "$lj" | latest_stamp)" || true
        # Require a real v… stamp before trusting it (bytes are still verified below).
        case "$st" in
            v*) TAG="$COMP/$st"; info "downloads mirror: $TAG" ;;
        esac
    fi
    # Still unresolved — last resort: the third-party gh-proxy mirrors (no auth).
    # These only decide the tag when GitHub AND the downloads mirror are both
    # unreachable; the bytes they serve are minisign + sha256 verified either way.
    # Skipped under the DL_BASE test hook and when mirrors are disabled (empty).
    if [ -z "$TAG" ] && [ -z "$DL_BASE" ] && [ -n "$GH_PROXIES" ]; then
        for _proxy in $GH_PROXIES; do
            info "GitHub API + downloads mirror unreachable — retrying via mirror $_proxy"
            # shellcheck disable=SC2086  # intentional word-split of $CURL flags
            body="$($CURL "$_proxy/$api" 2>/dev/null)" || true
            TAG="$(printf '%s' "$body" | latest_tag)" || true
            if [ -n "$TAG" ]; then info "mirror resolved: $TAG"; break; fi
        done
    fi
    [ -n "$TAG" ] || fail "no published release found for ${COMP} on ${REPO} (GitHub, ${DOWNLOADS_BASE:-the R2 mirror}, and the gh-proxy mirrors [$GH_PROXIES] were all unreachable)"
    info "latest: $TAG"
fi

# THE LINE IS A CROSS-CHECK ON WHAT GETS INSTALLED, whoever answered — an env
# pin reaches $TAG without passing through any of the resolution branches
# above, so the check is asserted once, here, where every path has converged.
if [ -n "${LINE:-}" ]; then
    _resolved_line="$(semver_of "${TAG#*/}")"
    [ "$_resolved_line" = "${LINE}" ] || fail "you asked for line ${LINE}, but the release resolved for this host is \"$TAG\" (line ${_resolved_line}).
    Refusing: installing ${_resolved_line} and then forcing its migrations while
    reporting a ${LINE} upgrade is exactly the wrong belief this argument exists
    to catch. Drop the argument to take what the channel is serving, or pin the
    exact tag you want via this component's version-pin env var."
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
# Public R2 mirror per-stamp base: downloads.clawee.org/<comp>/<stamp>. The tag
# is <comp>/<stamp>; strip the comp/ prefix to recover the stamp path segment.
STAMP="${TAG#"$COMP/"}"
DOWNLOADS_FILE_BASE="$DOWNLOADS_BASE/$COMP/$STAMP"

dl() {
    # dl <remote-name> <local-name>  (local goes under $TMP)
    #
    # Primary: $BASE (GitHub release or $CLAWEE_DL_BASE test hook). Mirror fallback:
    # if the primary fails, retry the %2F-encoded GitHub URL ($MIRROR_BASE) through
    # each GH_PROXIES HTTP mirror in turn (no auth, helps GitHub-blocked networks).
    # minisign + sha256 verification below is unchanged regardless of source, so an
    # untrusted mirror cannot inject tampered bytes undetected.
    info "GET $BASE/$1"
    # shellcheck disable=SC2086  # $CURL is an intentional space-split command string (flags + binary); POSIX sh has no arrays.
    if $CURL -o "$TMP/$2" "$BASE/$1" 2>/dev/null; then
        return 0
    fi
    # Each full mirror URL is printed so a stalled download is diagnosable from output.
    if [ -z "$DL_BASE" ] && [ -n "$GH_PROXIES" ]; then
        for _proxy in $GH_PROXIES; do
            info "primary failed; trying mirror: $_proxy/$MIRROR_BASE/$1"
            # shellcheck disable=SC2086  # intentional word-split of $CURL flags
            if $CURL -o "$TMP/$2" "$_proxy/$MIRROR_BASE/$1" 2>/dev/null; then
                ok "downloaded $1 via mirror $_proxy"
                return 0
            fi
        done
    fi
    # Last resort: the public R2 mirror (downloads.clawee.org/<comp>/<stamp>/).
    # Untrusted — the minisign + sha256 verification below is unchanged, so it
    # cannot inject tampered bytes. Skipped under the DL_BASE test hook / disabled.
    if [ -z "$DL_BASE" ] && [ -n "$DOWNLOADS_BASE" ]; then
        info "mirrors failed; trying downloads mirror: $DOWNLOADS_FILE_BASE/$1"
        # shellcheck disable=SC2086  # intentional word-split of $CURL flags
        if $CURL -o "$TMP/$2" "$DOWNLOADS_FILE_BASE/$1" 2>/dev/null; then
            ok "downloaded $1 via downloads.clawee.org"
            return 0
        fi
    fi
    fail "download failed: $1 (from $BASE; mirrors: $GH_PROXIES; downloads: ${DOWNLOADS_BASE:-disabled}) — refusing to install unverified bytes"
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
# relative to its own location (./clawee, ./claweed, ./claweed-updater).
#
# The two components have DIFFERENT inner-installer contracts:
#   clawee   — simple bin-placer: reads PREFIX + CLAWEE_UNINSTALL.
#   claweed  — canonical sudo-minimal daemon installer: runs interactively,
#              escalates with sudo only for the steps that need root, and
#              cross-installs burrowee-gateway. No setuid tier (the clawee-spawn
#              helper is retired). NOTE: CLAWEE_PREFIX is still exported below.
#              The CURRENT canonical installer no longer honours it, and the fix
#              is NOT to restore that: an installer-honoured prefix is how a
#              ROOT boot unit ended up naming a user-writable path, which is a
#              standing uid-0 grant to whoever owns that path. The daemon repo
#              removed it for that reason (whole-branch review C2) and now
#              always installs root-owned under /usr/local/bin. So on a current
#              release the export is inert; an OLDER pinned claweed still reads
#              it. Whether to drop the export is a separate question from the
#              spawn-helper retirement, and is not decided here.
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

# ---- upgrade mode: the forced migration pass -----------------------------
# THE SECOND STEP, and the only thing that separates upgrade.sh from
# install.sh. It runs out of $TMP/x — the SAME verified kit the case block
# above just ran the inner installer from — so the migrations that execute are
# the ones shipped alongside the binaries that were just placed, not whatever
# an earlier install left on disk.
#
# ORDER IS LOAD-BEARING and is enforced by `set -e`, not by a comment: the case
# block above aborts the script on a non-zero inner installer, so the ladder
# cannot run against a host whose binaries did not land.
#
# THE LINE COMES FROM THE VERIFIED KIT'S OWN LEDGER — the newest target in its
# migrations/run.sh — never from the operator's argument and NO LONGER from the
# resolved tag. The tag-derived line (the original port) silently assumed the
# ladder's newest target equals the release line, which is only true for a
# release that ships a new rung: the first release cut WITHOUT one (claweed
# 0.2.1, ladder still topping at 0.2.0) made migrations/upgrade.sh's own
# kit-level equality check refuse the tag's line, and this bootstrap then
# failed EVERY run — after a successful install, reporting a healthy upgrade as
# broken. Deriving from the kit states exactly what this mode does: force THE
# LADDER IN THE KIT THAT WAS JUST VERIFIED AND INSTALLED. Nothing is lost:
# the operator's wrong-belief protection is the LINE argument, already checked
# against the resolved tag above, and migrations/upgrade.sh's cross-check stays
# load-bearing where it was written for — an operator hand-running a ladder in
# a directory that may not be the kit they think it is. The ledger is read with
# the same (version, script) word-split its own runner uses.
if [ "$MODE" = upgrade ]; then

    # A kit with no ladder SAYS SO AND FAILS. Silent success here is the worst
    # outcome this file can produce: a zip shipped without migrations/, an
    # upgrade that skipped its state migration, and nothing in the output to
    # show it. The component and the version just installed are both named,
    # because the operator's next question is which of the two is wrong.
    #
    # *** THIS CHECK IS LOAD-BEARING, NOT BELT AND BRACES. *** `sh <script>`
    # exits 2 when it cannot open the script (dash, and /bin/sh on Debian and
    # Ubuntu) — the SAME 2 the ladder uses for "rungs ran, success". So a
    # missing, unreadable or empty migrations/upgrade.sh invoked without this
    # guard is not merely reported badly, it is reported as a COMPLETED
    # MIGRATION. The three tests below are cheap and they are the whole
    # defence — removing any of them is removing the reason exit 2 can be
    # trusted at all.
    { [ -f "$TMP/x/migrations/upgrade.sh" ] && [ -r "$TMP/x/migrations/upgrade.sh" ] && [ -s "$TMP/x/migrations/upgrade.sh" ]; } \
        || fail "$COMP $TAG ships no migrations/upgrade.sh — this release has no migration ladder, so there is nothing for upgrade.sh to force.
    The ${COMP} binaries from $TAG ARE installed: this run placed them, and only
    the migration half had nothing to run. If ${COMP} is not expected to have a
    ladder, the plain installer is the right entry point:
      curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/$COMP/install.sh | sh"

    # migrations/run.sh is the ledger's home; a kit carrying upgrade.sh without
    # it is as mis-assembled as one with no ladder at all, and upgrade.sh's own
    # first act would be to refuse over the missing runner — refuse HERE, with
    # the component and tag named, rather than reporting that as a host fault.
    { [ -f "$TMP/x/migrations/run.sh" ] && [ -r "$TMP/x/migrations/run.sh" ] && [ -s "$TMP/x/migrations/run.sh" ]; } \
        || fail "$COMP $TAG ships migrations/upgrade.sh but no migrations/run.sh — a mis-assembled release; refusing to force a ladder that has no runner. The ${COMP} binaries from $TAG ARE installed."

    # The newest target, by numeric field comparison over the (version, script)
    # pairs — ledger order is the runner's contract, not this script's to
    # assume, and rows legitimately share a target.
    MIG_LINE="$(awk '
        /^MIGRATIONS="$/ { f = 1; next }
        f && /^"$/       { f = 0 }
        f && NF == 2     { print $1 }
    ' "$TMP/x/migrations/run.sh" | sort -t. -k 1,1n -k 2,2n -k 3,3n | tail -n 1)"
    is_semver "$MIG_LINE" \
        || fail "cannot read a newest target out of $COMP $TAG's migrations/run.sh ledger (got '$MIG_LINE') — refusing to force migrations for a line this bootstrap cannot name. The ${COMP} binaries from $TAG ARE installed."

    info "forcing the $MIG_LINE state migrations from the verified kit"
    # `set +e` around the call ONLY. The ladder's non-zero codes are its
    # contract, not a failure of this script, and `set -e` would abort here and
    # throw away the code the mapping below exists to read.
    set +e
    ( cd "$TMP/x" && sh ./migrations/upgrade.sh "$MIG_LINE" )
    LADDER=$?
    set -e

    # THE MAPPING, stated explicitly and printed. The ladder's contract is five
    # values and TWO of them are success: 0 (nothing applied) and 2 (rungs
    # RAN). A bootstrap that treated non-zero as failure would report every
    # real upgrade as broken; one that ignored the code would report a refusal
    # as success. 3 and 64 are passed through as themselves rather than folded
    # into 1, because they mean different things to whoever reads `echo $?`.
    case "$LADDER" in
        0)  MAPPED=0; LADDER_MEANING="nothing applied — this host needed no migration" ;;
        2)  MAPPED=0; LADDER_MEANING="migrations RAN (success) — $COMP is STOPPED and starting it is yours" ;;
        3)  MAPPED=3; LADDER_MEANING="migrations ran but a receipt was lost — $COMP is STOPPED; the rungs stay re-runnable" ;;
        1)  MAPPED=1; LADDER_MEANING="the ladder REFUSED or FAILED — read its output above" ;;
        64) MAPPED=64; LADDER_MEANING="the ladder rejected the command line this bootstrap built for it (a defect here, not yours)" ;;
        *)  MAPPED=1; LADDER_MEANING="undocumented ladder exit — treated as a failure" ;;
    esac
    printf '\n  \342\206\222 migration ladder exited %s: %s\n' "$LADDER" "$LADDER_MEANING"
    printf '  \342\206\222 this bootstrap exits %s\n\n' "$MAPPED"
    if [ "$MAPPED" -ne 0 ]; then
        exit "$MAPPED"
    fi
    ok "$COMP $TAG installed and its $MIG_LINE migrations forced"
fi
