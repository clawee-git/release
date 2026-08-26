#!/bin/sh
# tools/sync-modules.test.sh — the four verdicts sync-modules.sh must produce.
set -eu
HERE="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"; trap 'rm -rf "$W"' EXIT
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

mk() {  # mk <root> <name> <version> <body>
    mkdir -p "$1/tools/modules"
    printf '# module: %s  %s\n# since:  2026-08-25\n%s\n' "$2" "$3" "$4" > "$1/tools/modules/$2.sh"
}
mk "$W/local"  verify-checksum v2 'echo old'
mk "$W/remote" verify-checksum v3 'echo new'
mk "$W/local"  helpers v1 'echo same'
mk "$W/remote" helpers v1 'echo same'
mk "$W/local"  download v1 'echo mine'
mk "$W/remote" download v1 'echo theirs'
mk "$W/local"  sha256 v2 'echo newer-here'
mk "$W/remote" sha256 v1 'echo older-there'

# This fixture mixes a LOCAL FORK conflict in with UPDATED/ok/AHEAD, so the
# overall run must exit 1 (a conflict it will not resolve) even though it
# still prints every other verdict and copies the UPDATED module.
set +e
out="$(sh "$HERE/tools/sync-modules.sh" --repo "$W/local" "$W/remote")"
rc=$?
set -e
[ "$rc" -eq 1 ] || die "expected exit 1 for a LOCAL FORK conflict, got exit $rc: $out"
# Patterns tolerate the printf column padding ("v2  -> v3   UPDATED").
case "$out" in *"verify-checksum"*"v2"*"-> v3"*"UPDATED"*) ;; *) die "no UPDATED verdict: $out" ;; esac
case "$out" in *"helpers"*"ok"*)                        ;; *) die "no ok verdict: $out" ;; esac
case "$out" in *"download"*"LOCAL FORK"*)               ;; *) die "no LOCAL FORK verdict: $out" ;; esac
case "$out" in *"sha256"*"AHEAD"*)                      ;; *) die "no AHEAD verdict: $out" ;; esac
grep -q 'echo new'      "$W/local/tools/modules/verify-checksum.sh" || die "UPDATED module was not copied"
grep -q 'echo mine'     "$W/local/tools/modules/download.sh"        || die "LOCAL FORK was overwritten — it must never be"
grep -q 'echo newer-here' "$W/local/tools/modules/sha256.sh"        || die "AHEAD module was overwritten"
printf '\nALL OK — four verdicts, and only UPDATED copies\n'
