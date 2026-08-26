#!/bin/sh
# tools/lock-modules.sh — rewrite tools/modules/MODULES.lock from the modules on
# disk. Run after adding a module or bumping one's version; tools/test-modules.sh
# fails while the lock and the files disagree.
set -eu
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODDIR="$ROOT/tools/modules"
sha_of() {
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    else sha256sum "$1" | awk '{print $1}'; fi
}
{
    echo "# module            version  sha256-of-authored-text"
    echo "# rewritten by tools/lock-modules.sh — do not hand-edit"
    for f in "$MODDIR"/*.sh; do
        n="$(basename "$f" .sh)"
        v="$(sed -n '1,4s/^# module:[[:space:]]*[a-z0-9-]*[[:space:]]*\(v[0-9][0-9]*\).*/\1/p' "$f")"
        [ -n "$v" ] || { echo "✗ $f has no '# module: <name> v<N>' header" >&2; exit 1; }
        printf '%-18s %-8s %s\n' "$n" "$v" "$(sha_of "$f")"
    done
} > "$MODDIR/MODULES.lock.tmp.$$"
mv -f "$MODDIR/MODULES.lock.tmp.$$" "$MODDIR/MODULES.lock"
echo "✓ wrote $MODDIR/MODULES.lock"
