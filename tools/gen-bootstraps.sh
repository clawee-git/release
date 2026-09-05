#!/bin/sh
# gen-bootstraps.sh — generate the self-contained outer bootstraps from one
# template: for each of clawee and claweed, and for each channel,
#
#   <comp>/install.sh        <comp>/upgrade.sh          (stable)
#   <comp>/beta.install.sh   <comp>/beta.upgrade.sh     (beta)
#
# BESIDE the stable name, never under a beta/ directory: <comp>/beta/ is where
# the channel MANIFEST lives on the downloads mirror, and giving the same
# segment two meanings on two hosts is how a fetch ends up one directory from
# the file it wanted.
#
# install.sh and upgrade.sh are ONE template under a @MODE@ substitution, not
# two files: the baked pubkey and the minisign gate are what make it the trust
# anchor, and a second copy of those is a copy that drifts. BOTH MODES FOR
# EVERY COMPONENT, not only claweed (the one whose kit ships a migration
# ladder today) — which kits carry migrations/ is decided in the COMPONENT
# repos at their build, and a conditional render here would put a belief about
# that in this repo that nothing keeps in step with the zips. A kit with no
# ladder is instead a runtime refusal from the shipped bootstrap, naming the
# component and the version it just installed.
#
# Each generated file is byte-identical to its siblings except for the @COMP@,
# @MODE@, @CHANNEL@ and @PUBKEY@ substitutions. The beta twins are rendered
# UNCONDITIONALLY, for the same reason both modes are: whether a beta exists is
# what its manifest answers at install time on the host doing the installing,
# and a render-time belief about it here would be a second answer nothing keeps
# in step. A twin whose channel serves nothing refuses at runtime, naming it. The outer bootstrap is THE TRUST ANCHOR,
# so the baked @PUBKEY@ must be the real release signing pubkey before
# activation.
#
# Pubkey resolution (first that exists wins):
#   1. $CLAWEE_PUBKEY_FILE   (explicit override; used by the offline E2E test)
#   2. clawee-release.pub    (the REAL release signing pubkey — Phase B5/activation)
#   3. tools/testkeys/test.pub (the local TEST key)
#   4. none -> a clearly-marked TEMP placeholder is baked in, and the generated
#      bootstraps WILL refuse to run (the runtime guards on *TEMP*). Regenerate
#      once a real key exists.
#
# The @PUBKEY@ value is the base64 key line of a minisign .pub file (the last
# non-comment line) — exactly what `minisign -V -P <pubkey>` expects inline.
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
TEMPLATE="$ROOT/tools/bootstrap.template.sh"
[ -f "$TEMPLATE" ] || { echo "✗ missing template: $TEMPLATE" >&2; exit 1; }

MODDIR="$ROOT/tools/modules"

