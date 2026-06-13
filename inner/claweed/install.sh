#!/bin/sh
# claweed installer — LOCAL-SOURCE variant (POSIX sh, macOS + Linux).
#
# This is the Phase-B `claweed` release-component installer (it installs the
# claweed PTY daemon + the setuid clawee-spawn helper, and cross-installs the
# burrowee-gateway dependency), in a variant that sources clawee's OWN binaries
# from this staged directory instead of a signed GitHub release fronted by
# release.clawee.org. It is generated + staged by install/build-local.sh; the
# claweed + clawee-spawn binaries sit alongside this script.
#
# SUDO-MINIMAL model. Run this AS THE USER — do NOT prefix it with sudo. Almost
# everything (the claweed binary, the data-dir, the launchd/systemd boot unit in
# your own user domain, the gateway dependency, the doctor tail) installs with NO
# privilege. The script escalates with `sudo` for EXACTLY ONE tier: the
# setuid-root spawn helper + its root-owned allowlist (Tier S). If passwordless
# sudo isn't available, the rest of the install still completes and the exact
# Tier-S sudo block is printed for you to run by hand.
#
# Usage (from the staged dir):
#   sh ./install.sh                 # interactive: prompts before the boot step
#   sh ./install.sh --yes           # unattended: assume-yes; also loads the boot unit
#   sh ./install.sh uninstall       # remove claweed + the setuid helper (keeps data)
#   sh ./install.sh uninstall --purge   # also remove ~/.clawee/data (tenant trust keys!)
#
# Env:
#   CLAWEE_PREFIX           install root for the USER-tier claweed binary
#                           (default: ~/.local/bin; user-writable, no sudo)
#   CLAWEE_REGISTER_SOCKET  burrowee-gateway register socket the daemon dials
#                           (default: auto-detected from the running gateway)
#   CLAWEE_DATA_DIR         claweed --data-dir (default: ~/.clawee/data)
#
# Only clawee's own channel is replaced by this local source; the burrowee
# dependency still installs from burrowee's PUBLIC channel
# (release.burrowee.com/gateway/install.sh).
set -eu

# ---- knobs --------------------------------------------------------------
# Tier U (no sudo): the claweed binary lands in a USER-writable prefix.
PREFIX="${CLAWEE_PREFIX:-$HOME/.local/bin}"
CLAWEED_BIN="$PREFIX/claweed"

# Tier S (sudo only): the setuid spawn helper + allowlist MUST live in a
# root-owned, NON-user-writable dir. A user-writable setuid-root binary is a
# trivial local-root escalation (anyone overwrites it, then triggers it as
# root) — so these paths are HARDCODED to /usr/local and are NEVER derived from
# CLAWEE_PREFIX. Do not move them under ~/.local/bin or any user-writable root.
SPAWN_DIR="/usr/local/bin"
SPAWN_HELPER="$SPAWN_DIR/clawee-spawn"
ETC_DIR="/usr/local/etc/clawee"
ALLOW_FILE="$ETC_DIR/spawn-allow"
# Legacy: the old all-sudo installer put claweed here. We clean it up below.
LEGACY_CLAWEED_BIN="/usr/local/bin/claweed"

LABEL="org.clawee.claweed"
VERSION="(rendered-at-build)"

# Minimum burrowee-gateway version this claweed expects (the register-link
# contract floor). Empty = accept any installed gateway; only install when none.
GATEWAY_FLOOR=""

# ---- helpers ------------------------------------------------------------
fail() { printf '\n  \xe2\x9c\x97 %s\n\n' "$*" >&2; exit 1; }
info() { printf '  \xe2\x86\x92 %s\n' "$*"; }
ok()   { printf '  \xe2\x9c\x93 %s\n' "$*"; }
warn() { printf '  ! %s\n' "$*" >&2; }

