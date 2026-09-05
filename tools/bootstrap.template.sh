#!/bin/sh
# Clawee outer bootstrap — THE TRUST ANCHOR (POSIX sh, macOS + Linux).
#
#   curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/@COMP@/install.sh | sh
#   curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/@COMP@/upgrade.sh | sh -s -- 0.2.0
#
# This is the stable, curl'd-alone entry point for the `@COMP@` component. It
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
# advertise. A conditional render would put a "does @COMP@ have a ladder"
# belief in this repo that nothing keeps in step with the zips, and the first
# time it was wrong the URL would 404. So the file always exists, and a kit
# with no migrations/upgrade.sh is a RUNTIME refusal naming the component and
# the version just installed — a message an operator can act on.
#
# TWO CHANNELS, THE SAME TEMPLATE. @CHANNEL@ is substituted at render time and
# decides which channel manifest this file resolves — nothing else about it
# differs, because the trust gate must not have two implementations:
#
#   @COMP@/install.sh        stable  ->  <comp>/latest.json
#   @COMP@/beta.install.sh   beta    ->  <comp>/beta/latest.json
#
# The beta twins are rendered UNCONDITIONALLY. Whether a beta exists is what
# its manifest answers, at install time, on the host doing the installing; a
# render-time belief about it in this repo would be a second answer that
# nothing keeps in step. A twin whose channel is serving nothing refuses at
# runtime, naming the channel.
#
# DO NOT EDIT generated copies (@COMP@/install.sh, @COMP@/upgrade.sh,
# @COMP@/beta.install.sh, @COMP@/beta.upgrade.sh) by hand — they are produced
# from tools/bootstrap.template.sh by tools/gen-bootstraps.sh.
#
# Arguments (upgrade.sh only; install.sh takes none and REJECTS any):
#   <line>                   the MIGRATION line to force, e.g. 0.2.0 — "assume
#                            this host is below <line>". Optional: absent, the
#                            line is read out of the verified kit's own ledger.
#                            Handed to the kit's migrations/upgrade.sh VERBATIM;
#                            whether this kit carries that line is that script's
#                            own cross-check (refusal names both values). The
#                            release installed is always the resolved one
#                            (latest, or the version-pin env var).
#
# Exit codes (upgrade.sh):
#   0   installed; the ladder applied nothing (its 0) or its rungs RAN (its 2)
#   1   installed, but the ladder refused or failed (its 1) — or any other abort
#   3   installed, the ladder ran, but a receipt was lost (its 3) — re-runnable
#  64   the command line was wrong, or the ladder rejected the one built for it
#
# Env vars:
#   <pin var>               pin a release tag (e.g. @COMP@/v0.1.1.…); default: latest
#                           (clawee → CLAWEE_VERSION; claweed → CLAWEE_CLAWEED_VERSION)
#   PREFIX                  install root (default $HOME/.local; bins at PREFIX/bin)
#   CLAWEE_UNINSTALL=1      clawee only — remove the installed bin
#   CLAWEE_RELEASE_REPO     GitHub repo serving releases (default clawee-git/release)
#   CLAWEE_DL_BASE          (test hook) download assets from this base instead of GitHub
#   CLAWEE_GH_API_BASE      (test hook) the GitHub API origin; the tag-list fallback
#                           is aimed here so a suite can make GitHub reachable or
#                           not without touching the real one
#   CLAWEE_GH_PROXY         Space-separated list of GitHub HTTP mirrors, tried in order
#                           ONLY when github.com / api.github.com are unreachable
#                           (default: gh-proxy.org cdn.gh-proxy.org v6.gh-proxy.org
#                           gh-proxy.com; set empty to disable). minisign + sha256
#                           verified, so an untrusted mirror cannot tamper undetected.
#                           For VERSION RESOLUTION they are only consulted after the
#                           operator-controlled downloads mirror (see below).
#   CLAWEE_DOWNLOADS_BASE   Operator-controlled public R2 mirror base (default
#                           https://downloads.clawee.org; set empty to disable).
#                           Serves <comp>/<stamp>/<file>, <comp>/latest.json and
#                           <comp>/beta/<stamp>/<file>, <comp>/beta/latest.json.
#                           VERSION RESOLUTION READS ITS MANIFEST FIRST — see the
#                           resolution section. Byte DOWNLOADS still use it
#                           last-resort; bytes from any source are minisign +
#                           sha256 verified.
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
COMP="@COMP@"
# "install" or "upgrade" — see the two-modes note in the header. Baked, never
# read from the environment: the mode is a property of the URL the operator
# curl'd, and a runtime override would make one file behave as the other.
MODE="@MODE@"
# "stable" or "beta". Baked for the same reason MODE is: the channel is a
# property of the URL the operator curl'd, and a host that could be moved
# between channels by an environment variable is a host whose channel nobody
# can state (release-management.md §9 — a host never changes channel).
CHANNEL="@CHANNEL@"
PUBKEY="@PUBKEY@"
REPO="${CLAWEE_RELEASE_REPO:-clawee-git/release}"
PREFIX="${PREFIX:-$HOME/.local}"
DL_BASE="${CLAWEE_DL_BASE:-}"           # test hook (undocumented to users)
GH_API_BASE="${CLAWEE_GH_API_BASE:-https://api.github.com}"  # test hook
# TEST_HOOKS is set when EITHER undocumented hook is in play. It is what the
# relaxed-TLS decision and the third-party-mirror skip both read, so "the
# relaxed mode only ever talks to a base the test named" stays true by
# construction rather than by two conditions that have to agree.
TEST_HOOKS=""
[ -n "$DL_BASE" ] && TEST_HOOKS=1
[ -n "${CLAWEE_GH_API_BASE:-}" ] && TEST_HOOKS=1
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

