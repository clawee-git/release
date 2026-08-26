#!/bin/sh
# tools/test-modules.sh — the module gates.
#
# (1) LOCK      — every module's recorded sha256 matches its bytes, and every
#                 module on disk is listed. An edit without a version bump is
#                 what makes "which copy is newer" unanswerable across products.
# (2) DEPS      — every `# needs:` dependency is included EARLIER in each
#                 generated bootstrap that includes the dependent module. An
#                 undeclared order bug surfaces on an operator's machine as
#                 "command not found", after the download and before the gate.
#                 Also: any module (other than helpers itself) that CALLS
#                 fail/info/ok must declare `# needs: helpers` even if no
#                 bootstrap yet exposes the gap — the DEPS ordering check above
#                 only believes what a module declares about itself, so an
#                 under-declared dependency passes it clean today and dies on
#                 an operator's machine the day some other template composition
#                 splices this module without helpers ahead of it.
# (3) GENERATOR — regenerating leaves every committed bootstrap byte-identical.
set -eu

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
MODDIR="$ROOT/tools/modules"
LOCK="$MODDIR/MODULES.lock"
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

sha_of() {
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    elif command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    else die "neither shasum nor sha256sum found"; fi
}

printf '\n=== LOCK: modules match MODULES.lock ===\n'
[ -f "$LOCK" ] || die "missing $LOCK"
while read -r name version want; do
    [ -n "${name:-}" ] || continue
    case "$name" in \#*) continue ;; esac
    f="$MODDIR/$name.sh"
    [ -f "$f" ] || die "$LOCK lists '$name' but $f does not exist"
    got="$(sha_of "$f")"
    [ "$got" = "$want" ] \
        || die "$name changed without a version bump (lock says $version/$want, file is $got)
    bump the '# module:' header and re-run tools/lock-modules.sh"
    hdr="$(sed -n '1,4s/^# module:[[:space:]]*\([a-z0-9-]*\)[[:space:]]*v\([0-9][0-9]*\).*/\1 v\2/p' "$f")"
    [ "$hdr" = "$name $version" ] \
        || die "$f header says '$hdr', $LOCK says '$name $version'"
done < "$LOCK"
for f in "$MODDIR"/*.sh; do
    n="$(basename "$f" .sh)"
    grep -q "^$n " "$LOCK" || die "$f is not listed in $LOCK"
done
printf '  OK\n'

# The generator's OWN generated locations, anchored explicitly — NOT a git
# pathspec glob. '*/install.sh' etc. would also match any hand-written
# inner/<comp>/install.sh: a glob crosses '/' in a git pathspec, so any later
# task touching an inner installer would get a false "stale, commit the
# regeneration" naming a file the generator never writes. Shared by the DEPS
# loop and the GENERATOR diff below. Clawee generates two components (clawee,
# claweed), each with install.sh + upgrade.sh only — no preflight (Clawee's
# generator has no OS-dependency preflight step) and no relay (Clawee has no
# gated channel).
GENERATED_REL="clawee/install.sh clawee/upgrade.sh \
claweed/install.sh claweed/upgrade.sh"

printf '\n=== DEPS: every "# needs:" is included earlier ===\n'
for relgen in $GENERATED_REL; do
    gen="$ROOT/$relgen"
    [ -f "$gen" ] || continue
    order="$(sed -n 's/^# BEGIN \([a-z0-9-]*\)$/\1/p' "$gen")"
    for mod in $order; do
        needs="$(sed -n '1,6s/^# needs:[[:space:]]*//p' "$MODDIR/$mod.sh" 2>/dev/null || true)"
        for dep in $needs; do
            seen=0
            for m in $order; do
                [ "$m" = "$dep" ] && seen=1
                [ "$m" = "$mod" ] && break
            done
            [ "$seen" = 1 ] || die "$gen includes '$mod' which needs '$dep', but '$dep' is not included before it"
        done
    done
done
printf '  OK\n'

printf '\n=== DEPS: modules calling fail/info/ok declare needs: helpers ===\n'
# A module's own `# needs:` line is the only thing the ordering check above
# trusts — it never looks at what the module's BODY actually calls. Catch the
# mismatch directly: search each module (other than helpers itself) for an
# invocation — not merely the word — of fail/info/ok, and require `helpers` in
# its needs line if found. The pattern requires a non-identifier character (or
# start of line) before the name and the start of a quoted/variable argument
# right after it, so `check_ok(`, a comment mentioning "ok", or the word "ok"
# inside a message string do not trip it — only an actual `fail "..."` /
# `info "..."` / `ok "..."`-shaped call does.
for f in "$MODDIR"/*.sh; do
    n="$(basename "$f" .sh)"
    [ "$n" = "helpers" ] && continue
    body="$(grep -vE '^[[:space:]]*#' "$f")"
    calls=""
    for fn in fail info ok; do
        pat="(^|[^A-Za-z0-9_])${fn}[[:space:]]+[\"\$]"
        if printf '%s\n' "$body" | grep -Eq "$pat"; then
            calls="$calls $fn"
        fi
    done
    [ -n "$calls" ] || continue
    needs="$(sed -n '1,6s/^# needs:[[:space:]]*//p' "$f")"
    case " $needs " in
        *' helpers '*) ;;
        *) die "$f calls$calls (from the helpers module) but its '# needs:' line does not list 'helpers'" ;;
    esac
done
printf '  OK\n'

printf '\n=== GENERATOR-FAILS-CLOSED: a missing module aborts before any destination file is written ===\n'
# Regression cover for the bug where expand_includes ran on the LEFT of a
# pipeline: `set -eu` cannot see a pipeline's left-hand exit status, so a
# missing module made awk print to stderr and exit 1 while the pipeline's sed
# stage still ran to completion on the truncated input, wrote a decapitated
# bootstrap, and gen-bootstraps.sh exited 0. Proven here against a throwaway
# scratch tree (never the real repo) so it fires on every run, not just once.
SCRATCH="$(mktemp -d)"
mkdir -p "$SCRATCH/tools"
cp "$ROOT/tools/gen-bootstraps.sh" "$SCRATCH/tools/gen-bootstraps.sh"
{
    echo '#!/bin/sh'
    echo '@INCLUDE:does-not-exist@'
    echo 'echo hi'
} > "$SCRATCH/tools/bootstrap.template.sh"
scratch_log="$SCRATCH/gen.log"
if CLAWEE_PUBKEY_FILE="$ROOT/tools/testkeys/test.pub" \
   sh "$SCRATCH/tools/gen-bootstraps.sh" >"$scratch_log" 2>&1
then
    rm -rf "$SCRATCH"
    die "gen-bootstraps.sh exited 0 against a template that @INCLUDEs a nonexistent module — it must fail closed"
fi
if [ -e "$SCRATCH/clawee/install.sh" ]; then
    rm -rf "$SCRATCH"
    die "a missing module still left a file at clawee/install.sh — expand_includes' failure did not stop the write (see $scratch_log)"
fi
rm -rf "$SCRATCH"
printf '  OK\n'

printf '\n=== GENERATOR: committed bootstraps are what the generator writes ===\n'
gen_log="$(sh "$ROOT/tools/gen-bootstraps.sh" 2>&1)" || die "gen-bootstraps.sh failed:
$gen_log"
dirty="$(cd "$ROOT" && git status --porcelain -- $GENERATED_REL)"
[ -z "$dirty" ] || die "regenerating changed committed bootstraps — they are stale, commit the regeneration:
$dirty"
printf '  OK\n'

printf '\nALL OK — modules locked, dependencies ordered, bootstraps current\n'