# expand_includes <template> — write <template> to stdout with every line that is
# exactly `@INCLUDE:<name>@` replaced by tools/modules/<name>.sh, wrapped in
# `# BEGIN <name>` / `# END <name>` markers and with the module's own header
# lines dropped. Runs BEFORE the sed substitution pass, so a module may contain
# @COMP@ / @MODE@ / @BRAND@ / @brand@ like any other template text.
#
# The bootstrap is the trust anchor: it is delivered as `curl … | sh` and fetches
# no code. Modules are therefore spliced HERE, at generation time, and never
# sourced at runtime.
#
# The emitted `# BEGIN <name>` / `# END <name>` markers are LOAD-BEARING: one
# or more tools/test-*.sh scripts (e.g. tools/test-checksum-verify.sh) extract
# a module's spliced block out of a GENERATED bootstrap by matching these exact
# marker names verbatim. Renaming a module (and therefore its markers) without
# first grepping tools/test-*.sh for the old name will silently break that
# extraction.
expand_includes() {
    awk -v moddir="$MODDIR" '
        /^@INCLUDE:[a-z0-9-]+@$/ {
            name = substr($0, 10, length($0) - 9 - 1)
            path = moddir "/" name ".sh"
            if ((getline probe < path) < 0) {
                printf("✗ @INCLUDE:%s@ but %s does not exist\n", name, path) > "/dev/stderr"
                exit 1
            }
            close(path)
            printf("# BEGIN %s\n", name)
            while ((getline line < path) > 0) {
                if (line ~ /^# (module|needs|since):/) continue
                print line
            }
            close(path)
            printf("# END %s\n", name)
            next
        }
        { print }
    ' "$1"
}

# ---- resolve the pubkey -------------------------------------------------
pubfile=""
for cand in "${CLAWEE_PUBKEY_FILE:-}" "$ROOT/clawee-release.pub" "$ROOT/tools/testkeys/test.pub"; do
    [ -n "$cand" ] || continue
    if [ -f "$cand" ]; then pubfile="$cand"; break; fi
done

if [ -n "$pubfile" ]; then
    # last non-empty, non-comment line = the base64 key line
    PUBKEY="$(grep -v '^untrusted comment:' "$pubfile" | grep -v '^[[:space:]]*$' | tail -n1)"
    [ -n "$PUBKEY" ] || { echo "✗ could not extract a pubkey line from $pubfile" >&2; exit 1; }
    echo "→ baking pubkey from: $pubfile"
else
    # No key file anywhere yet. Bake a TEMP placeholder — the runtime guard in
    # the template aborts on *TEMP* so these can never silently install.
    PUBKEY="RWTEMP_PLACEHOLDER_REGENERATE_AFTER_B5_OR_ACTIVATION_xxxxxxxxxxxx"
    echo "! no pubkey file found (clawee-release.pub / tools/testkeys/test.pub)" >&2
    echo "! baking a TEMP placeholder — generated bootstraps will REFUSE to run." >&2
    echo "! create the key (B5: minisign -G ... or activation) and re-run." >&2
fi

# ---- generate -----------------------------------------------------------
for comp in clawee claweed; do
    mkdir -p "$ROOT/$comp"
    # @COMP@, @MODE@, @CHANNEL@ and @PUBKEY@ — order doesn't matter, none of
    # the four values contains another's placeholder. Use a tmp then move so a partial
    # write can't ship.
    for spec in stable:install stable:upgrade beta:install beta:upgrade; do
        channel="${spec%%:*}"
        mode="${spec#*:}"
        # stable keeps the bare name — it is the one in every README, every
        # install line and every agent's memory, and renaming it would break
        # every one of them for a directory tidy nobody asked for.
        if [ "$channel" = stable ]; then
            out="$ROOT/$comp/$mode.sh"
        else
            out="$ROOT/$comp/$channel.$mode.sh"
        fi
        tmp="$out.tmp.$$"
        exp="$out.exp.$$"
        # expand_includes runs OFF the left of the pipeline (its own redirection,
        # not a pipe) so `set -e` sees its exit status directly — a pipeline's
        # left-hand failure is otherwise invisible under plain `set -eu` and
        # would silently ship a bootstrap truncated at the include point,
        # losing the whole trust gate while exiting 0. The @INCLUDE: guard below
        # still runs against the tmp file BEFORE the mv, to catch the OTHER
        # failure shape: a malformed include name the awk regex declines to
        # match and so passes through literally.
        expand_includes "$TEMPLATE" > "$exp"
        sed -e "s|@COMP@|$comp|g" -e "s|@MODE@|$mode|g" -e "s|@CHANNEL@|$channel|g" \
            -e "s|@PUBKEY@|$PUBKEY|g" \
            -e "s|@BRAND@|CLAWEE|g" -e "s|@brand@|clawee|g" \
            "$exp" > "$tmp"
        rm -f "$exp"
        grep -q '@INCLUDE:' "$tmp" && { rm -f "$tmp"; echo "✗ unexpanded @INCLUDE in $out" >&2; exit 1; }
        chmod +x "$tmp"
        mv -f "$tmp" "$out"
        echo "✓ wrote $out  (channel $channel, mode $mode)"
    done
done
