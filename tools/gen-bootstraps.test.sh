#!/usr/bin/env bash
# gen-bootstraps.test.sh — the four generated scripts per component differ ONLY
# by their two substitutions, and each resolves its own channel.
#
# WHAT THIS ADDS over test-bootstrap-resolve.sh, which runs the generated
# bootstraps against a fake mirror: that one proves the beta twin RESOLVES the
# beta manifest. This one proves the twin is the same file — that beta.install.sh
# is stable's install.sh with @CHANNEL@ and @MODE@ changed and nothing else, so a
# fix to the trust gate cannot land on one channel and miss the other. The
# generator is one template for exactly that reason, and a diff is the only
# thing that keeps it honest.
#
# It also pins that the committed bootstraps are what the generator writes, and
# restores them either way — a test that regenerated into the checkout and left
# the result behind would turn a stale generator into a passing test plus a
# dirty tree.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

# ---- the committed files are what the generator writes ----------------------
say "the committed bootstraps are current"
before="$(git -C "${REPO_ROOT}" status --porcelain clawee claweed)"
[ -z "${before}" ] || die "clawee/ or claweed/ is already dirty — cannot tell a stale generator from local edits:
${before}"
bash "${REPO_ROOT}/tools/gen-bootstraps.sh" >/dev/null
after="$(git -C "${REPO_ROOT}" status --porcelain clawee claweed)"
if [ -n "${after}" ]; then
    git -C "${REPO_ROOT}" checkout -- clawee claweed
    die "regenerating changed the committed bootstraps — run tools/gen-bootstraps.sh and commit:
${after}"
fi
printf '  ✓ regeneration is a no-op\n'

# ---- all four exist, for both components ------------------------------------
# The twins are rendered UNCONDITIONALLY (see gen-bootstraps.sh's header):
# whether a beta exists is what its manifest answers on the host doing the
# installing, and a render-time belief about it here would be a second answer
# nothing keeps in step.
say "every component has all four scripts"
for comp in clawee claweed; do
    for f in install.sh upgrade.sh beta.install.sh beta.upgrade.sh; do
        [ -x "${REPO_ROOT}/${comp}/${f}" ] || die "${comp}/${f} missing or not executable"
    done
done
printf '  ✓ install / upgrade / beta.install / beta.upgrade × 2\n'

# ---- the twin is the stable file with two values changed --------------------
say "the twin differs from stable only in CHANNEL and MODE"
for comp in clawee claweed; do
    for mode in install upgrade; do
        s="${REPO_ROOT}/${comp}/${mode}.sh"
        b="${REPO_ROOT}/${comp}/beta.${mode}.sh"
        # BOTH channel words are collapsed to one token in BOTH files, then the
        # files must be identical. Normalising only the CHANNEL= line is not
        # enough: @CHANNEL@ is substituted into the header comments too, and the
        # header names the other channel as well, so no line-targeted rule
        # separates "the substitution" from "the word".
        #
        # What this deliberately cannot see is a twin that baked the WRONG
        # channel — both collapse to the same token. That is the next check's
        # job, and it is a direct assertion rather than a diff.
        collapse() { sed -e 's/stable/@C@/g' -e 's/beta/@C@/g' "$1"; }
        diff <(collapse "${b}") <(collapse "${s}") > /dev/null \
            || die "${comp}/beta.${mode}.sh and ${comp}/${mode}.sh differ by more than the channel:
$(diff <(collapse "${b}") <(collapse "${s}") | head -20)"
    done
done
printf '  ✓ one template, two renderings\n'

# ---- each names its own channel, and only its own ---------------------------
# A twin that baked CHANNEL="stable" would resolve the stable manifest under a
# beta name — the host would be moved onto stable silently, which is the exact
# failure release-management.md §9 forbids (a host never changes channel).
say "each script bakes its own channel"
for comp in clawee claweed; do
    grep -qx 'CHANNEL="stable"' "${REPO_ROOT}/${comp}/install.sh" \
        || die "${comp}/install.sh does not bake CHANNEL=stable"
    grep -qx 'CHANNEL="beta"' "${REPO_ROOT}/${comp}/beta.install.sh" \
        || die "${comp}/beta.install.sh does not bake CHANNEL=beta"
    # And nothing may re-decide it at runtime: an env var that could flip the
    # channel is a host whose channel nobody can state.
    ! grep -qE 'CHANNEL="\$\{|CHANNEL=\$' "${REPO_ROOT}/${comp}/beta.install.sh" \
        || die "${comp}/beta.install.sh lets the environment set the channel"
done
printf '  ✓ baked, and not overridable\n'

# ---- no unexpanded placeholder survives -------------------------------------
say "nothing ships a placeholder"
for comp in clawee claweed; do
    for f in install.sh upgrade.sh beta.install.sh beta.upgrade.sh; do
        if grep -nE '@(COMP|MODE|CHANNEL|PUBKEY|BRAND|brand|INCLUDE:[a-z0-9-]+)@' "${REPO_ROOT}/${comp}/${f}" >/dev/null; then
            grep -nE '@(COMP|MODE|CHANNEL|PUBKEY|BRAND|brand|INCLUDE:[a-z0-9-]+)@' "${REPO_ROOT}/${comp}/${f}" >&2
            die "${comp}/${f} carries an unsubstituted placeholder"
        fi
    done
done
printf '  ✓ clean\n'

printf '\n✓ gen-bootstraps.test.sh passed\n'
