#!/usr/bin/env bash
# test-bootstrap-resolve.sh — VERSION RESOLUTION in the generated bootstraps.
#
# The bootstrap's one irreversible decision is WHICH release to install. Every
# other guarantee it makes — minisign, sha256 — only says the bytes it fetched
# are the bytes that release published; none of them says that release is the
# one this channel is serving. That question is answered here, and until this
# file existed nothing tested it: every other suite pins a version, which is
# precisely the path resolution does not take.
#
# The order under test (release-management.md §9):
#
#   1. the channel MANIFEST   <comp>/latest.json, <comp>/beta/latest.json
#   2. the GitHub tag list    only when the manifest host did not answer
#   3. the gh-proxy mirrors   only when neither did (not exercised here: they
#                             are third-party hosts and no test may reach one)
#
# The manifest is first because promote writes it LAST, as the go-live: a build
# with a manifest entry is one an operator approved. The tag list is neither
# channel-aware nor promote-aware, so it can only ever be a fallback.
#
# NOTHING here reaches the real GitHub or the real downloads host. Both are
# aimed at a local HTTP server through the two undocumented test hooks
# (CLAWEE_DOWNLOADS_BASE, CLAWEE_GH_API_BASE); "GitHub unreachable" is a closed
# port on loopback.
#
#     bash tools/test-bootstrap-resolve.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
W="$(mktemp -d)"
SERVER_PID=""
cleanup() { [ -n "${SERVER_PID}" ] && kill "${SERVER_PID}" 2>/dev/null || true; rm -rf "${W}"; }
trap cleanup EXIT

say()  { printf '\n=== %s ===\n' "$*"; }
die()  { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }
has()  { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }

STABLE_STAMP="v0.4.7.2026.09.05.aaaaaaaa"
BETA_STAMP="v0.5.0.beta.2026.09.05.bbbbbbbb"
# Deliberately NEWER than the stable manifest's stamp: if the tag list were
# consulted first, or the manifest ignored, this is the tag that would win — so
# the assertions below can tell "resolved from the manifest" from "resolved
# from GitHub and happened to agree".
GH_ONLY_STAMP="v0.9.9.2026.09.09.cccccccc"

# ---- fixtures ---------------------------------------------------------------
mkdir -p "${W}/srv/clawee/beta" "${W}/srv/repos/clawee-git/release"
cat > "${W}/srv/clawee/latest.json" <<JSON
{
  "component": "clawee",
  "path": "clawee/${STABLE_STAMP}",
  "stamp": "${STABLE_STAMP}",
  "version": "0.4.7"
}
JSON
write_beta_manifest() {
    cat > "${W}/srv/clawee/beta/latest.json" <<JSON
{
  "component": "clawee",
  "path": "clawee/beta/${BETA_STAMP}",
  "stamp": "${BETA_STAMP}",
  "version": "0.5.0"
}
JSON
}
write_beta_manifest
# The GitHub /releases stub. python3's http.server serves a directory, so the
# API path is a FILE at that path; the bootstrap appends a query string, which
# the server ignores.
cat > "${W}/srv/repos/clawee-git/release/releases" <<JSON
[
  {"tag_name": "clawee/${GH_ONLY_STAMP}"},
  {"tag_name": "clawee/v0.9.0.beta.2026.09.08.dddddddd"}
]
JSON

PORT="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
( cd "${W}/srv" && exec python3 -m http.server "${PORT}" --bind 127.0.0.1 ) >/dev/null 2>&1 &
SERVER_PID=$!
disown "${SERVER_PID}" 2>/dev/null || true
for _ in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:${PORT}/clawee/latest.json" -o /dev/null 2>/dev/null && break
    sleep 0.1
done
curl -fsS "http://127.0.0.1:${PORT}/clawee/latest.json" -o /dev/null 2>/dev/null \
    || die "the fixture http server did not come up on ${PORT}"
BASE="http://127.0.0.1:${PORT}"

# A port nothing listens on: this is what "unreachable" means here.
DEAD_PORT="$(python3 - <<'PY'
import socket
s = socket.socket(); s.bind(("127.0.0.1", 0)); print(s.getsockname()[1]); s.close()
PY
)"
DEAD="http://127.0.0.1:${DEAD_PORT}"

