#!/bin/sh
# tools/sync-modules.sh — compare this repo's tools/modules against another
# product's and copy in the ones that are newer.
#
#     sh tools/sync-modules.sh ../../../Clawee/release/code/main
#     sh tools/sync-modules.sh --repo <local-root> <remote-root>   # for tests
#
# Verdicts: UPDATED (remote newer — copied), ok (same version, same bytes),
# LOCAL FORK (same version, different bytes — never overwritten, resolve by
# hand), AHEAD (local newer — nothing copied; the other product is behind),
# NOT CARRIED (recorded in tools/modules/MODULES.exclude — this product's
# shipped logic is not this module, so nothing is copied or compared).
#
# NOT CARRIED is the verdict that keeps the other four honest. A module that
# sits in tools/modules/ without being @INCLUDEd anywhere reports "v1 == v1 ok",
# and the day upstream fixes a real defect and ships v2 it reports
# "v1 -> v2 UPDATED" — an operator commits the copy, every gate passes, and the
# generated bootstrap is byte-for-byte what it was, bug included. Recording the
# module as not-carried says so out loud on every run instead.
#
# Nothing is automatic: a person runs this and reviews the diff. The outer
# bootstrap is a trust anchor, and an unreviewed edit to one is exactly the
# risk this whole arrangement exists to reduce.
set -eu
LOCAL_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
if [ "${1:-}" = "--repo" ]; then LOCAL_ROOT="$2"; shift 2; fi
REMOTE_ROOT="${1:-}"
[ -n "$REMOTE_ROOT" ] || { echo "usage: sync-modules.sh [--repo <local>] <other-repo-root>" >&2; exit 64; }
[ -d "$REMOTE_ROOT/tools/modules" ] || { echo "✗ no tools/modules under $REMOTE_ROOT" >&2; exit 1; }
EXCLUDE="$LOCAL_ROOT/tools/modules/MODULES.exclude"

ver_of() {
    v="$(sed -n '1,4s/^# module:[[:space:]]*[a-z0-9-]*[[:space:]]*v\([0-9][0-9]*\).*/\1/p' "$1" | head -1)"
    case "$v" in
        ''|*[!0-9]*) echo "✗ $1: missing or malformed '# module: <name>  vN' header" >&2; exit 1 ;;
    esac
    echo "$v"
}
sha_of() {
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    else sha256sum "$1" | awk '{print $1}'; fi
}
# Only the first field of a non-comment row is a module name; the rest of the
# row is the human reason and is never parsed.
is_excluded() {
    [ -f "$EXCLUDE" ] || return 1
    awk -v n="$1" '/^[[:space:]]*#/ { next } $1 == n { hit = 1 } END { exit !hit }' "$EXCLUDE"
}

rc=0
for rf in "$REMOTE_ROOT"/tools/modules/*.sh; do
    name="$(basename "$rf" .sh)"
    lf="$LOCAL_ROOT/tools/modules/$name.sh"
    rv="$(ver_of "$rf")"
    if is_excluded "$name"; then
        printf '%-20s  --     v%-4s NOT CARRIED (MODULES.exclude — not copied)\n' "$name" "$rv"
        continue
    fi
    if [ ! -f "$lf" ]; then
        cp "$rf" "$lf"
        printf '%-20s   --  -> v%-4s NEW\n' "$name" "$rv"
        continue
    fi
    lv="$(ver_of "$lf")"
    if [ "$lv" -lt "$rv" ]; then
        cp "$rf" "$lf"
        printf '%-20s v%-3s -> v%-4s UPDATED\n' "$name" "$lv" "$rv"
    elif [ "$lv" -gt "$rv" ]; then
        printf '%-20s v%-3s    v%-4s AHEAD (they are behind)\n' "$name" "$lv" "$rv"
    elif [ "$(sha_of "$lf")" = "$(sha_of "$rf")" ]; then
        printf '%-20s v%-3s == v%-4s ok\n' "$name" "$lv" "$rv"
    else
        printf '%-20s v%-3s != v%-4s LOCAL FORK (same version, different bytes)\n' "$name" "$lv" "$rv"
        rc=1
    fi
done
echo
echo "re-run tools/lock-modules.sh and tools/test-modules.sh after any copy."
exit "$rc"
