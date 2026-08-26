#!/usr/bin/env bash
# module_gate — release.sh's pre-cut gate over the outer-bootstrap trust chain.
#
# The bootstrap-modules work added six checks over the chain a user's install
# actually walks (resolve → minisign → sha256 → unzip → exec inner). All six
# passed from the day they landed and NOTHING RAN THEM: no CI, no release.sh
# hook. That is not a hypothetical failure mode — before the modules work, main
# was already shipping generated bootstraps its own template would not
# reproduce, and a drift check that would have caught it already existed
# (tools/test-tag-binding.sh, DRIFT section). It did not fail because nobody ran
# it. This file is where "nobody ran it" stops being possible for a cut.
#
# Called from release.sh's pre-flight, beside vulncheck_gate and for the same
# reason: it must abort BEFORE the first artifact is produced, so a tree whose
# trust chain is not what it claims never mints a binary, a tag or a Release.
#
# WHICH SUITES ARE WIRED, AND WHY NOT THE OTHERS
#
#   tools/test-modules.sh          the five module gates: LOCK, DEPS-ordering,
#                                  DEPS-printer, INCLUDED, GENERATOR-FAILS-CLOSED,
#                                  GENERATOR
#   tools/test-checksum-verify.sh  the checksum gate over the verify step itself
#   tools/sync-modules.test.sh     sync-modules.sh's own behaviour, INCLUDING that
#                                  it refuses to overwrite a LOCAL FORK
#
# The set is the same three in all three release repos, on purpose: the modules
# are shared, so a gate that differs per repo is a gate you cannot reason about
# from one place. Burrowee additionally excludes test-r2-fallback.sh,
# test-e2e-relay.sh and test-upgrade-bootstrap.sh because they are red on ITS
# main; here the first two do not exist and test-upgrade-bootstrap.sh passes.
# That is not a reason to wire it — widening the set in one repo is how the
# three drift apart. If it should be gated, gate it everywhere in one change.
#
# Deliberately NOT wired: tools/sync-modules.sh ITSELF. It is the cross-product
# sync tool, and it exits non-zero on a LOCAL FORK verdict BY DESIGN — a fork is
# a state a repo is allowed to sit in, not a defect in the tree being cut.
# Gating a cut on it would refuse releases from a repo that has deliberately
# diverged from the shared module set. We gate on its TEST (that the tool
# behaves), never on a live sync run. If you are here because you were about to
# add it: that is the reason not to.
#
# Deliberately NOT wired: tools/test-e2e.sh. It regenerates the shipped
# bootstraps with the TEST pubkey and does not restore them, which would leave
# test-keyed installers in the tree being cut.

# module_gate — run the green gate set against the tree being cut. Aborts the
# cut on the first failure.
#
# Runs against ${REPO_ROOT} itself, not a copy: the point is to gate the bytes
# that are about to be signed and published, and a copy is only ever an argument
# about whether it is faithful.
#
# ON A RED GENERATOR GATE THE TREE IS LEFT DIRTY, ON PURPOSE. test-modules.sh's
# GENERATOR check regenerates in place and then diffs; when the committed
# bootstraps are stale it leaves the regenerated ones sitting in the working
# tree. That is the fix, already applied — `git diff` shows exactly what drifted,
# and committing it is the whole remedy. Restoring them here would delete the
# answer and leave the operator to rediscover it. The cut has already aborted at
# that point, and release.sh's clean-tree pre-flight ran earlier in this same
# invocation, so nothing downstream consumes the dirty tree; the next cut's
# pre-flight is what refuses, correctly, until the regeneration is committed.
module_gate() {
    local suite rc log
    for suite in test-modules.sh test-checksum-verify.sh sync-modules.test.sh; do
        [ -f "${REPO_ROOT}/tools/${suite}" ] \
            || { echo "✗ module gate: ${suite} is missing from tools/" >&2; exit 1; }
        echo "→ module gate: ${suite}" >&2
        rc=0
        log="$(bash "${REPO_ROOT}/tools/${suite}" 2>&1)" || rc=$?
        if [ "${rc}" != 0 ]; then
            echo "${log}" >&2
            echo "✗ module gate: ${suite} failed (exit ${rc}) — release aborted" >&2
            if [ "${suite}" = test-modules.sh ]; then
                echo "  If this was the GENERATOR check: the regenerated bootstraps are in your" >&2
                echo "  working tree now. Run 'git diff' to see the drift, commit it, then re-cut." >&2
            fi
            exit 1
        fi
        echo "✓ module gate: ${suite} clean" >&2
    done
}