# Production downloads are pinned to HTTPS/TLS1.2 (--proto =https) and to https
# on every REDIRECT too (--proto-redir =https). Both are spelled because which
# one carries the guarantee depends on the host's curl: current curl (8.7
# measured) already restricts redirects to the --proto set, while older
# releases defaulted --proto-redir to permit http. We follow redirects
# everywhere (-L) — GitHub's asset URLs redirect by design — so on such a host
# the https pinning stopped at the first request. Bytes are minisign + sha256
# verified regardless, so this was never an install-anything hole; it was a
# "who can read and rewrite the download" one.
#
# The test hooks point at a local plain-HTTP server, so when either is set we
# drop the TLS-only flags (they'd reject http://). That relaxed mode stays
# locked to bases the test named BY CONSTRUCTION: every dl() fetch uses
# $BASE=$DL_BASE, the tag list is read from $GH_API_BASE, and the third-party
# gh-proxy mirrors — the only hosts nobody in the test named — are skipped.
#
# --speed-limit/--speed-time abort a STALLED transfer (< ~4 KB/s for 20s) instead
# of hanging until --max-time. This matters for the gh-proxy mirror loop: a mirror
# that streams a few MB then stalls is abandoned in ~20s so the NEXT mirror is
# tried, rather than the install appearing stuck for the full 5-minute max-time.
if [ -n "$TEST_HOOKS" ]; then
    CURL="curl -fsSL --connect-timeout 15 --max-time 300 --speed-limit 4096 --speed-time 20"
else
    CURL="curl -fsSL --proto =https --proto-redir =https --tlsv1.2 --connect-timeout 15 --max-time 300 --speed-limit 4096 --speed-time 20"
fi

# ---- helpers ------------------------------------------------------------
@INCLUDE:helpers@

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
    fi | grep -E "^${COMP}/v" | channel_tags | sort -V | tail -n1
}

# channel_tags — keep only the tags belonging to $CHANNEL, reading stdin. A beta
# stamp carries a ".beta." segment and a stable one does not, which is the same
# exclusivity the catalog validates on the way in. Without this the beta twin's
# GitHub fallback would resolve the newest STABLE tag and install it as a beta.
channel_tags() {
    if [ "$CHANNEL" = beta ]; then
        grep -F ".beta."
    else
        grep -vF ".beta."
    fi
}

