# module: install-minisign-linux  v1
# needs:  helpers platform-detect install-minisign-common
# since:  2026-08-26
# Linux: the package manager first — but only as root or with passwordless
# sudo, because a user-level install must never prompt for a password inside
# curl|sh — then the pinned static upstream build (x86_64 / aarch64, statically
# linked, so distro and libc do not matter). Every failure here is an info
# line and falls through; require-minisign is the one that decides.
# MINISIGN_SKIP_PM=1 says a preflight already made the package-manager attempt.
if [ "$OS" = linux ] && ! command -v minisign >/dev/null 2>&1 && ! minisign_known >/dev/null; then
    _ml_sudo=""; _ml_can_pm=0
    if [ "$(id -u)" = 0 ]; then
        _ml_can_pm=1
    elif sudo -n true 2>/dev/null; then
        _ml_sudo="sudo"; _ml_can_pm=1
    fi
    if [ -n "${MINISIGN_SKIP_PM:-}" ]; then
        :
    elif [ "$_ml_can_pm" = 1 ]; then
        info "minisign: not found — trying the package manager"
        # shellcheck disable=SC2086  # $_ml_sudo is an optional prefix word
        if command -v apt-get >/dev/null 2>&1; then
            { $_ml_sudo apt-get update && $_ml_sudo apt-get install -y minisign; } >/dev/null 2>&1 || true
        elif command -v dnf >/dev/null 2>&1; then
            $_ml_sudo dnf install -y minisign >/dev/null 2>&1 || true
        elif command -v yum >/dev/null 2>&1; then
            $_ml_sudo yum install -y minisign >/dev/null 2>&1 || true
        elif command -v apk >/dev/null 2>&1; then
            $_ml_sudo apk add minisign >/dev/null 2>&1 || true
        fi
    else
        info "minisign: not found, and no root or passwordless sudo — skipping the package manager"
    fi
    if command -v minisign >/dev/null 2>&1; then
        ok "minisign installed by the package manager"
    else
        if [ "$_ml_can_pm" = 1 ] && [ -z "${MINISIGN_SKIP_PM:-}" ]; then
            info "minisign: the package manager could not install it — trying the pinned upstream build"
        else
            info "minisign: trying the pinned upstream build"
        fi
        case "$ARCH" in
            amd64) _ml_sub=x86_64 ;;
            *)     _ml_sub=aarch64 ;;
        esac
        _ml_asset="minisign-$MINISIGN_VERSION-linux.tar.gz"
        if minisign_fetch "$_ml_asset" "$MINISIGN_LINUX_SHA256" \
           && tar xzf "$TMP/$_ml_asset" -C "$TMP" "minisign-linux/$_ml_sub/minisign" 2>/dev/null \
           && _ml_bin="$(minisign_install_file "$TMP/minisign-linux/$_ml_sub/minisign")" \
           && minisign_seal "$TMP/$_ml_asset" "$_ml_bin"; then
            MINISIGN="$_ml_bin"
            ok "minisign $MINISIGN_VERSION installed to $(dirname "$_ml_bin") (pinned upstream build)"
        else
            info "minisign: could not install the pinned upstream build (network, mirrors, its signature, or the destination is not writable)"
        fi
    fi
fi
