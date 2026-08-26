# module: download  v1
# needs:  helpers
# since:  2026-08-25
if [ -n "$DL_BASE" ]; then
    BASE="$DL_BASE"
else
    BASE="https://github.com/${REPO}/releases/download/${TAG}"
fi
ZIP="@brand@-${COMP}-${OS}-${ARCH}.zip"
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
    # Primary: download from $BASE (GitHub release or $@BRAND@_DL_BASE test hook).
    # Mirror fallback: if the primary fails, retry the SAME GitHub URL through each
    # GH_PROXIES HTTP mirror in turn (no auth, helps GitHub-blocked networks).
    # R2 fallback (grant gate): if all fail AND `@brand@ download-url` is
    # available with a device grant, resolve a presigned URL and download from it.
    # Verification (minisign + sha256 + tag binding) is unchanged regardless of
    # download source, so neither the mirror nor R2 can inject tampered bytes or
    # substitute an older signed release undetected.
    #
    # Only the grant-gated R2 fallback relies on `@brand@` being on PATH. A plain
    # `curl install.sh | sh` with GitHub down and no `@brand@` fails with a clear
    # message — the fallback is for hosts that have already installed @brand@.
    _asset="$1"
    _local="$2"
    info "GET $BASE/$_asset"
    # Primary (GitHub) gets a tight 30s cap (the trailing --max-time overrides
    # $CURL's baked --max-time 300 — curl honours the last occurrence) so a slow or
    # throttled GitHub fails over to the mirrors fast instead of creeping for minutes.
    # The mirror attempts below keep the longer budget: they're the fallback of last
    # resort, so abandoning a working-but-slow mirror at 30s would risk failing the
    # whole install. --connect-timeout 15 + --speed-time 20 (stall) still apply.
    # shellcheck disable=SC2086  # $CURL is an intentional space-split command string (flags + binary); POSIX sh has no arrays.
    if $CURL --max-time 30 -o "$TMP/$_local" "$BASE/$_asset" 2>/dev/null; then
        return 0
    fi
    # Mirror fallback: route the %2F-encoded GitHub URL (MIRROR_BASE) through each
    # mirror in turn. Only for the real GitHub BASE (skip under the DL_BASE test
    # hook) and when enabled. Each full mirror URL is printed so a stalled download
    # is diagnosable from the installer output.
    if [ -z "$DL_BASE" ] && [ -n "$GH_PROXIES" ]; then
        for _proxy in $GH_PROXIES; do
            info "primary failed; trying mirror: $_proxy/$MIRROR_BASE/$_asset"
            # shellcheck disable=SC2086  # intentional word-split of $CURL flags
            if $CURL -o "$TMP/$_local" "$_proxy/$MIRROR_BASE/$_asset" 2>/dev/null; then
                ok "downloaded $_asset via mirror $_proxy"
                return 0
            fi
        done
    fi
    # Primary + mirrors failed. Attempt R2 fallback only when `@brand@` is on PATH.
    if command -v @brand@ >/dev/null 2>&1; then
        info "primary download failed for $_asset; trying R2 fallback via @brand@"
        _r2url="$(@brand@ download-url @COMP@ "$TAG" "$_asset" 2>/dev/null)" || true
        if [ -n "$_r2url" ]; then
            # Scheme guard: the resolved URL MUST be https:// in production, or
            # https:// / http:// in test mode (@BRAND@_DL_BASE set). This prevents
            # a compromised `@brand@` from redirecting to file://, ftp://, or
            # other unsafe schemes. Fail the fallback (not the whole install) if
            # the URL doesn't pass this check — user will see the no-@brand@ error path.
            _valid_scheme=0
            case "$_r2url" in
                https://*)
                    _valid_scheme=1
                    ;;
                http://*)
                    # Allow http:// only in test mode (when DL_BASE is set).
                    if [ -n "$DL_BASE" ]; then
                        _valid_scheme=1
                    fi
                    ;;
            esac
            if [ "$_valid_scheme" -eq 1 ]; then
                # shellcheck disable=SC2086  # intentional word-split of $CURL flags
                $CURL -o "$TMP/$_local" "$_r2url" 2>/dev/null \
                    || fail "R2 fallback download failed for $_asset — check device grant and retry"
                ok "downloaded $_asset via R2 fallback"
                return 0
            fi
            # URL scheme invalid — treat as a fallback failure so the caller
            # sees the standard "no authorized @brand@" error.
        fi
        fail "@brand@ download-url returned no URL for $_asset — device grant may be expired; run '@brand@ login' to renew, or retry when GitHub is reachable"
    fi
    fail "download failed: $_asset (from $BASE; mirrors: $GH_PROXIES) — GitHub and all mirrors are unreachable and there is no authorized @brand@ on PATH — install @brand@ + run '@brand@ login' to enable the backup channel, or retry when GitHub is reachable"
}
info "downloading $ZIP"
dl "$ZIP" "$ZIP"
info "downloading SHA256SUMS.txt + signature"
dl "SHA256SUMS.txt"         "SHA256SUMS.txt"
dl "SHA256SUMS.txt.minisig" "SHA256SUMS.txt.minisig"
