#!/usr/bin/env bash
# gen-version-jsonp.test.sh — the version badge's JSONP, over fixture manifests.
#
# The badge is the one place a reader outside this system learns what is
# current. It is generated from the CHANNEL MANIFEST, which promote writes as
# the go-live, so it can never announce a build nobody approved — and this
# suite is what holds that, plus the three ways a manifest can be wrong:
# missing, malformed, or naming the other channel's stamp.
#
# Nothing here reaches the real downloads host: the base is a local HTTP server.
#
#     bash tools/gen-version-jsonp.test.sh
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
W="$(mktemp -d)"
SERVER_PID=""
cleanup() { [ -n "${SERVER_PID}" ] && kill "${SERVER_PID}" 2>/dev/null; rm -rf "${W}"; return 0; }
trap cleanup EXIT

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }
has() { case "$2" in *"$1"*) return 0 ;; *) return 1 ;; esac; }

# The generator writes into the REPO's own <comp>/ dirs, so the test runs
# against a COPY of the repo — a suite that rewrote the committed version.js
# would leave the tree dirty and the next cut's pre-flight would refuse it.
SANDBOX="${W}/repo"
mkdir -p "${SANDBOX}/tools" "${SANDBOX}/versions" "${SANDBOX}/clawee" "${SANDBOX}/claweed"
cp "${REPO_ROOT}/tools/gen-version-jsonp.sh" "${SANDBOX}/tools/"
echo "0.4.7" > "${SANDBOX}/versions/clawee"
echo "0.4.7" > "${SANDBOX}/versions/claweed"

mkdir -p "${W}/srv/clawee/beta" "${W}/srv/claweed"
write_manifest() {
    # write_manifest <path> <version> <stamp>
    cat > "${W}/srv/$1" <<JSON
{
  "component": "clawee",
  "stamp": "$3",
  "version": "$2"
}
JSON
}
write_manifest clawee/latest.json      0.4.7 v0.4.7.2026.09.05.aaaaaaaa
write_manifest clawee/beta/latest.json 0.5.0 v0.5.0.beta.2026.09.05.bbbbbbbb
# claweed gets a stable manifest and NO beta one: "one component has a beta and
# the other does not" is the ordinary state of an open cycle, not an edge case.
write_manifest claweed/latest.json     0.4.7 v0.4.7.2026.09.05.aaaaaaaa

PORT="$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
( cd "${W}/srv" && exec python3 -m http.server "${PORT}" --bind 127.0.0.1 ) >/dev/null 2>&1 &
SERVER_PID=$!
disown "${SERVER_PID}" 2>/dev/null || true
for _ in $(seq 1 50); do
    curl -fsS "http://127.0.0.1:${PORT}/clawee/latest.json" -o /dev/null 2>/dev/null && break
    sleep 0.1
done
BASE="http://127.0.0.1:${PORT}"

gen() { CLAWEE_R2_DOWNLOADS_BASE="$1" sh "${SANDBOX}/tools/gen-version-jsonp.sh" "${@:2}"; }

# ---- (1) both channels, both components -------------------------------------
say "both channels are rendered, and each names its own"
gen "${BASE}" >/dev/null 2>&1 || die "the generator failed against a good fixture"
for f in clawee/version.js clawee/beta.version.js claweed/version.js claweed/beta.version.js; do
    [ -f "${SANDBOX}/${f}" ] || die "${f} was not written"
done
stable="$(cat "${SANDBOX}/clawee/version.js")"
beta="$(cat "${SANDBOX}/clawee/beta.version.js")"
has '__claweeVersion({"component":"clawee","channel":"stable","version":"0.4.7","stamp":"v0.4.7.2026.09.05.aaaaaaaa"});' "${stable}" \
    || die "the stable snippet is not the agreed payload: ${stable}"
has '__claweeVersion({"component":"clawee","channel":"beta","version":"0.5.0","stamp":"v0.5.0.beta.2026.09.05.bbbbbbbb"});' "${beta}" \
    || die "the beta snippet is not the agreed payload: ${beta}"
printf '  OK: %s\n  OK: %s\n' "${stable}" "${beta}"

# ---- (2) a channel serving nothing still gets a file ------------------------
say "a channel with no manifest gets an EMPTY snippet, not a missing file"
empty="$(cat "${SANDBOX}/claweed/beta.version.js")"
has '"channel":"beta","version":"","stamp":""' "${empty}" \
    || die "claweed has no beta manifest; the snippet must say so rather than be absent or stale: ${empty}"
printf '  OK: %s\n' "${empty}"

# ---- (3) the stable offline fallback ----------------------------------------
say "with the manifest host unreachable, stable falls back to versions/<comp>"
DEAD="http://127.0.0.1:$(python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1]);s.close()')"
gen "${DEAD}" clawee >/dev/null 2>&1 || die "the generator failed with the manifest host down"
out="$(cat "${SANDBOX}/clawee/version.js")"
has '"channel":"stable","version":"0.4.7","stamp":""' "${out}" \
    || die "the offline fallback did not use versions/clawee (and must carry no stamp): ${out}"
out="$(cat "${SANDBOX}/clawee/beta.version.js")"
has '"channel":"beta","version":"","stamp":""' "${out}" \
    || die "beta has no offline fallback — there is no local record of what a beta channel serves: ${out}"
printf '  OK: stable fell back, beta went empty\n'

# ---- (4) a manifest naming the other channel's stamp ------------------------
say "a stable stamp in the beta manifest is refused, not repeated"
write_manifest clawee/beta/latest.json 0.4.7 v0.4.7.2026.09.05.aaaaaaaa
log="$(gen "${BASE}" clawee 2>&1)"
has "manifest names the stable stamp" "${log}" \
    || die "the mismatch was not reported: ${log}"
out="$(cat "${SANDBOX}/clawee/beta.version.js")"
has '"channel":"beta","version":"","stamp":""' "${out}" \
    || die "the badge repeated a stable stamp as a beta one — every reader would be told a cycle is open when none is: ${out}"
printf '  OK: refused and went empty\n'

# ---- (5) a hostile value never reaches the emitted script -------------------
say "a malformed manifest version does not reach the emitted JavaScript"
cat > "${W}/srv/clawee/latest.json" <<'JSON'
{
  "component": "clawee",
  "stamp": "v0.4.7.2026.09.05.aaaaaaaa",
  "version": "0.4.7\"}); alert(1); ({\"x\":\"y"
}
JSON
log="$(gen "${BASE}" clawee 2>&1)"
out="$(cat "${SANDBOX}/clawee/version.js")"
has "alert(1)" "${out}" \
    && die "a manifest value was embedded verbatim into a file clawee.org EXECUTES: ${out}"
has "malformed manifest version" "${log}" || die "the malformed value was not reported: ${log}"
has '"version":"0.4.7"' "${out}" || die "the fallback did not take over: ${out}"
printf '  OK: rejected, fell back to versions/clawee\n'

printf '\nALL OK — the badge reports both channels, from the manifest, or says nothing\n'
