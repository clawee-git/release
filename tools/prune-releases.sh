#!/usr/bin/env bash
# prune-releases.sh — keep only the newest N releases per component on the
# Clawee release repo; delete the older GitHub Releases AND their git tags.
#
# Usage:
#   tools/prune-releases.sh            # DRY-RUN (default): list what would be deleted
#   tools/prune-releases.sh --execute  # actually delete
#
# Env (optional):
#   KEEP                    newest versions to retain per component (default 3 on
#                           stable, 1 on beta). tools/retain-permanent pins are
#                           kept in addition.
#   CHANNEL                 stable|beta (default stable)
#   COMPONENTS              space-separated set (default "clawee claweed")
#   CLAWEE_RELEASE_REPO     GitHub repo (default clawee-git/release)
#   CLAWEE_DOWNLOADS_BASE   R2 mirror base consulted before each delete (default
#                           https://downloads.clawee.org; set empty to skip the check)
#
# Per component it lists "<comp>/v*" release tags matching the selected
# CHANNEL's stamp shape, version-sorts them with `sort -V` (so v0.1.12 >
# v0.1.9), keeps the highest KEEP, and deletes the rest via
# `gh release delete --cleanup-tag` (removes the Release AND the tag). gh
# runs through ghp so the per-repo clawee-git token is used.
#
# Safety: a tag NOT mirrored to downloads.clawee.org is skipped (never deleted)
# — pruning it from GitHub would make CLAWEE_VERSION-pinned installs of that
# tag uninstallable (pre-R2 releases, or cuts whose mirror step failed).
# Exit code: non-zero if any delete failed, so automation can detect partial prunes.
set -euo pipefail
# The Clawee per-dir PATH hook strips /opt/homebrew/bin; re-add a sane PATH so
# grep/sort/sed/tr + gh/ghp resolve.
export PATH="/usr/bin:/bin:/opt/homebrew/bin:${HOME}/.claude/bin:${PATH}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="${CLAWEE_RELEASE_REPO:-clawee-git/release}"
CHANNEL="${CHANNEL:-stable}"
case "${CHANNEL}" in
  stable|beta) ;;
  *) echo "✗ CHANNEL must be stable or beta (got '${CHANNEL}')" >&2; exit 2 ;;
esac
# Beta is disposable — a cycle is opened, cut, promoted and superseded, and
# nothing on it is worth carrying history for.
if [ "${CHANNEL}" = beta ]; then
  KEEP="${KEEP:-1}"
else
  KEEP="${KEEP:-3}"
fi
COMPONENTS="${COMPONENTS:-clawee claweed}"
PERMANENT_FILE="${HERE}/retain-permanent"

is_permanent() {
  local tag="$1" stamp="${1##*/}" line
  [ -r "${PERMANENT_FILE}" ] || return 1
  while IFS= read -r line || [ -n "${line}" ]; do
    line="${line#"${line%%[![:space:]]*}"}"
    line="${line%"${line##*[![:space:]]}"}"
    case "${line}" in
      ''|'#'*) continue ;;
    esac
    if [ "${line}" = "${tag}" ] || [ "${line}" = "${stamp}" ]; then
      return 0
    fi
  done < "${PERMANENT_FILE}"
  return 1
}

# R2 mirror base — before deleting a release, its tag must still be served
# here or the delete is skipped (see the Safety note above). Empty disables.
DOWNLOADS_BASE="${CLAWEE_DOWNLOADS_BASE-https://downloads.clawee.org}"

EXECUTE=0
for a in "$@"; do
  case "$a" in
    --execute|--yes) EXECUTE=1 ;;
    -h|--help) awk 'NR==1{next} !/^#/{exit} {sub(/^# ?/,""); print}' "$0"; exit 0 ;;
    *) echo "✗ unknown argument: $a" >&2; exit 2 ;;
  esac
done

GHP="$(command -v ghp || echo "${HOME}/bin/ghp")"
[ -x "$GHP" ] || { echo "✗ ghp not found at ${GHP}" >&2; exit 1; }

