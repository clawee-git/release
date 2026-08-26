#!/bin/sh
# tools/sync-modules.test.sh — the six verdicts sync-modules.sh must produce,
# and the three copies it must NOT make.
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
# Remote-only, NOT excluded: the NEW path must copy it in.
mk "$W/remote" platform-detect v1 'echo fresh'
# Remote-only AND excluded: the NEW path must leave it absent. This is the
# adoption bug the marker exists for — a relay-only module was twice copied
# into a product with no relay channel, caught only by reading the diff.
mk "$W/remote" download-r2-only v1 'echo relay-only'
# Carried locally BUT excluded: must report NOT CARRIED, never "ok" or
# "UPDATED". Without the marker this is the false green — a module nothing
# @INCLUDEs syncing clean for code that does not ship.
mk "$W/local"  version-resolve v1 'echo local-fork-lives-in-the-template'
mk "$W/remote" version-resolve v2 'echo upstream-fixed-a-real-bug'
cat > "$W/local/tools/modules/MODULES.exclude" <<'EOF'
# module            why this product does not carry it
download-r2-only   gated relay channel this product does not have
version-resolve    local fork in tools/bootstrap.template.sh
EOF

# This fixture mixes a LOCAL FORK conflict in with the other verdicts, so the
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
case "$out" in *"platform-detect"*"NEW"*)               ;; *) die "no NEW verdict: $out" ;; esac
case "$out" in *"download-r2-only"*"NOT CARRIED"*)      ;; *) die "an excluded remote-only module was not reported NOT CARRIED: $out" ;; esac
case "$out" in *"version-resolve"*"NOT CARRIED"*)       ;; *) die "an excluded carried module was not reported NOT CARRIED: $out" ;; esac
case "$out" in *"version-resolve"*"UPDATED"*) die "an excluded module was reported UPDATED: $out" ;; esac
# Verdicts are printed text; what shipped is what is on disk. Assert the bytes.
grep -q 'echo new'      "$W/local/tools/modules/verify-checksum.sh" || die "UPDATED module was not copied"
grep -q 'echo mine'     "$W/local/tools/modules/download.sh"        || die "LOCAL FORK was overwritten — it must never be"
grep -q 'echo newer-here' "$W/local/tools/modules/sha256.sh"        || die "AHEAD module was overwritten"
grep -q 'echo fresh'    "$W/local/tools/modules/platform-detect.sh" || die "NEW module was not copied"
[ ! -e "$W/local/tools/modules/download-r2-only.sh" ] \
    || die "the NEW path copied in a module recorded in MODULES.exclude"
grep -q 'echo local-fork-lives-in-the-template' "$W/local/tools/modules/version-resolve.sh" \
    || die "an excluded module was overwritten by the UPDATED path"
printf '\nALL OK — six verdicts; only UPDATED and NEW copy, and neither touches an excluded module\n'
