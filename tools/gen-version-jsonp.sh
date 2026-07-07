#!/bin/sh
# gen-version-jsonp.sh — write <comp>/version.js, a JSONP snippet reporting the
# current PUBLISHED version of each component. Consumed by clawee.org to render a
# live version badge via a plain <script src> (JSONP — no CORS, works on the
# static release.clawee.org surface with no dynamic backend).
#
# Source of truth: the R2 catalog latest.json
# (downloads.clawee.org/<comp>/latest.json — updated by release.sh's R2 mirror
# step), with the local versions/<comp> file as an offline fallback. The emitted
# file calls a FIXED global callback the page defines before injecting the script:
#
#   __claweeVersion({"component":"clawee","version":"0.1.80","stamp":"v0.1.80.…"});
#
# Usage:
#   tools/gen-version-jsonp.sh                # both components
#   tools/gen-version-jsonp.sh clawee         # just one (release.sh passes the cut comp)
#
# Env:
#   CLAWEE_VERSION_CALLBACK      global callback name (default __claweeVersion)
#   CLAWEE_R2_DOWNLOADS_BASE     R2 catalog base URL (default https://downloads.clawee.org)
set -eu

ROOT="$(CDPATH='' cd -- "$(dirname -- "$0")/.." && pwd)"
CALLBACK="${CLAWEE_VERSION_CALLBACK:-__claweeVersion}"
R2_BASE="${CLAWEE_R2_DOWNLOADS_BASE:-https://downloads.clawee.org}"

COMPS="$*"
[ -n "${COMPS}" ] || COMPS="clawee claweed"

# json_str KEY < json  — extract a top-level string value from the pretty-printed
# latest.json (one "key": "value" pair per line). Portable sed, no jq dependency.
json_str() {
    sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -n1
}

for comp in ${COMPS}; do
    case "${comp}" in
        clawee|claweed) ;;
        *) echo "✗ unknown component: ${comp}" >&2; exit 2 ;;
    esac

    version=""; stamp=""
    # Prefer the authoritative R2 catalog (updated before this step runs).
    json="$(curl -fsSL --max-time 10 "${R2_BASE}/${comp}/latest.json" 2>/dev/null || true)"
    if [ -n "${json}" ]; then
        version="$(printf '%s\n' "${json}" | json_str version)"
        stamp="$(printf '%s\n' "${json}" | json_str stamp)"
    fi
    # These remote-sourced values are embedded into version.js — served on
    # release.clawee.org and EXECUTED as JS on clawee.org. The [^"]* extraction
    # above already prevents quote breakout, but validate the shape too so a
    # corrupted/hostile catalog value (spaces, backslashes, garbage) can't
    # propagate verbatim; on mismatch fall back as if the catalog were absent.
    if [ -n "${version}" ] && ! printf '%s' "${version}" | grep -Eq '^[0-9][0-9.]*$'; then
        echo "⚠ ${comp}: malformed catalog version '${version}' — falling back to versions/${comp}" >&2
        version=""
    fi
    if [ -n "${stamp}" ] && ! printf '%s' "${stamp}" | grep -Eq '^v[0-9A-Za-z.]*$'; then
        echo "⚠ ${comp}: malformed catalog stamp '${stamp}' — omitting stamp" >&2
        stamp=""
    fi
    # Offline fallback: the local marketing version (no stamp).
    [ -n "${version}" ] || version="$(cat "${ROOT}/versions/${comp}" 2>/dev/null || true)"
    [ -n "${version}" ] || { echo "✗ no version for ${comp} (R2 catalog + versions/${comp} both empty)" >&2; exit 1; }

    out="${ROOT}/${comp}/version.js"
    mkdir -p "${ROOT}/${comp}"
    tmp="${out}.tmp.$$"
    printf '%s({"component":"%s","version":"%s","stamp":"%s"});\n' \
        "${CALLBACK}" "${comp}" "${version}" "${stamp}" > "${tmp}"
    mv -f "${tmp}" "${out}"
    echo "✓ wrote ${out} (${comp} ${version})"
done