# ---- argument parse -----------------------------------------------------
# install.sh                 -> install (default)
# install.sh uninstall [...]  -> uninstall mode
# --yes/-y                   -> assume-yes (install: load boot unit unattended)
# --purge                    -> uninstall: also remove the data-dir
# -h/--help                  -> usage
MODE=install
ASSUME_YES=0
PURGE=0
usage() {
    cat <<EOF
claweed installer ($VERSION) — sudo-minimal

  sh ./install.sh [--yes]              install (run AS THE USER, not sudo)
  sh ./install.sh uninstall [--purge]  remove claweed + the setuid helper
  sh ./install.sh -h                   this help

  --yes      unattended install; load the boot unit without prompting
  --purge    uninstall only: also delete ~/.clawee/data (TENANT TRUST KEYS)
EOF
}
# Consume an optional leading subcommand (install|uninstall), then flags.
case "${1:-}" in
    uninstall|--uninstall) MODE=uninstall; shift ;;
    install) MODE=install; shift ;;
esac
for arg in "$@"; do
    case "$arg" in
        --yes|-y) ASSUME_YES=1 ;;
        --purge)  PURGE=1 ;;
        -h|--help) usage; exit 0 ;;
        *) fail "unknown argument: $arg (try: sh ./install.sh -h)" ;;
    esac
done

# semver_lt A B — exit 0 iff version A is strictly older than B. Portable
# (no GNU `sort -V`): strips a leading 'v', splits on '.', compares fields
# numerically left-to-right, missing fields treated as 0. Non-numeric/garbage
# fields compare as 0.
semver_lt() {
    awk -v a="$1" -v b="$2" '
        function norm(s,   t) { sub(/^v/, "", s); return s }
        BEGIN {
            na = split(norm(a), x, ".")
            nb = split(norm(b), y, ".")
            n = (na > nb) ? na : nb
            for (i = 1; i <= n; i++) {
                xi = (i <= na) ? x[i] + 0 : 0
                yi = (i <= nb) ? y[i] + 0 : 0
                if (xi < yi) { exit 0 }
                if (xi > yi) { exit 1 }
            }
            exit 1   # equal -> not strictly less
        }'
}

# confirm PROMPT — returns 0 on yes. --yes makes it unconditional.
confirm() {
    [ "$ASSUME_YES" -eq 1 ] && return 0
    [ -r /dev/tty ] || { warn "no tty for prompt; re-run with --yes to proceed unattended"; return 1; }
    printf '  %s [y/N] ' "$1" >/dev/tty
    ans=''
    IFS= read -r ans </dev/tty || ans=''
    case "$ans" in y|Y|yes|YES) return 0 ;; *) return 1 ;; esac
}

# ---- identity + data-dir ------------------------------------------------
# This script runs AS THE INVOKING USER (no leading sudo), so id/$HOME are the
# real human's. The spawn allowlist trusts THIS uid — it's the uid claweed runs
# as. The data-dir is this user's ~/.clawee/data.
USER_UID="$(id -u)"
USER_NAME="$(id -un)"
DATA_DIR="${CLAWEE_DATA_DIR:-$HOME/.clawee/data}"

# ---- platform detection -------------------------------------------------
case "$(uname -s)" in
    Darwin) OS=darwin ;;
    Linux)  OS=linux ;;
    *)      fail "unsupported OS: $(uname -s) (claweed ships darwin + linux only)" ;;
esac
case "$(uname -m)" in
    arm64|aarch64) ARCH=arm64 ;;
    x86_64|amd64)  ARCH=amd64 ;;
    *)             fail "unsupported arch: $(uname -m)" ;;
esac
KIND=$( [ "$OS" = darwin ] && echo launchd || echo systemd )

# resolve the directory holding this script (the staged dir with the binaries).
SELF_DIR="$(cd "$(dirname "$0")" && pwd)"

