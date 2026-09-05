#!/usr/bin/env bash
# version.test.sh — the two channels keep two lines, and opening a cycle is a
# one-way operator act.
#
# Runs against a THROWAWAY copy of the repo layout, never this checkout: every
# path under test writes a version file and `git add`s it, and a test that did
# that here would leave a staged version bump behind.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

# A fake kit: the script, a versions/ dir, and a git repo so `git add` works.
KIT="${WORK}/kit"
mkdir -p "${KIT}/tools" "${KIT}/versions"
cp "${REPO_ROOT}/tools/version.sh" "${KIT}/tools/version.sh"
printf '0.2.28\n' > "${KIT}/versions/clawee"
git -C "${KIT}" init -q -b main
git -C "${KIT}" -c user.email=t@t -c user.name=t add -A
git -C "${KIT}" -c user.email=t@t -c user.name=t commit -q -m init

# A fake component source worktree — --stamp reads its HEAD and nothing else.
SRC="${WORK}/src"; mkdir -p "${SRC}"
git -C "${SRC}" init -q -b main
git -C "${SRC}" -c user.email=t@t -c user.name=t commit -q --allow-empty -m init
SHA="$(git -C "${SRC}" rev-parse --short=8 HEAD)"
TODAY="$(date -u +%Y.%m.%d)"

v() { SRC_DIR="${SRC}" bash "${KIT}/tools/version.sh" "$@"; }

# ---- a closed cycle refuses, and says whose job opening one is --------------
say "the beta channel refuses while no cycle is open"
set +e
out="$(CHANNEL=beta v clawee --semver 2>&1)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "a beta read succeeded with no .beta.stamp: ${out}"
echo "${out}" | grep -q -- '--seed-beta' || die "the refusal does not name the way out:
${out}"
[ -e "${KIT}/versions/clawee.beta.stamp" ] && die "a refused read created the marker file"
printf '  ✓ refused, named --seed-beta, created nothing\n'

# ---- seeding opens it -------------------------------------------------------
say "--seed-beta opens the cycle"
out="$(v clawee --seed-beta 0.3.0 2>&1)" || die "seeding failed: ${out}"
[ "$(cat "${KIT}/versions/clawee.beta.stamp")" = "0.3.0" ] \
    || die "the marker holds $(cat "${KIT}/versions/clawee.beta.stamp"), want 0.3.0"
git -C "${KIT}" diff --cached --name-only | grep -qx 'versions/clawee.beta.stamp' \
    || die "the seeded file was not staged"
printf '  ✓ versions/clawee.beta.stamp = 0.3.0, staged\n'

# ---- and refuses to re-open --------------------------------------------------
# Re-seeding replaces the line a cycle has already climbed to, and the next beta
# cut re-mints a stamp that is already published.
say "--seed-beta refuses to overwrite an open cycle"
printf '0.3.2\n' > "${KIT}/versions/clawee.beta.stamp"
set +e
out="$(v clawee --seed-beta 0.3.0 2>&1)"; rc=$?
set -e
[ "${rc}" -ne 0 ] || die "re-seeding an open cycle succeeded"
[ "$(cat "${KIT}/versions/clawee.beta.stamp")" = "0.3.2" ] \
    || die "the refused re-seed still overwrote the file"
echo "${out}" | grep -qi 'already' || die "the refusal does not say why:
${out}"
printf '  ✓ refused, file untouched at 0.3.2\n'

say "--seed-beta insists on a .0 patch"
rm -f "${KIT}/versions/clawee.beta.stamp"
set +e
out="$(v clawee --seed-beta 0.3.4 2>&1)"; rc=$?
set -e
[ "${rc}" -eq 2 ] || die "--seed-beta 0.3.4 should be a usage error (got ${rc})"
[ -e "${KIT}/versions/clawee.beta.stamp" ] && die "a refused seed created the file"
printf '  ✓ exit 2, nothing written\n'
printf '0.3.0\n' > "${KIT}/versions/clawee.beta.stamp"

# ---- the stamp shapes -------------------------------------------------------
say "the beta stamp carries .beta. and the stable one does not"
stable="$(v clawee --stamp)"
beta="$(CHANNEL=beta v clawee --stamp)"
[ "${stable}" = "v0.2.28.${TODAY}.${SHA}" ] || die "stable stamp is ${stable}"
[ "${beta}" = "v0.3.0.beta.${TODAY}.${SHA}" ] || die "beta stamp is ${beta}"
# Every reader tells the channels apart by this segment; a stable stamp that
# contained it would be installed as a beta.
case "${stable}" in *.beta.*) die "the STABLE stamp contains .beta.: ${stable}" ;; esac
printf '  ✓ %s / %s\n' "${stable}" "${beta}"

# ---- the two lines move independently ---------------------------------------
# This is the whole reason for a second file: a cycle's beta cuts climb their
# own patch while stable stays where it is.
say "a beta bump does not move the stable line"
before="$(cat "${KIT}/versions/clawee")"
CHANNEL=beta v clawee --bump-patch >/dev/null
[ "$(cat "${KIT}/versions/clawee.beta.stamp")" = "0.3.1" ] \
    || die "the beta line did not climb: $(cat "${KIT}/versions/clawee.beta.stamp")"
[ "$(cat "${KIT}/versions/clawee")" = "${before}" ] \
    || die "a beta bump moved the STABLE line to $(cat "${KIT}/versions/clawee")"
printf '  ✓ beta 0.3.0 → 0.3.1, stable still %s\n' "${before}"

say "a stable bump does not move the beta line"
CHANNEL=stable v clawee --bump-patch >/dev/null
[ "$(cat "${KIT}/versions/clawee")" = "0.2.29" ] || die "the stable line did not climb"
[ "$(cat "${KIT}/versions/clawee.beta.stamp")" = "0.3.1" ] \
    || die "a stable bump moved the BETA line"
printf '  ✓ stable 0.2.28 → 0.2.29, beta still 0.3.1\n'

say "an unknown CHANNEL is refused"
set +e
CHANNEL=nightly v clawee --semver >/dev/null 2>&1; rc=$?
set -e
[ "${rc}" -eq 2 ] || die "CHANNEL=nightly should be a usage error (got ${rc})"
printf '  ✓ exit 2\n'

printf '\n✓ version.test.sh passed\n'
