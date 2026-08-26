# module: version-resolve  v1
# needs:  helpers
# since:  2026-08-25
# Read the per-component pin env var by name (no eval). $COMP is a baked
# literal, so a direct case over the four known components is exhaustive.
case "$COMP" in
    cli)     PIN="${@BRAND@_CLI_VERSION:-}" ;;
    gateway) PIN="${@BRAND@_GATEWAY_VERSION:-}" ;;
    edge)    PIN="${@BRAND@_EDGE_VERSION:-}" ;;
    agent)   PIN="${@BRAND@_AGENT_VERSION:-}" ;;
    *)       fail "unknown component '$COMP' — cannot resolve its version pin" ;;
esac
# An explicit pin is the operator's own answer, not a resolver's — it is used
# verbatim and is NOT held to the version floor below. Pinning an older release
# is a deliberate, local downgrade (debugging, staged rollback); refusing it
# would take away the only lever an operator has when a new cut misbehaves.
if [ -n "$PIN" ]; then
    TAG="$PIN"
    info "using pinned version: $TAG"
else
    info "resolving latest ${COMP} release"
    # Who answered? "github" = api.github.com, or a GH_PROXY mirror standing in
    # for it; "catalog" = the first-party console. Only the github answer is held
    # to the version floor — see the choke point at the end of this block.
    TAG_SOURCE=github
    # GH_ANSWERED separates "nobody could be reached" from "the source was
    # reached and answered empty" — the two want different advice.
    GH_ANSWERED=0
    if TAG="$(resolve_latest '')"; then GH_ANSWERED=1; else TAG=""; fi
    # GitHub API unreachable/empty — retry through each mirror in turn BEFORE the
    # console catalog (mirrors need no authorized @brand@, so they serve fresh
    # hosts). Skipped under the DL_BASE test hook and when mirrors are disabled.
    if [ -z "$TAG" ] && [ -z "$DL_BASE" ] && [ -n "$GH_PROXIES" ]; then
        for _proxy in $GH_PROXIES; do
            info "GitHub API unreachable — retrying via mirror $_proxy"
            if TAG="$(resolve_latest "$_proxy/")"; then GH_ANSWERED=1; else TAG=""; fi
            if [ -n "$TAG" ]; then info "mirror resolved: $TAG"; break; fi
        done
    fi
    if [ -z "$TAG" ]; then
        TAG_SOURCE=catalog
        # GitHub unreachable or no releases published. Try the console catalog
        # (public, no auth): GET ${CONSOLE_URL}/api/v1/releases/@COMP@/current.
        # This is the R2 fallback path — assets are served via `@brand@ download-url`
        # (see the dl() function below), which requires a device grant.
        info "GitHub unreachable — trying console catalog for latest @COMP@ version"
        catalog_url="${CONSOLE_URL}/api/v1/releases/@COMP@/current"
        # Use plain curl (no TLS-only flags) when DL_BASE is set for tests, else
        # standard hardened curl.
        # shellcheck disable=SC2086  # intentional word-split of $CURL flags
        catalog_body="$($CURL "$catalog_url" 2>/dev/null)" || true
        # Resolve the tag the SAME hardened way as latest_tag(): prefer jq
        # (structural — reads only the top-level "version" field). Without jq,
        # split the body on field boundaries FIRST (tr , and { → newlines): the
        # console serves MINIFIED single-line JSON, so a line-anchored grep
        # would never match it. The field-anchored grep plus the @COMP@/v… shape
        # check below keep a "version":"…" substring buried in notes or nested
        # metadata from spoofing the tag. (Bytes are still minisign+sha256
        # verified downstream; this closes a downgrade / wrong-version vector
        # at the resolution step.)
        if command -v jq >/dev/null 2>&1; then
            TAG="$(printf '%s' "$catalog_body" | jq -r '.version // empty' 2>/dev/null)" || true
        else
            TAG="$(printf '%s' "$catalog_body" \
                | tr ',{' '\n\n' \
                | grep -E '^[[:space:]]*"version"[[:space:]]*:' \
                | sed -E 's/.*"version"[[:space:]]*:[[:space:]]*"([^"]+)".*/\1/' \
                | head -n1)" || true
        fi
        case "$TAG" in
            "$COMP"/v*) : ;;
            *) TAG="" ;;
        esac
        [ -n "$TAG" ] \
            || fail "GitHub and the console catalog are both unreachable — cannot resolve the latest @COMP@ version; retry when either is available"
        info "console catalog: $TAG"
    fi
    info "latest: $TAG"
    # A resolver that also serves the artifacts must not get to pick the version
    # too — GitHub's answer, and a mirror's answer standing in for it, are held
    # to the baked floor. See assert_version_floor.
    #
    # The console catalog is EXEMPT, deliberately. $MIN_VERSION is baked from
    # versions/<comp>.stamp — the version at CUT time — while the catalog serves
    # the last PROMOTED release, and cut and promote are separate steps that
    # legitimately lag each other. Holding the catalog to the cut floor aborts
    # every install in that window, on the only path a GitHub-blocked host has,
    # with advice ("retry when github.com is reachable") that host cannot act on.
    # The catalog is also not a third party: it is the same first-party control
    # plane the static channel and $PUBKEY come from, so exempting it costs no
    # trust this installer had not already extended. The bytes it leads to are
    # still minisign + sha256 verified and still bound to the resolved tag by the
    # signed trusted comment, so the catalog can at worst name an older release
    # OF OURS — never a forged one.
    if [ "$TAG_SOURCE" = catalog ]; then
        info "version floor not applied to the console catalog (first-party; serves the last PROMOTED release)"
    else
        assert_version_floor "$TAG"
    fi
fi

