#!/bin/bash
# release.command — run this repo's release cut in a DESKTOP session.
#
# Not a release step. It launches tools/release.sh unmodified; every decision
# about what a cut does still lives there. This exists for one reason:
#
#   Signing and notarizing are different capabilities. rcodesign is pure
#   userspace and signs in any session. notarytool reaches Apple through
#   CFNetwork/AppSSO, which needs a per-user bootstrap namespace — in a
#   background/daemon-hosted shell it does not crash politely, it SIGTRAPs with
#   no submission id, and release.sh can only report `status: unknown`. That
#   reads like a vendor outage and is not one.
#
# LaunchServices opens a .command in the desktop's own terminal, which IS such a
# session — no Apple Events, no TCC prompt, no sudo. Hence the extension: this
# file must be openable, not merely executable.
#
#   open tools/release.command        # committed 100755; no chmod needed
#
# Inputs live OUTSIDE this repo or are ignored by it. This file carries flow and
# the names of its own inputs — never a host, credential, absolute machine path,
# or component inventory:
#
#   ~/.agents/local/release.env  machine facts: PATH to the toolchain, signing and
#                             notarization backends, non-interactive flags.
#                             Override with RELEASE_ENV.
#   .release-request          what to cut, written per run. Override with
#                             RELEASE_REQUEST. Sourced as shell. Shape:
#                                 COMPONENTS="clawee"
#                                 FLAGS="--public"
#
# Output: .release.log, ending in RELEASE-EXIT:<code> so a watcher can block on it
# rather than guess when the run finished. Exactly one run per log — the previous
# run is rotated to .release.log.prev, so a refusal never destroys the record of
# the last real cut. Override the log with RELEASE_LOG.
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)" || exit 1
cd "$REPO_ROOT" || exit 1

# The rest of tools/ calls git by absolute path: the per-directory PATH hook on
# this tree strips Homebrew, and release.env rewrites PATH further down. A guard
# that silently loses its git is a guard that passes.
GIT=/usr/bin/git

LOG="${RELEASE_LOG:-$REPO_ROOT/.release.log}"
[ -e "$LOG" ] && mv -f "$LOG" "${LOG}.prev" 2>/dev/null
if ! : > "$LOG"; then
    echo "✗ cannot write log: $LOG" >&2
    exit 1
fi

say() { echo "$@" | tee -a "$LOG"; }
die() { say "✗ $*"; exit 1; }

# ONE emitter for the sentinel. Hand-written sentinels covered only the paths
# someone remembered: closing the Terminal window (SIGHUP — the expected way an
# operator abandons a .command), Ctrl-C, and `set -u` tripping inside a sourced
# file all left a watcher blocked forever.
LOCK=""
on_exit() {
    rc=$?
    trap - EXIT
    [ -n "$LOCK" ] && rmdir "$LOCK" 2>/dev/null
    say "RELEASE-EXIT:${rc}"
    exit "$rc"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
trap 'exit 129' HUP

# 1. Session. Checked FIRST and refused loudly: the whole point of this file is
#    that the wrong session builds and signs for minutes before dying at notarize.
#
#    managername alone is necessary, not sufficient. A daemon shell that re-execs
#    through `launchctl asuser <uid>` lands in the user's GUI domain and reports
#    Aqua while still lacking the console security session AppSSO needs, and a
#    sudo'd run inherits Aqua but notarizes against root's keychain. Each
#    condition refuses separately so the operator learns which one it was.
DOMAIN="$(launchctl managername 2>/dev/null || echo unknown)"
say "session-domain: ${DOMAIN}"
[ "${DOMAIN}" = "Aqua" ] || die "not a desktop session (need Aqua, got ${DOMAIN}) — 'open' this file, do not run it from a shell"
[ "$(id -u)" -ne 0 ] || die "running as root — notarization would use root's keychain; open this file as your own user"
[ -z "${SSH_CONNECTION:-}" ] || die "this is an SSH session — it has no console security session; open this file on the desktop"
[ -t 0 ] || die "stdin is not a terminal — this was not opened by LaunchServices"

# 2. One release at a time. Two `open`s (an agent racing an operator, or a
#    double-click) would otherwise interleave into one log and race each other's
#    marker commits and pushes.
LOCK_DIR="$REPO_ROOT/.release.lock"
mkdir "$LOCK_DIR" 2>/dev/null || die "a release is already running (lock: $LOCK_DIR) — remove it only if no cut is live"
LOCK="$LOCK_DIR"

# 3. Environment. Loaded, never embedded. Restore IFS afterwards: everything
#    below splits COMPONENTS and FLAGS on whitespace, and a sourced file that
#    leaves IFS changed would silently re-split them.
ENV_FILE="${RELEASE_ENV:-$HOME/.agents/local/release.env}"
[ -r "${ENV_FILE}" ] || die "env file not readable: ${ENV_FILE}"
# shellcheck source=/dev/null
. "${ENV_FILE}"
IFS=$' \t\n'
say "env: ${ENV_FILE}"

# 4. Request.
REQUEST="${RELEASE_REQUEST:-$REPO_ROOT/.release-request}"
[ -r "${REQUEST}" ] || die "request file not readable: ${REQUEST}"
COMPONENTS=""; FLAGS=""
# shellcheck source=/dev/null
. "${REQUEST}"
IFS=$' \t\n'
[ -n "${COMPONENTS}" ] || die "request names no COMPONENTS: ${REQUEST}"

# `all` is a real release.sh argument, and it defeats this file: it cuts every
# component inside ONE process with no pushes between, then HEAD reads
# [RELEASED: <last>] and the marker test below can never match `all`. The run
# would report success sitting on two unpushed markers — the exact wedge this
# file exists to prevent. Reject it here rather than discover it afterwards.
set -f   # COMPONENTS/FLAGS are split on whitespace below; they must not glob
for comp in ${COMPONENTS}; do
    case "${comp}" in
        clawee|claweed) ;;
        all) die "COMPONENTS=\"all\" is not usable here: it cuts every component in one process with no push between, and leaves markers unpushed. List them instead: COMPONENTS=\"clawee claweed\"" ;;
        *)   die "unknown component: ${comp} (expected clawee or claweed)" ;;
    esac