# =========================================================================
# UNINSTALL MODE
# =========================================================================
if [ "$MODE" = uninstall ]; then
    printf '\n  claweed uninstaller  %s  (%s/%s)\n\n' "$VERSION" "$OS" "$ARCH"

    # ---- Tier U (no sudo): boot unit + user binary + (optionally) data ---
    if [ "$OS" = darwin ]; then
        UNIT_PATH="$HOME/Library/LaunchAgents/$LABEL.plist"
        if launchctl bootout "gui/$USER_UID/$LABEL" 2>/dev/null; then
            ok "unloaded LaunchAgent (gui/$USER_UID/$LABEL)"
        else
            info "LaunchAgent not loaded — nothing to unload"
        fi
        if [ -f "$UNIT_PATH" ]; then
            rm -f "$UNIT_PATH"; ok "removed $UNIT_PATH"
        else
            info "no LaunchAgent plist at $UNIT_PATH — skipped"
        fi
    else
        UNIT_PATH="$HOME/.config/systemd/user/claweed.service"
        if systemctl --user disable --now claweed.service 2>/dev/null; then
            ok "disabled + stopped systemd user unit"
        else
            info "systemd user unit not active — nothing to disable"
        fi
        if [ -f "$UNIT_PATH" ]; then
            rm -f "$UNIT_PATH"
            systemctl --user daemon-reload 2>/dev/null || true
            ok "removed $UNIT_PATH"
        else
            info "no systemd unit at $UNIT_PATH — skipped"
        fi
    fi

    if [ -f "$CLAWEED_BIN" ]; then
        rm -f "$CLAWEED_BIN" && ok "removed $CLAWEED_BIN"
    else
        info "no claweed binary at $CLAWEED_BIN — skipped"
    fi

    if [ "$PURGE" -eq 1 ]; then
        if [ -d "$DATA_DIR" ]; then
            warn "--purge: removing the data-dir $DATA_DIR (this deletes tenant trust keys; enrolled devices must re-enroll)"
            rm -rf "$DATA_DIR" && ok "removed data-dir $DATA_DIR"
        else
            info "no data-dir at $DATA_DIR — skipped"
        fi
    else
        if [ -d "$DATA_DIR" ]; then
            info "kept data-dir $DATA_DIR (holds tenant trust keys; pass --purge to remove)"
        fi
    fi

    # ---- Tier S (sudo, only if anything root-owned is present) -----------
    NEED_TIER_S=0
    [ -e "$SPAWN_HELPER" ] && NEED_TIER_S=1
    [ -e "$ALLOW_FILE" ] && NEED_TIER_S=1
    [ -e "$LEGACY_CLAWEED_BIN" ] && NEED_TIER_S=1
    if [ "$NEED_TIER_S" -eq 0 ]; then
        info "no root-owned spawn helper / allowlist present — nothing for sudo to remove"
        printf '\n  \xe2\x9c\x93 claweed uninstalled (local-source, %s)\n\n' "$VERSION"
        exit 0
    fi

    SUDO=""
    if [ "$USER_UID" -eq 0 ]; then
        SUDO=""
    elif sudo -n true 2>/dev/null; then
        SUDO="sudo"
    else
        cat >&2 <<EOF

  ! the setuid spawn helper + allowlist are root-owned — passwordless sudo is
    not available, so remove them by hand:

      sudo rm -f '$SPAWN_HELPER' '$ALLOW_FILE'
      sudo rmdir '$ETC_DIR' 2>/dev/null || true
EOF
        if [ -e "$LEGACY_CLAWEED_BIN" ]; then
            printf "      sudo rm -f '%s'   # legacy all-sudo install\n" "$LEGACY_CLAWEED_BIN" >&2
        fi
        printf '\n' >&2
        warn "claweed uninstalled (user tier); the root-owned spawn tier was left for the manual block above"
        exit 4
    fi

    if [ -e "$SPAWN_HELPER" ]; then
        $SUDO rm -f "$SPAWN_HELPER" && ok "removed $SPAWN_HELPER"
    fi
    if [ -e "$ALLOW_FILE" ]; then
        $SUDO rm -f "$ALLOW_FILE" && ok "removed $ALLOW_FILE"
    fi
    # rmdir only when empty — never recursive (it's a shared /usr/local tree).
    if [ -d "$ETC_DIR" ] && $SUDO rmdir "$ETC_DIR" 2>/dev/null; then
        ok "removed empty $ETC_DIR"
    fi
    if [ -e "$LEGACY_CLAWEED_BIN" ]; then
        $SUDO rm -f "$LEGACY_CLAWEED_BIN" && ok "removed legacy $LEGACY_CLAWEED_BIN"
    fi

    printf '\n  \xe2\x9c\x93 claweed uninstalled (local-source, %s)\n\n' "$VERSION"
    exit 0
