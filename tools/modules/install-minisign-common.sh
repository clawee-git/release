# module: install-minisign-common  v1
# needs:  helpers sha256 tmp-workspace
# since:  2026-08-26
# Provides minisign when the host has none. The per-platform modules that follow
# try the OS package manager first, then the official jedisct1/minisign release
# archive whose SHA-256 is PINNED here. This bootstrap is the install's trust
# root already — it is served from the release host over HTTPS and the operator
# runs it — so a hash carried inside it makes the fetched verifier exactly as
# trusted as the script that carries the hash. The mirror or CDN that served
# the bytes never enters that calculation: only bytes matching the pin survive
# minisign_fetch. A second seal, minisign_seal, then checks the archive's own
# .minisig against upstream's release key using the binary just installed.
#
# BUMPING THE PIN is a deliberate, reviewed change — never "latest":
#   1. download minisign-<v>-linux.tar.gz, minisign-<v>-macos.zip and both
#      .minisig files from https://github.com/jedisct1/minisign/releases
#   2. minisign -Vm <archive> -P "$MINISIGN_UPSTREAM_PUBKEY"     (each archive)
#   3. shasum -a 256 <archive>                                    (each archive)
#   4. update MINISIGN_VERSION and both sha256 constants, bump this module's vN,
#      sh tools/lock-modules.sh && sh tools/gen-bootstraps.sh &&
#      sh tools/test-modules.sh && sh tools/test-install-minisign.sh
#      (the suite reads MINISIGN_VERSION and the pins from the generated block —
#      nothing in it to edit)
#   5. sync-modules.sh from the other products (they carry this module too)
MINISIGN=""
MINISIGN_VERSION="0.12"
MINISIGN_LINUX_SHA256="9a599b48ba6eb7b1e80f12f36b94ceca7c00b7a5173c95c3efc88d9822957e73"
MINISIGN_MACOS_SHA256="89000b19535765f9cffc65a65d64a820f433ef6db8020667f7570e06bf6aac63"
MINISIGN_UPSTREAM_PUBKEY="RWQf6LRCGA9i53mlYecO4IzT51TGPpvWucNSCh1CBM0QTaLn73Y7GFO3"
MINISIGN_UPSTREAM_BASE="https://github.com/jedisct1/minisign/releases/download/$MINISIGN_VERSION"
# Homebrew locations a daemon-hosted shell's bare PATH omits — the same two the
# product's `update` verb probes.
MINISIGN_KNOWN_PATHS="/opt/homebrew/bin/minisign /usr/local/bin/minisign"

# minisign_known — print the first executable minisign at a location PATH may
# not cover: the install destination itself (an earlier run, or the operator's
# own copy in $PREFIX/bin), then the Homebrew locations. Nothing here is ever
# overwritten; require-minisign uses whatever this finds.
minisign_known() {
    for _mk_p in "${PREFIX:-/usr/local}/bin/minisign" $MINISIGN_KNOWN_PATHS; do
        [ -x "$_mk_p" ] && { printf '%s' "$_mk_p"; return 0; }
    done
    return 1
}

# minisign_dest_dir — where a fetched minisign lands: beside the product, in
# $PREFIX/bin. The inner installer puts that directory on PATH, so the
# product's `update` verb finds it on later runs. PREFIX is resolved by the
# bootstrap before this point; empty means the root-only installers' /usr/local.
minisign_dest_dir() {
    _md="${PREFIX:-/usr/local}/bin"
    mkdir -p "$_md" 2>/dev/null || { info "minisign: cannot create $_md" >&2; return 1; }
    printf '%s' "$_md"
}

# minisign_fetch <name> [sha256] — download <name> into $TMP. With a pin,
# succeed only when the sha256 matches; a mismatch is deleted and the next
# source tried. Without a pin (the .minisig only — the seal proves it) the
# first successful download wins. Sources: $DL_BASE (the test hook), else
# upstream GitHub, then each GH_PROXIES mirror in the <mirror>/<full-url> form
# the download module uses. Every source exhausted -> 1.
minisign_fetch() {
    _mf_name="$1"; _mf_want="${2:-}"; _mf_out="$TMP/$_mf_name"
    if [ -n "${DL_BASE:-}" ]; then
        _mf_srcs="$DL_BASE/$_mf_name"
    else
        _mf_srcs="$MINISIGN_UPSTREAM_BASE/$_mf_name"
        for _mf_p in ${GH_PROXIES:-}; do
            _mf_srcs="$_mf_srcs $_mf_p/$MINISIGN_UPSTREAM_BASE/$_mf_name"
        done
    fi
    for _mf_src in $_mf_srcs; do
        rm -f "$_mf_out"
        # shellcheck disable=SC2086  # $CURL is a command plus its flags
        $CURL -o "$_mf_out" "$_mf_src" 2>/dev/null || continue
        [ -n "$_mf_want" ] || return 0
        _mf_got="$(sha256_of "$_mf_out")" || break
        [ "$_mf_got" = "$_mf_want" ] && return 0
        info "minisign: $_mf_name from $_mf_src does not match the pinned sha256 — discarded"
    done
    rm -f "$_mf_out"
    return 1
}

# minisign_install_file <src> — install <src> as minisign in the destination
# directory and print the absolute path. Never overwrites: a file already
# there belongs to the operator (or an earlier run) and minisign_known will
# have reported it — so minisign_seal's removal below only ever touches a
# file this run created.
minisign_install_file() {
    _mi_dir="$(minisign_dest_dir)" || return 1
    if [ -e "$_mi_dir/minisign" ]; then
        info "minisign: $_mi_dir/minisign already exists — not overwriting it" >&2
        return 1
    fi
    install -m 0755 "$1" "$_mi_dir/minisign" 2>/dev/null \
        || { info "minisign: cannot write $_mi_dir/minisign (not writable — re-run as root, or set PREFIX)" >&2; return 1; }
    printf '%s/minisign' "$_mi_dir"
}

# minisign_seal <archive> <bin> — the second seal: verify the archive's own
# upstream .minisig with the minisign just installed. Failure (including a
# .minisig that cannot be fetched) removes <bin> and returns 1.
minisign_seal() {
    _ms_arc="$1"; _ms_bin="$2"; _ms_name="$(basename "$_ms_arc")"
    if minisign_fetch "$_ms_name.minisig" \
       && "$_ms_bin" -Vm "$_ms_arc" -x "$TMP/$_ms_name.minisig" -P "$MINISIGN_UPSTREAM_PUBKEY" >/dev/null 2>&1; then
        return 0
    fi
    info "minisign: upstream signature on $_ms_name did not verify — removing $_ms_bin"
    rm -f "$_ms_bin"
    return 1
}