# manifest_path — the channel manifest's path under the downloads base. The
# channel is a PATH SEGMENT, matching what the publisher writes; never a pattern
# matched out of a stamp.
manifest_path() {
    if [ "$CHANNEL" = beta ]; then
        printf '%s/beta/latest.json' "$COMP"
    else
        printf '%s/latest.json' "$COMP"
    fi
}

# channel_prefix — the per-stamp key prefix on the downloads mirror, the twin of
# manifest_path.
channel_prefix() {
    if [ "$CHANNEL" = beta ]; then
        printf '%s/beta' "$COMP"
    else
        printf '%s' "$COMP"
    fi
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
@INCLUDE:platform-detect@

# ---- guard against a TEMP / unbaked pubkey ------------------------------
@INCLUDE:pubkey-guard@

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
# becomes, and `| sh -s -- 0.2.0` against install.sh is exactly that mistype.
#
# upgrade.sh takes at most one — the MIGRATION LINE: "assume this host is
# below <line>, and force that line's state migrations" (operator ruling
# 2026-08-21; the argument used to be a release-line cross-check, which made
# `-- 0.2.0` refuse whenever the channel had moved past 0.2.0 — the exact
# invocation the backfill needs). It is optional: absent, the line is read out
# of the verified kit's own ledger. It is handed to the kit's
# migrations/upgrade.sh VERBATIM — whether this kit carries that line is that
# script's own cross-check, which names both values on refusal; this file only
# rejects a value that is not a version at all. Release selection is a
# different axis and stays where it was: latest by default, or the component's
# version-pin env var.
usage() {
    printf 'usage: curl -fsSL https://release.clawee.org/%s/%s.sh | sh' "$COMP" "$MODE"
    if [ "$MODE" = upgrade ]; then
        printf ' -s -- [<line>]\n\n'
        printf 'Install the latest %s release and then FORCE state migrations from the same\n' "$COMP"
        printf 'verified kit, as if this host were still below <line>. <line> is the MIGRATION\n'
        printf 'line, MAJOR.MINOR.PATCH (e.g. 0.2.0); a leading "v" and a release stamp'"'"'s\n'
        printf 'trailing .date.sha are accepted. Omitted, the line is read out of the kit'"'"'s own\n'
        printf 'migration ledger; either way the kit'"'"'s migrations/upgrade.sh cross-checks it.\n\n'
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
                || usage_error "$COMP/install.sh takes no arguments, and was given '$1' — did you mean upgrade.sh, which takes the migration line?"
            [ -z "$LINE" ] \
                || usage_error "unexpected extra argument '$1' — upgrade.sh takes at most one, the migration line"
            LINE="$(norm_line "$1")" \
                || usage_error "'$1' is not a migration line this bootstrap can pass on — expected MAJOR.MINOR.PATCH, all numeric (0.2.0, v0.2.0, or a release stamp like v0.2.0.2026.08.19.4e43c2ed)"
            shift ;;
    esac
done

# ---- temp workspace -----------------------------------------------------
@INCLUDE:tmp-workspace@

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
    info "resolving latest ${COMP} release on the ${CHANNEL} channel"
    # Declared before the first source is tried: every step below is guarded on
    # "$TAG" being empty, and under `set -u` an unset one aborts at the second
    # step rather than falling through to it.
    TAG=""
    # ---- 1. THE CHANNEL MANIFEST, FIRST -------------------------------------
    # The manifest is the publisher's own answer to "what is this channel
    # serving": promote writes it LAST, as the go-live, so a build with a
    # manifest entry is a build an operator approved and a build without one is
    # not reachable by design.
    #
    # It used to be consulted only when GitHub was unreachable, which made the
    # GitHub tag list the real authority — and the tag list is not channel-aware
    # and not promote-aware. A staged-then-abandoned cut never has a tag today,
    # but the ordering was still wrong in the way that matters: the tag list
    # answers "what was published", the manifest answers "what should be
    # installed", and only the second is a decision anyone made.
    #
    # GitHub is the FALLBACK for exactly one situation — the manifest host is
    # unreachable — and the third-party mirrors after it, unchanged.
    #
    # MANIFEST_FALLBACK is what gates steps 2 and 3, and the distinction it
    # draws is the whole correctness of this block: "the manifest host did not
    # answer" is a reason to look elsewhere, "the manifest says this channel
    # serves nothing" is an ANSWER. Discarding curl's status collapsed the two
    # — a 404 fell through to the tag list exactly like a dead host — and the
    # tag list records everything ever published, YANKED BUILDS INCLUDED. Yank
    # removes the manifest entry when no public row remains, and deliberately
    # leaves the GitHub release in place, so the collapsed case reinstalled the
    # build that had just been withdrawn; a beta twin reinstalled the previous
    # cycle's beta after the cycle closed.
    MANIFEST_FALLBACK=""
    if [ -z "$DOWNLOADS_BASE" ]; then
        # No manifest is configured at all, so there is nothing that could have
        # answered and the tag list is all there is.
        MANIFEST_FALLBACK=1
    else
        _mf="$DOWNLOADS_BASE/$(manifest_path)"
        info "reading the ${CHANNEL} manifest: $_mf"
        _rc=0
        # shellcheck disable=SC2086  # intentional word-split of $CURL flags
        lj="$($CURL "$_mf" 2>/dev/null)" || _rc=$?
        case "$_rc" in
            0) ;;
            # The connection-class exits, and ONLY these, mean "the host did
            # not answer": 5/6 could not resolve, 7 could not connect, 28 timed
            # out, 35 TLS handshake, 52 empty reply, 56 receive failure. Every
            # other status — 22 above all, which under curl -f is any HTTP 4xx
            # or 5xx — means the host DID answer, and what it answered is that
            # this channel is serving nothing.
            5|6|7|28|35|52|56)
                info "the manifest host did not answer (curl $_rc) — falling back"
                MANIFEST_FALLBACK=1 ;;
            *)
                info "the ${CHANNEL} manifest is not there (curl $_rc): this channel is serving nothing" ;;
        esac
        st="$(printf '%s' "$lj" | latest_stamp)" || true
        # Require a real v… stamp, and require it to BELONG to this channel:
        # a stable stamp in the beta manifest (or the reverse) is a publisher
        # bug, and installing it anyway would put a host on a channel it never
        # asked for. Bytes are still verified below either way.
        case "$st" in
            v*)
                if printf '%s' "$st" | channel_tags >/dev/null 2>&1; then
                    TAG="$COMP/$st"
                    info "manifest: $TAG"
                else
                    info "the ${CHANNEL} manifest names '$st', which is not a ${CHANNEL} stamp — ignoring it"
                fi ;;
        esac
    fi
    # ---- 2. THE GITHUB TAG LIST, only when the manifest host did not answer --
    if [ -z "$TAG" ] && [ -n "$MANIFEST_FALLBACK" ] \
        && { [ -z "$DL_BASE" ] || [ -n "${CLAWEE_GH_API_BASE:-}" ]; }; then
        info "manifest unavailable — falling back to the ${REPO} release list"
        api="${GH_API_BASE}/repos/${REPO}/releases?per_page=100"
        # shellcheck disable=SC2086  # $CURL is an intentional space-split command string (flags + binary); POSIX sh has no arrays.
        body="$($CURL "$api" 2>/dev/null)" || true
        TAG="$(printf '%s' "$body" | latest_tag)" || true
    fi
    # ---- 3. The third-party gh-proxy mirrors, last --------------------------
    # These only decide the tag when the manifest AND GitHub are both
    # unreachable; the bytes they serve are minisign + sha256 verified either
    # way. Skipped under the DL_BASE test hook and when mirrors are disabled.
    if [ -z "$TAG" ] && [ -n "$MANIFEST_FALLBACK" ] && [ -z "$TEST_HOOKS" ] && [ -n "$GH_PROXIES" ]; then
        api="${GH_API_BASE}/repos/${REPO}/releases?per_page=100"
        for _proxy in $GH_PROXIES; do
            info "manifest + GitHub API unreachable — retrying via mirror $_proxy"
            # shellcheck disable=SC2086  # intentional word-split of $CURL flags
            body="$($CURL "$_proxy/$api" 2>/dev/null)" || true
            TAG="$(printf '%s' "$body" | latest_tag)" || true
            if [ -n "$TAG" ]; then info "mirror resolved: $TAG"; break; fi
        done
    fi
    if [ -z "$TAG" ] && [ -n "$MANIFEST_FALLBACK" ]; then
        fail "nothing is published for ${COMP} on the ${CHANNEL} channel: the manifest host did not answer (${DOWNLOADS_BASE:-disabled}), and neither the ${REPO} release list nor the gh-proxy mirrors [${GH_PROXIES:-disabled}] named a ${CHANNEL} release."
    fi
    [ -n "$TAG" ] || fail "nothing is published for ${COMP} on the ${CHANNEL} channel: ${DOWNLOADS_BASE}/$(manifest_path) names no release. That is the expected state when a ${CHANNEL} cycle is closed, or when the only build this channel had was YANKED. The ${REPO} release list is deliberately NOT consulted here: it records every tag ever published, withdrawn builds included, so falling back to it would reinstall exactly what was just taken down."
    info "latest: $TAG"