mode="DRY-RUN"; [ "$EXECUTE" = 1 ] && mode="EXECUTE"
echo "repo=${REPO}  channel=${CHANNEL}  keep=${KEEP}  components=[${COMPONENTS}]  mode=${mode}"
echo

# One API pass; --paginate walks every page so nothing is missed past page 1.
tags="$("$GHP" api "repos/${REPO}/releases" --paginate --jq '.[].tag_name')"

planned=0
failed=0
for comp in ${COMPONENTS}; do
  # Anchored per channel. A tag matching NEITHER shape belongs to neither
  # channel: it is ignored here exactly as it is everywhere else, rather than
  # being swept into whichever count happens to accept a bare "<comp>/v" prefix.
  if [ "${CHANNEL}" = beta ]; then
    pattern="^${comp}/v[0-9]+\.[0-9]+\.[0-9]+\.beta\.[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9a-f]{8}\$"
  else
    pattern="^${comp}/v[0-9]+\.[0-9]+\.[0-9]+\.[0-9]{4}\.[0-9]{2}\.[0-9]{2}\.[0-9a-f]{8}\$"
  fi
  sorted="$(printf '%s\n' "${tags}" | grep -E "${pattern}" | sort -V || true)"
  if [ -z "${sorted}" ]; then
    echo "[${comp}] no releases"
    continue
  fi
  n="$(printf '%s\n' "${sorted}" | grep -c . || true)"
  if [ "${n}" -le "${KEEP}" ]; then
    echo "[${comp}] ${n} release(s) ≤ keep=${KEEP} — nothing to prune"
    continue
  fi
  drop="$(( n - KEEP ))"
  echo "[${comp}] ${n} releases → keep newest ${KEEP}, remove ${drop}"
  echo "  keep:   $(printf '%s\n' "${sorted}" | tail -n "${KEEP}" | tr '\n' ' ')"
  # Process substitution (not a pipe) so the loop body runs in THIS shell and
  # the failed/planned counters survive the loop.
  while IFS= read -r tag; do
    [ -n "${tag}" ] || continue
    # Never delete a release the R2 mirror doesn't serve: GitHub is the only
    # remaining source for it, and pinned installs of the tag would break.
    if is_permanent "${tag}"; then
      echo "  keep permanent ${tag}"
      continue
    fi
    if [ -n "${DOWNLOADS_BASE}" ]; then
      stamp="${tag#"${comp}"/}"
      sums_url="${DOWNLOADS_BASE}/${comp}/${stamp}/SHA256SUMS.txt"
      if [ "${CHANNEL}" = beta ]; then
        sums_url="${DOWNLOADS_BASE}/${comp}/beta/${stamp}/SHA256SUMS.txt"
      fi
      if ! curl -fsSL --max-time 15 -o /dev/null "${sums_url}" 2>/dev/null; then
        echo "  ! skip ${tag} — not on ${DOWNLOADS_BASE} (deleting would break pinned installs; mirror it first)"
        continue
      fi
    fi
    if [ "${EXECUTE}" = 1 ]; then
      if "$GHP" release delete "${tag}" -R "${REPO}" --yes --cleanup-tag >/dev/null 2>&1; then
        echo "  ✓ deleted ${tag}"
      else
        echo "  ✗ FAILED to delete ${tag}" >&2
        failed=1
      fi
    else
      echo "  - would delete ${tag}"
    fi
    planned="$(( planned + 1 ))"
  done < <(printf '%s\n' "${sorted}" | head -n "${drop}")
done

echo
if [ "${EXECUTE}" = 1 ]; then
  if [ "${failed}" -ne 0 ]; then
    echo "✗ done WITH FAILURES — some releases could not be deleted (see ✗ lines above)." >&2
    exit 1
  fi
  echo "✓ done — removed up to ${planned} release(s); kept newest ${KEEP} per component."
else
  echo "DRY-RUN: ${planned} release(s) would be removed. Re-run with --execute to apply."
fi
