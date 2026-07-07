#!/bin/sh
# Clawee inner installer — clawee (POSIX sh).
#
# Ships at the ROOT of the verified release zip as `install.sh`. The outer
# bootstrap verifies the zip (minisign + sha256) and ONLY THEN execs this with
# cwd = the unzipped dir, so the `clawee` binary sits alongside this script.
# It installs clawee into PREFIX/bin (default $HOME/.local/bin), then ensures
# the burrowee-cli transport dependency is present. Set CLAWEE_UNINSTALL to
# remove clawee instead (the burrowee-cli dependency is left in place — it is
# managed by burrowee's own channel).
set -eu

BIN_DIR="${PREFIX:-$HOME/.local}/bin"
BINS="clawee clawee-updater"

# Update mode (set by `clawee update` via CLAWEE_UPDATE_MODE; the contract is
# cli/cmd/clawee-updater):
#   dry   — print what would change (the clawee binary version), then STOP.
#   apply — print the plan, then PROMPT before installing (the updater default).
#   auto  — install, unattended.
#   force — reinstall even when the version already matches.
# Empty = a fresh / direct install (no plan, no prompt). clawee is a client —
# no service to restart, so the plan is just the binary.
UPDATE_MODE="${CLAWEE_UPDATE_MODE:-}"

if [ -n "${CLAWEE_UNINSTALL:-}" ]; then
    for b in $BINS; do rm -f "$BIN_DIR/$b"; done
    echo "removed from $BIN_DIR: $BINS"
    exit 0
fi

# PLAN — what would change. Printed for any update mode. dry STOPS here; apply
# PROMPTS before installing; auto/force proceed unattended.
if [ -n "$UPDATE_MODE" ]; then
    staged_ver="$(./clawee --version 2>/dev/null | awk '{print $NF}')"
    installed_ver="$("$BIN_DIR/clawee" --version 2>/dev/null | awk '{print $NF}')"
    [ -n "$installed_ver" ] || installed_ver="(none)"
    if [ "$UPDATE_MODE" = force ] || [ "$installed_ver" != "$staged_ver" ]; then
        bin_line="$installed_ver -> $staged_ver   REPLACE"
    else
        bin_line="$staged_ver   unchanged"
    fi
    printf '\n  update plan (%s):\n' "$staged_ver"
    printf '    clawee binary    %s\n' "$bin_line"
    printf '    restart          not required (cli)\n\n'
    if [ "$UPDATE_MODE" = dry ]; then
        echo "  → plan only (--dry) — nothing changed. apply with --auto."
        exit 0
    fi
    if [ "$UPDATE_MODE" = apply ]; then
        if [ -r /dev/tty ]; then
            printf '  apply this update? [y/N] ' >/dev/tty
            ans=''; IFS= read -r ans </dev/tty || ans=''
            case "$ans" in y|Y|yes|YES) ;; *) echo "  → update skipped."; exit 0 ;; esac
        else
            echo "  → no tty for the prompt; re-run with --auto to install unattended." >&2
            exit 0
        fi
    fi
fi

mkdir -p "$BIN_DIR"
for b in $BINS; do
    [ -f "./$b" ] || { echo "missing $b in archive" >&2; exit 1; }
    install -m 0755 "./$b" "$BIN_DIR/$b"
    if [ "$(uname -s)" = "Darwin" ]; then
        xattr -d com.apple.quarantine "$BIN_DIR/$b" 2>/dev/null || true
    fi
done
echo "installed to $BIN_DIR: $BINS"

case ":$PATH:" in
    *":$BIN_DIR:"*) ;;
    *) echo "note: $BIN_DIR is not on PATH — add: export PATH=\"$BIN_DIR:\$PATH\"" ;;
esac

"$BIN_DIR/clawee" --version 2>/dev/null || true

# =========================================================================
# DEPENDENCY: burrowee-cli (the client transport clawee dials through)
#   No sudo here — burrowee's own installer escalates as it needs to. Install
#   when missing or older than the latest published; never downgrades. This is
#   the ONE public cross-channel step.
#
#   Threat model for the curl|sh below (accepted, by design): we pin transport
#   with --proto '=https' --tlsv1.2 (no http downgrade, no SSLv3/TLS1.0/1.1), so
#   the fetch is authenticated to release.burrowee.com by its TLS cert. We do
#   NOT minisign-verify the fetched bootstrap here: the burrowee bootstrap is its
#   OWN minisign trust-anchor — it verifies the burrowee-cli payload it then
#   downloads against burrowee's release key. Re-verifying here would only couple
#   clawee to burrowee's signing keys. We fetch to a var and pipe so a
#   truncated/failed fetch never partially executes.
# =========================================================================
dep_burrowee_cli() {
    if ! command -v curl >/dev/null 2>&1; then
        echo "note: curl not found — install burrowee-cli manually:" >&2
        echo "  curl -fsSL https://release.burrowee.com/cli/install.sh | sh" >&2
        return 0
    fi
    bootstrap="$(curl -fsSL --proto '=https' --tlsv1.2 \
        https://release.burrowee.com/cli/install.sh 2>/dev/null)" || {
        echo "note: cannot reach release.burrowee.com — skipping burrowee-cli check" >&2
        return 0; }

    # The burrowee bootstrap resolves the latest tag itself; we only need to
    # know whether we already have a burrowee-cli at all (and, if so, leave it —
    # the burrowee channel owns its upgrades). Run the burrowee installer only
    # when burrowee-cli is missing or unreadable; never downgrade an existing one.
    have="$(burrowee-cli --version 2>/dev/null | awk '{print $2}')"
    if [ -n "$have" ]; then
        echo "  ✓ burrowee-cli present ($have) — dependency satisfied"
        return 0
    fi
    echo "  → burrowee-cli not found — installing from burrowee's public channel"
    printf '%s' "$bootstrap" | sh || echo "warn: burrowee-cli install reported an error" >&2
}
dep_burrowee_cli

echo
echo "next: clawee status   (then: clawee)"