fi

# THE ARGUMENT IS NOT COMPARED TO THE RELEASE. It is the MIGRATION line, and
# its one legitimate judge is the kit's own migrations/upgrade.sh below —
# which refuses a line its ledger does not carry, naming both values. The old
# release-line cross-check here is what made `-- 0.2.0` refuse on every
# channel that had moved past 0.2.0, i.e. on exactly the hosts a forced
# backfill exists for.

# ---- download -----------------------------------------------------------
# LOCAL FORK — see docs/adoption-2026-08-25-bootstrap-modules.md: the shared
# download module's fallback is a grant-gated `clawee download-url` R2 lookup
# (Burrowee's console/device-grant mechanism), which would REPLACE Clawee's own
# no-auth public downloads.clawee.org mirror fallback rather than add to it —
# a real capability lost, not a wash. Keeping Clawee's own block.
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
# Public R2 mirror per-stamp base: <base>/<comp>[/beta]/<stamp> — the same
# channel layout the manifest is published under. The tag is <comp>/<stamp>;
# strip the comp/ prefix to recover the stamp path segment.
STAMP="${TAG#"$COMP/"}"
DOWNLOADS_FILE_BASE="$DOWNLOADS_BASE/$(channel_prefix)/$STAMP"

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
    if [ -z "$TEST_HOOKS" ] && [ -n "$GH_PROXIES" ]; then
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
    if [ -z "$TEST_HOOKS" ] && [ -n "$DOWNLOADS_BASE" ]; then
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

