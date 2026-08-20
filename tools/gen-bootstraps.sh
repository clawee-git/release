#!/bin/sh
# gen-bootstraps.sh — generate the self-contained outer bootstraps
# (<comp>/install.sh, <comp>/upgrade.sh, for comp in clawee claweed) from one
# template.
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
# Each generated file is byte-identical to its sibling except for the @COMP@,
# @MODE@ and @PUBKEY@ substitutions. The outer bootstrap is THE TRUST ANCHOR,
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
    # @COMP@, @MODE@ and @PUBKEY@ — order doesn't matter, none of the three
    # values contains another's placeholder. Use a tmp then move so a partial
    # write can't ship.
    for mode in install upgrade; do
        out="$ROOT/$comp/$mode.sh"
        tmp="$out.tmp.$$"
        sed -e "s|@COMP@|$comp|g" -e "s|@MODE@|$mode|g" -e "s|@PUBKEY@|$PUBKEY|g" "$TEMPLATE" > "$tmp"
        chmod +x "$tmp"
        mv -f "$tmp" "$out"
        echo "✓ wrote $out  (mode $mode)"
    done
done