done
say "request: ${COMPONENTS} [${FLAGS}]"

# A dry run makes no marker commit, so HEAD is still the PREVIOUS marker and the
# subject test below would match it and push whatever is unpushed. Skip the push
# path entirely rather than rely on the marker test to notice.
DRY=0
case " ${FLAGS} " in *" --dry-run "*) DRY=1 ;; esac
[ "$DRY" -eq 0 ] || say "note: --dry-run — components will be cut, no marker will be pushed"

# tree_state — echoes porcelain output, non-zero if git itself failed.
#
# A bare `[ -n "$(git status --porcelain)" ]` fails OPEN: git's errors go to
# stderr and stdout is left empty, so a missing git, a held index.lock or an
# unreadable object store all read as "tree is clean" and the push proceeds.
# --untracked-files=all because a repo-local status.showUntrackedFiles=no would
# otherwise retire the untracked half of the check.
tree_state() { $GIT status --porcelain --untracked-files=all; }

# unpushed_count — commits on HEAD that origin/main does not have. Fetches
# first: nothing else re-verifies in-sync at the moment of the push, and a
# multi-minute cut is long enough for the remote to have moved.
unpushed_count() {
    $GIT fetch --quiet origin main || return 1
    $GIT rev-list --count FETCH_HEAD..HEAD
}

# push_marker <comp> — publish exactly the marker this component just wrote.
#
# `git push origin HEAD` would publish HEAD's whole unpushed ancestry to whatever
# branch HEAD is on. Assert branch, attachment and ahead-count here, at the
# moment of the push, and name the destination explicitly.
push_marker() {
    local comp="$1" branch ahead
    branch="$($GIT symbolic-ref --quiet --short HEAD)" \
        || die "HEAD is detached — refusing to push"
    [ "${branch}" = "main" ] \
        || die "on branch '${branch}', not main — refusing to push"
    ahead="$(unpushed_count)" \
        || die "cannot reach origin to verify what would be pushed"
    [ "${ahead}" = "1" ] \
        || die "expected exactly 1 unpushed commit (the ${comp} marker), found ${ahead} — inspect before pushing"
    $GIT push origin HEAD:refs/heads/main 2>&1 | tee -a "$LOG"
    [ "${PIPESTATUS[0]}" -eq 0 ] || die "marker push failed for ${comp}"
    say "✓ ${comp} marker pushed"
}

# 5. Cut each component, pushing its marker before the next one starts.
#
#    release.sh publishes the release and then records a [RELEASED: <comp>]
#    marker commit — but it deliberately never pushes. Leave that marker sitting
#    while the next component cuts and the repo carries two unrecorded releases
#    at once, with the ahead-count assertion above no longer able to tell which
#    marker belongs to which cut. Pushing between components is what lets a batch
#    run unattended and keeps every published release recorded upstream at the
#    moment it is published.
for comp in ${COMPONENTS}; do
    say ""
    say "── cut: ${comp} ──"
    # shellcheck disable=SC2086
    bash tools/release.sh "${comp}" ${FLAGS} 2>&1 | tee -a "$LOG"
    rc="${PIPESTATUS[0]}"
    [ "${rc}" -eq 0 ] || { say "✗ ${comp} failed (exit ${rc}) — later components NOT cut"; say "   already-cut components above are PUBLISHED: drop them from COMPONENTS before re-running"; exit "${rc}"; }

    [ "$DRY" -eq 0 ] || { say "→ ${comp}: --dry-run, nothing to push"; continue; }

    state="$(tree_state)" || die "cannot read git status — refusing to push (is git reachable on PATH set by ${ENV_FILE}?)"
    [ -z "${state}" ] || die "${comp} cut left an unclean tree — refusing to push; inspect before continuing"

    subject="$($GIT log -1 --format=%s)" || die "cannot read HEAD subject — refusing to push"
    case "${subject}" in
        "[RELEASED: ${comp}]"*)
            push_marker "${comp}"
            ;;
        *)
            # One legitimate reason HEAD is not a marker: a re-cut at an
            # identical stamp produces a byte-identical tree, the marker commit
            # records nothing, and the repo is left IN SYNC. That is the only
            # shape allowed to pass silently. Anything unpushed here means the
            # cut published something it did not record.
            ahead="$(unpushed_count)" \
                || die "cannot reach origin to check for unpushed work after ${comp}"
            if [ "${ahead}" = "0" ]; then
                say "→ ${comp}: no marker and nothing unpushed — re-cut at an identical stamp; the marker for it is already in history"
            else
                die "${comp}: HEAD is not a [RELEASED: ${comp}] marker (got: ${subject}) yet ${ahead} commit(s) are unpushed — the cut published something it did not record; inspect before continuing"
            fi
            ;;
    esac
done

say ""
exit 0
