#!/bin/sh
# Clawee inner installer — claweev2 (POSIX sh).
#
# Ships at the ROOT of the verified release zip as `install.sh`. The outer
# bootstrap verifies the zip (minisign + sha256) and ONLY THEN execs this with
# cwd = the unzipped dir, so the `claweev2` binary sits alongside this script.
# It installs claweev2 into PREFIX/bin (default $HOME/.local/bin), then ensures
# the burrowee-cli transport dependency is present. Set CLAWEE_UNINSTALL to
# remove claweev2 instead (the burrowee-cli dependency is left in place — it is
# managed by burrowee's own channel).
set -eu

BIN_DIR="${PREFIX:-$HOME/.local}/bin"
BINS="claweev2"

if [ -n "${CLAWEE_UNINSTALL:-}" ]; then
    for b in $BINS; do rm -f "$BIN_DIR/$b"; done
    echo "removed from $BIN_DIR: $BINS"
    exit 0
fi

# semver_lt A B — exit 0 iff version A is strictly older than B. Portable
# (no GNU `sort -V`): strips a leading 'v', splits on '.', compares fields
# numerically left-to-right, missing fields treated as 0.
semver_lt() {
    awk -v a="$1" -v b="$2" '
        function norm(s) { sub(/^v/, "", s); return s }
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

"$BIN_DIR/claweev2" --version 2>/dev/null || true

# =========================================================================
# DEPENDENCY: burrowee-cli (the client transport claweev2 dials through)
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
echo "next: claweev2 status   (then: claweev2)"