fi

# =========================================================================
# INSTALL MODE
# =========================================================================
printf '\n  claweed installer (local-source)  %s  (%s/%s)\n\n' "$VERSION" "$OS" "$ARCH"

# Refuse a blanket-sudo run: under sudo $HOME/id are root's, so the whole
# user tier would land in root's home + trust root's uid — exactly the old
# broken model this restructure removes. Tier S escalates per-command instead.
if [ "$USER_UID" -eq 0 ] && [ -n "${SUDO_USER:-}" ]; then
    fail "don't run this under sudo — run it AS YOUR USER (the script escalates with sudo only for the spawn helper). Re-run: sh ./install.sh"
fi

# ---- preflight: staged binaries present ---------------------------------
for b in claweed clawee-spawn; do
    [ -f "$SELF_DIR/$b" ] || fail "missing $b in $SELF_DIR — re-run build-local.sh to stage the binaries"
done

# =========================================================================
# TIER U (no sudo): claweed -> $PREFIX (0755), as the invoking user.
# =========================================================================
info "installing claweed -> $PREFIX"
mkdir -p "$PREFIX" || fail "could not create $PREFIX (set CLAWEE_PREFIX to a writable dir)"
[ -w "$PREFIX" ] || fail "$PREFIX is not writable — set CLAWEE_PREFIX to a user-writable dir"
install -m 0755 "$SELF_DIR/claweed" "$CLAWEED_BIN"
[ "$OS" = darwin ] && xattr -d com.apple.quarantine "$CLAWEED_BIN" 2>/dev/null || true
ok "claweed installed: $($CLAWEED_BIN --version 2>/dev/null || echo claweed)"

# PATH hint: ~/.local/bin is frequently NOT on PATH.
if ! command -v claweed >/dev/null 2>&1; then
    warn "claweed is not on PATH yet — add $PREFIX to PATH:"
    warn "  export PATH=\"$PREFIX:\$PATH\"   # add to ~/.profile or your shell rc"
else
    case ":$PATH:" in
        *":$PREFIX:"*) ;;
        *) warn "$PREFIX is not on PATH — add: export PATH=\"$PREFIX:\$PATH\"" ;;
    esac
fi

# ---- data-dir (this user's ~/.clawee/data, 0700) — no sudo --------------
# Never clobber an existing data-dir — it holds tenant trust keys. Only create
# (0700) when absent; leave an existing one untouched.
if [ -d "$DATA_DIR" ]; then
    ok "data-dir exists -> $DATA_DIR (left untouched)"
else
    info "creating data-dir -> $DATA_DIR (0700)"
    install -d -m 700 "$DATA_DIR" || fail "could not create data-dir $DATA_DIR"
    ok "data-dir ready"
fi

# ---- register socket (detect early; unprivileged) -----------------------
# Resolve the burrowee-gateway register.sock the daemon dials: explicit env
# wins; else detect from open unix sockets. Done BEFORE the Tier-S block so the
# printed manual hint can quote the real path. lsof can return several matches
# (multiple gateways) — only auto-use a UNIQUE match; on 0 or >1 leave empty.
REGISTER_SOCKET="${CLAWEE_REGISTER_SOCKET:-}"
if [ -z "$REGISTER_SOCKET" ] && command -v lsof >/dev/null 2>&1; then
    socket_matches="$(lsof -U 2>/dev/null | awk '/register\.sock/ {print $NF}' | sort -u)"
    match_count="$(printf '%s' "$socket_matches" | grep -c . || true)"
    if [ "$match_count" -eq 1 ]; then
        REGISTER_SOCKET="$socket_matches"
    elif [ "$match_count" -gt 1 ]; then
        warn "multiple burrowee-gateway register sockets found — cannot auto-pick:"
        printf '%s\n' "$socket_matches" | while IFS= read -r s; do warn "    $s"; done
        warn "set CLAWEE_REGISTER_SOCKET=<path> and re-run to choose one"
    fi