@INCLUDE:sha256@

# ---- provide minisign (package manager, then pinned upstream) ----------
@INCLUDE:install-minisign-common@
@INCLUDE:install-minisign-linux@
@INCLUDE:install-minisign-darwin@

# ---- require minisign ---------------------------------------------------
@INCLUDE:require-minisign@

# ---- VERIFY (the trust gate) --------------------------------------------
@INCLUDE:verify-signature@

info "verifying checksum"
# 2) the zip's checksum against the now-trusted sums file
@INCLUDE:verify-checksum@
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

    # THE OPERATOR'S LINE WINS; the kit's ledger is the DEFAULT. An explicit
    # argument means "assume this host is below <line>" and is handed to the
    # kit verbatim — migrations/upgrade.sh's own cross-check decides whether
    # this kit carries that line. Absent, the newest target is read out of the
    # ledger, by numeric field comparison over the (version, script) pairs —
    # ledger order is the runner's contract, not this script's to assume, and
    # rows legitimately share a target.
    if [ -n "${LINE:-}" ]; then MIG_LINE="$LINE"; else MIG_LINE="$(awk '
        /^MIGRATIONS="$/ { f = 1; next }
        f && /^"$/       { f = 0 }
        f && NF == 2     { print $1 }
    ' "$TMP/x/migrations/run.sh" | sort -t. -k 1,1n -k 2,2n -k 3,3n | tail -n 1)"; fi
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