# run_resolve <script> <downloads-base> <gh-api-base> — runs a generated
# bootstrap far enough to resolve a version and then fail at the download (the
# fixture serves no zips), returning its combined output. The exit status is
# EXPECTED to be non-zero; what is under test is which tag it printed.
run_resolve() {
    local script="$1" downloads="$2" ghapi="$3"
    set +e
    CLAWEE_DOWNLOADS_BASE="${downloads}" \
        CLAWEE_GH_API_BASE="${ghapi}" \
        CLAWEE_GH_PROXY= \
        PREFIX="${W}/prefix" \
        HOME="${W}/home" \
        sh "${script}" 2>&1
    set -e
}

# ---- (1) the manifest is FIRST, even when GitHub answers --------------------
say "STABLE: the channel manifest wins over a newer tag on the release list"
out="$(run_resolve "${REPO_ROOT}/clawee/install.sh" "${BASE}" "${BASE}")"
has "manifest: clawee/${STABLE_STAMP}" "${out}" \
    || die "the stable bootstrap did not resolve from the manifest:
${out}"
has "${GH_ONLY_STAMP}" "${out}" \
    && die "the stable bootstrap resolved the GitHub tag although the manifest answered — the tag list is a FALLBACK, and it is neither channel-aware nor promote-aware:
${out}"
printf '  OK: resolved %s from <comp>/latest.json\n' "${STABLE_STAMP}"

say "BETA: the twin reads <comp>/beta/latest.json, not the stable one"
out="$(run_resolve "${REPO_ROOT}/clawee/beta.install.sh" "${BASE}" "${BASE}")"
has "manifest: clawee/${BETA_STAMP}" "${out}" \
    || die "the beta twin did not resolve from the beta manifest:
${out}"
has "${STABLE_STAMP}" "${out}" \
    && die "the beta twin resolved the STABLE stamp — a beta host would be moved onto stable silently:
${out}"
printf '  OK: resolved %s from <comp>/beta/latest.json\n' "${BETA_STAMP}"

# ---- (2) GitHub is the fallback, and only that ------------------------------
say "STABLE: the manifest host unreachable falls back to the release list"
out="$(run_resolve "${REPO_ROOT}/clawee/install.sh" "${DEAD}" "${BASE}")"
has "the manifest host did not answer" "${out}" \
    || die "no fallback was announced when the manifest host was unreachable:
${out}"
has "clawee/${GH_ONLY_STAMP}" "${out}" \
    || die "the tag-list fallback did not resolve the newest stable tag:
${out}"
printf '  OK: fell back to the release list and resolved %s\n' "${GH_ONLY_STAMP}"

say "BETA: the tag-list fallback picks a BETA tag, never the newest stable one"
out="$(run_resolve "${REPO_ROOT}/clawee/beta.install.sh" "${DEAD}" "${BASE}")"
has "clawee/v0.9.0.beta.2026.09.08.dddddddd" "${out}" \
    || die "the beta twin's tag-list fallback did not pick the beta tag:
${out}"
has "${GH_ONLY_STAMP}" "${out}" \
    && die "the beta twin's fallback resolved the newest STABLE tag — the tag list is not channel-aware, and this is the filter that makes it safe to use:
${out}"
printf '  OK: fell back to the beta tag only\n'

# ---- (2b) a manifest that ANSWERS "not found" is an answer ------------------
say "manifest host UP but the path 404s: refuse, and never reach the tag list"
# This is the yank case, and it is the reason the two failure shapes must not be
# collapsed. Yank removes the manifest entry when no public row remains and
# deliberately leaves the GitHub release standing, so a 404 that fell through to
# the tag list would resolve — and install — the build just withdrawn. The
# fixture reproduces it exactly: the server is up, claweed has no manifest at
# all, and the tag list is serving tags.
mkdir -p "${W}/srv/repos/clawee-git/release"
cat > "${W}/srv/repos/clawee-git/release/releases" <<JSON
[
  {"tag_name": "claweed/${GH_ONLY_STAMP}"},
  {"tag_name": "clawee/${GH_ONLY_STAMP}"}
]
JSON
out="$(run_resolve "${REPO_ROOT}/claweed/install.sh" "${BASE}" "${BASE}")"
has "this channel is serving nothing" "${out}" \
    || die "a 404 from a reachable manifest host was not treated as an answer:
${out}"
has "${GH_ONLY_STAMP}" "${out}" \
    && die "a manifest 404 fell through to the tag list and resolved a tag. The tag list records every release ever published INCLUDING YANKED ONES, and yank is precisely what removes a manifest entry while leaving the GitHub release — so this reinstalls the withdrawn build:
${out}"
has "would reinstall exactly what was just taken down" "${out}" \
    || die "the refusal does not say why the tag list is not consulted:
${out}"
[ ! -e "${W}/prefix/bin/claweed" ] || die "the withdrawn build was INSTALLED"
printf '  OK: refused without consulting the release list\n'
# Restore the clawee-only tag list the later cases expect.
cat > "${W}/srv/repos/clawee-git/release/releases" <<JSON
[
  {"tag_name": "clawee/${GH_ONLY_STAMP}"},
  {"tag_name": "clawee/v0.9.0.beta.2026.09.08.dddddddd"}
]
JSON

say "the beta twin refuses the same way once a cycle closes"
# A closed cycle is the same shape: the beta manifest is gone, the previous
# cycle's beta release is still on GitHub.
rm -f "${W}/srv/clawee/beta/latest.json"
out="$(run_resolve "${REPO_ROOT}/clawee/beta.install.sh" "${BASE}" "${BASE}")"
has "this channel is serving nothing" "${out}" \
    || die "a closed beta cycle did not read as an answer:
${out}"
has "v0.9.0.beta.2026.09.08.dddddddd" "${out}" \
    && die "the beta twin reinstalled the previous cycle's beta from the tag list:
${out}"
printf '  OK: a closed cycle installs nothing rather than the last beta\n'
write_beta_manifest

# ---- (3) both unreachable is a REFUSAL, not a guess -------------------------
say "BOTH unreachable: refuse, naming the channel"
out="$(run_resolve "${REPO_ROOT}/clawee/beta.install.sh" "${DEAD}" "${DEAD}")"
has "nothing is published for clawee on the beta channel" "${out}" \
    || die "with no source reachable the bootstrap must refuse and name the channel:
${out}"
[ ! -e "${W}/prefix/bin/clawee" ] || die "something was INSTALLED although nothing resolved"
printf '  OK: refused, and installed nothing\n'

# ---- (4) a manifest naming the WRONG channel is ignored ---------------------
say "A stable stamp in the beta manifest is ignored, not installed"
cp "${W}/srv/clawee/latest.json" "${W}/srv/clawee/beta/latest.json"
out="$(run_resolve "${REPO_ROOT}/clawee/beta.install.sh" "${BASE}" "${DEAD}")"
has "is not a beta stamp" "${out}" \
    || die "a stable stamp in the beta manifest was not rejected:
${out}"
has "manifest: clawee/${STABLE_STAMP}" "${out}" \
    && die "the beta twin installed a STABLE stamp because the manifest named one — the manifest says what to install, not which channel a host is on:
${out}"
printf '  OK: refused the mismatched stamp\n'

# ---- (5) the trust gate is untouched ---------------------------------------
# Resolution decides WHICH release; these lines decide whether its bytes are
# trusted. Moving the first must not have touched the second, and a grep is the
# right check because the alternative — a full verified install — is what
# tools/test-e2e.sh already runs end to end.
say "STATIC: signature and checksum verification are still in every bootstrap"
for f in "${REPO_ROOT}"/clawee/*.sh "${REPO_ROOT}"/claweed/*.sh; do
    grep -q 'minisign' "$f"      || die "$(basename "$f"): no minisign verification"
    grep -q 'SHA256SUMS.txt'     "$f" || die "$(basename "$f"): no checksum verification"
    grep -q 'refusing to install unverified bytes' "$f" \
        || die "$(basename "$f"): the download failure no longer refuses"
done
printf '  OK: every generated bootstrap still verifies signature + sha256\n'

printf '\nALL OK — resolution reads the channel manifest first, GitHub only as a fallback\n'