fi

# =========================================================================
# TIER S (sudo, ONLY this): setuid clawee-spawn + root-owned allowlist.
#
#   SECURITY INVARIANT: the setuid-root binary stays root-owned in a
#   NON-user-writable dir (/usr/local/bin), and the allowlist stays root-owned,
#   non-group/world-writable (0644). A user-writable setuid file would let any
#   local user overwrite it and run arbitrary code as root.
#
#   need_sudo posture: run the Tier-S commands with sudo if we already are root
#   or passwordless sudo works; otherwise PRINT the exact Tier-S block (the rest
#   of the install is done — the spawn helper is the only remainder) and exit
#   non-zero so the operator knows to run it.
# =========================================================================
SPAWN_SKIPPED=0
SUDO=""
if [ "$USER_UID" -eq 0 ]; then
    SUDO=""
elif sudo -n true 2>/dev/null; then
    SUDO="sudo"
else
    SPAWN_SKIPPED=1
fi

if [ "$SPAWN_SKIPPED" -eq 1 ]; then
    cat >&2 <<EOF

  ! Tier S (the ONLY privileged step) was skipped — no passwordless sudo.
    claweed is installed and your boot unit can be loaded, but clawee-spawn must
    be placed setuid-root and the allowlist written by root. Run this exact
    block (it's the whole remainder):

      sudo install -m 4755 -o root '$SELF_DIR/clawee-spawn' '$SPAWN_HELPER'
      sudo mkdir -p '$ETC_DIR'
      printf '%s\n' '$USER_UID' | sudo tee '$ALLOW_FILE' >/dev/null
      sudo chown root '$ALLOW_FILE' && sudo chmod 0644 '$ALLOW_FILE'
EOF
    if [ -e "$LEGACY_CLAWEED_BIN" ]; then
        printf "      sudo rm -f '%s'   # legacy all-sudo install of claweed\n" "$LEGACY_CLAWEED_BIN" >&2
    fi
    printf '\n' >&2
else
    info "installing clawee-spawn setuid-root (4755) -> $SPAWN_HELPER"
    $SUDO install -m 4755 -o root "$SELF_DIR/clawee-spawn" "$SPAWN_HELPER"
    [ "$OS" = darwin ] && $SUDO xattr -d com.apple.quarantine "$SPAWN_HELPER" 2>/dev/null || true
    ok "clawee-spawn installed setuid-root (root-owned, non-user-writable dir)"

    info "writing spawn allowlist (uid $USER_UID, user $USER_NAME) -> $ALLOW_FILE"
    $SUDO mkdir -p "$ETC_DIR"
    printf '%s\n' "$USER_UID" | $SUDO tee "$ALLOW_FILE" >/dev/null
    $SUDO chown root "$ALLOW_FILE"
    $SUDO chmod 0644 "$ALLOW_FILE"
    ok "allowlist written (root-owned, 0644)"

    # Legacy cleanup: the old all-sudo installer placed claweed in /usr/local/bin.
    # The user-tier binary now lives in $PREFIX, so a stale copy there shadows it
    # on PATH — offer to remove it (Tier S, sudo).
    if [ -e "$LEGACY_CLAWEED_BIN" ] && [ "$LEGACY_CLAWEED_BIN" != "$CLAWEED_BIN" ]; then
        if confirm "remove the legacy root-owned $LEGACY_CLAWEED_BIN (from the old all-sudo install)?"; then
            $SUDO rm -f "$LEGACY_CLAWEED_BIN" && ok "removed legacy $LEGACY_CLAWEED_BIN"
        else
            warn "kept $LEGACY_CLAWEED_BIN — it may shadow $CLAWEED_BIN on PATH; remove with: sudo rm -f '$LEGACY_CLAWEED_BIN'"
        fi
    fi
fi

# =========================================================================
# DEPENDENCY: burrowee-gateway (cross-channel, PUBLIC burrowee installer)
#   No sudo here — burrowee's own installer escalates as it needs to. Install
#   when missing or older than floor; never downgrades. This is the ONE step
#   that stays public in this variant.
# =========================================================================
dep_burrowee_gateway() {
    have="$(burrowee-gateway --version 2>/dev/null | awk '{print $2}')"
    if [ -z "$have" ]; then
        info "burrowee-gateway not found — installing from burrowee's public channel"
    elif [ -n "$GATEWAY_FLOOR" ] && semver_lt "$have" "$GATEWAY_FLOOR"; then
        info "burrowee-gateway $have is older than floor $GATEWAY_FLOOR — upgrading"
    else
        ok "burrowee-gateway present ($have) — dependency satisfied"
        return 0
    fi
    if ! command -v curl >/dev/null 2>&1; then
        warn "curl not found — cannot install burrowee-gateway; install it manually:"
        warn "  curl -fsSL https://release.burrowee.com/gateway/install.sh | sh"
        return 0
    fi
    # Threat model for the curl|sh below (accepted, by design): this is the ONE
    # public cross-channel step. We pin transport with --proto '=https' --tlsv1.2
    # (no http downgrade, no SSLv3/TLS1.0/1.1), so the fetch is authenticated to
    # release.burrowee.com by its TLS cert. We deliberately do NOT minisign-verify
    # the fetched bootstrap here: the burrowee bootstrap is its OWN minisign
    # trust-anchor — it verifies the gateway payload it then downloads against
    # burrowee's release key. Re-verifying it from clawee's installer would only
    # duplicate (and couple us to) burrowee's signing keys. We fetch to a var and
    # pipe so a truncated/failed fetch never partially executes.
    bootstrap="$(curl -fsSL --proto '=https' --tlsv1.2 \
        https://release.burrowee.com/gateway/install.sh 2>/dev/null)" || {
        warn "cannot reach release.burrowee.com — skipping burrowee-gateway install"; return 0; }
    printf '%s' "$bootstrap" | sh || warn "burrowee-gateway install reported an error"
}
dep_burrowee_gateway

# =========================================================================
# BOOT UNIT (no sudo): render via `claweed print-boot-unit`, write under THIS
#   user's home, and load it in THIS user's domain. Uses the register socket
#   resolved earlier (explicit env or unique detect). The spawn-helper flag
#   points at the Tier-S path even if Tier S was skipped — the helper is
#   expected to be installed there (now or via the printed block).
# =========================================================================
if [ -z "$REGISTER_SOCKET" ]; then
    warn "could not detect the burrowee-gateway register socket."
    warn "render the boot unit yourself once the gateway is running:"
    warn "  $CLAWEED_BIN print-boot-unit --kind=$KIND --claweed '$CLAWEED_BIN' \\"
    warn "    --data-dir '$DATA_DIR' --spawn-helper '$SPAWN_HELPER' --register-socket <path>"
else
    info "register socket: $REGISTER_SOCKET"
    if [ "$OS" = darwin ]; then
        # The LaunchAgent belongs to THIS user's GUI domain — write it under
        # this user's home and load it as this user (no sudo).
        UNIT_DIR="$HOME/Library/LaunchAgents"
        UNIT_PATH="$UNIT_DIR/$LABEL.plist"
        LOG_PATH="$HOME/Library/Logs/claweed.log"
        mkdir -p "$UNIT_DIR" "$(dirname "$LOG_PATH")"
        "$CLAWEED_BIN" print-boot-unit --kind=launchd \
            --claweed "$CLAWEED_BIN" --data-dir "$DATA_DIR" \
            --spawn-helper "$SPAWN_HELPER" --register-socket "$REGISTER_SOCKET" \
            --log "$LOG_PATH" >"$UNIT_PATH" \
            || fail "print-boot-unit failed"
        ok "wrote LaunchAgent $UNIT_PATH"
        # Load into THIS user's GUI domain (gui/<uid>). bootout-then-bootstrap;
        # kickstart if already loaded.
        domain="gui/$USER_UID"
        manual_hint="launchctl bootstrap $domain '$UNIT_PATH'"
        if confirm "load the LaunchAgent now (launchctl)?"; then
            launchctl bootout "$domain/$LABEL" 2>/dev/null || true
            if launchctl bootstrap "$domain" "$UNIT_PATH" 2>/dev/null; then
                ok "launchd unit loaded ($domain)"
            elif launchctl kickstart -k "$domain/$LABEL" 2>/dev/null; then
                ok "launchd unit already loaded — kickstarted ($domain)"
            else
                warn "could not load the LaunchAgent — load it manually:"
                warn "  $manual_hint"
            fi
        else
            info "skipped load — run later: $manual_hint"
        fi
    else
        # The unit belongs to THIS user's systemd user manager — write it under
        # ~/.config and drive systemctl --user (no sudo).
        UNIT_DIR="$HOME/.config/systemd/user"
        UNIT_PATH="$UNIT_DIR/claweed.service"
        mkdir -p "$UNIT_DIR"
        "$CLAWEED_BIN" print-boot-unit --kind=systemd \
            --claweed "$CLAWEED_BIN" --data-dir "$DATA_DIR" \
            --spawn-helper "$SPAWN_HELPER" --register-socket "$REGISTER_SOCKET" \
            --user "$USER_NAME" >"$UNIT_PATH" \
            || fail "print-boot-unit failed"
        ok "wrote systemd user unit $UNIT_PATH"
        manual_hint="systemctl --user enable --now claweed.service"
        if confirm "enable + start the systemd user unit now?"; then
            systemctl --user daemon-reload 2>/dev/null || true
            if systemctl --user enable --now claweed.service 2>/dev/null; then
                ok "systemd unit started (user manager, uid $USER_UID)"
            else
                warn "could not start the unit — start it manually:"
                warn "  $manual_hint"
            fi
        else
            info "skipped — run later: $manual_hint"
        fi
    fi
fi

# =========================================================================
# DOCTOR (informational): run the deployment diagnosis with the same paths the
#   boot unit uses. Runs AS THIS USER directly — the whole script is the user,
#   so no `sudo -u` is needed: doctor's checks are user-relative (the allowlist
#   trusts this uid, the LaunchAgent lives in this user's gui/<uid> domain, the
#   setuid helper accepts only this allowlisted caller). `claweed doctor` needs
#   (--register-socket|--socket), so skip when neither is known. Older binaries
#   lack the verb — detect via the usage text so a build without it never errors.
# =========================================================================
if ! "$CLAWEED_BIN" 2>&1 | grep -q 'doctor'; then
    info "claweed doctor not available in this build — skipping (informational)"
elif [ -z "$REGISTER_SOCKET" ]; then
    info "claweed doctor skipped — no register socket known (set CLAWEE_REGISTER_SOCKET)"
else
    printf '\n'
    # Informational only: a ✗ check must not fail the install (|| true).
    "$CLAWEED_BIN" doctor --data-dir "$DATA_DIR" --spawn-helper "$SPAWN_HELPER" \
        --register-socket "$REGISTER_SOCKET" 2>&1 || true
fi

# =========================================================================
# COMPLETION: non-zero exit if the Tier-S spawn step was skipped, so callers
#   (and the operator) know the install isn't fully done until the printed
#   block runs.
# =========================================================================
if [ "$SPAWN_SKIPPED" -eq 1 ]; then
    printf '\n  ! claweed installed, but the spawn helper is NOT installed — run the printed Tier-S sudo block to finish.\n\n' >&2
    exit 3
fi

printf '\n  \xe2\x9c\x93 claweed install complete (local-source, %s)\n\n' "$VERSION"
