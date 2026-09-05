#!/bin/sh
# gen-version-jsonp.sh — write the JSONP version snippets clawee.org's live
# badge loads with a plain <script src> (no CORS, no dynamic backend on the
# host serving them).
#
# ONE FILE PER COMPONENT PER CHANNEL, because there are two channels and a
# badge that can only say "the stable version" cannot say a beta cycle is open:
#
#   <comp>/version.js        stable   <- <comp>/latest.json
#   <comp>/beta.version.js   beta     <- <comp>/beta/latest.json
#
# Beside the stable name, never under a beta/ directory: <comp>/beta/ is where
# the channel MANIFEST lives on the downloads mirror, and the bootstraps use
# the same beside-not-under spelling for their twins.
#
# Each file calls a FIXED global callback the page defines before injecting the
# script, and the payload names its own channel so one callback can serve both:
#
#   __claweeVersion({"component":"clawee","channel":"stable","version":"0.1.80","stamp":"v0.1.80.…"});
#
# A BETA FILE IS ALWAYS WRITTEN, with an EMPTY version and stamp when that
# channel is serving nothing. The alternative — omitting the file — makes "no
# beta is open" indistinguishable from "the badge script failed to load", and
# leaves whatever a previous cycle published sitting on the host forever. An
# empty version is a statement; a missing file is a question.
#
# Source of truth: the channel manifest, which is what promote writes as the
# go-live — so this file can never announce a version no operator approved.
# versions/<comp> is a STABLE-only offline fallback (it is the number the NEXT
# cut will carry, which is close enough for a badge and wrong for anything
# else); beta has no such fallback, because there is no local record of what a
# beta channel is serving.
#
# Usage:
#   tools/gen-version-jsonp.sh                # both components, both channels
#   tools/gen-version-jsonp.sh clawee         # just one component, both channels
#
# Env:
#   CLAWEE_VERSION_CALLBACK      global callback name (default __claweeVersion)
#   CLAWEE_R2_DOWNLOADS_BASE     manifest base URL (default https://downloads.clawee.org)
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

# manifest_url COMP CHANNEL — the channel manifest's URL. The channel is a PATH
# SEGMENT, the same one internal/manifest publishes under.
manifest_url() {
    if [ "$2" = beta ]; then
        printf '%s/%s/beta/latest.json' "${R2_BASE}" "$1"
    else
        printf '%s/%s/latest.json' "${R2_BASE}" "$1"
    fi
}

# out_path COMP CHANNEL — where the snippet is written.
out_path() {
    if [ "$2" = beta ]; then
        printf '%s/%s/beta.version.js' "${ROOT}" "$1"
    else
        printf '%s/%s/version.js' "${ROOT}" "$1"
    fi
}

for comp in ${COMPS}; do
    case "${comp}" in
        clawee|claweed) ;;
        *) echo "✗ unknown component: ${comp}" >&2; exit 2 ;;
    esac

    for channel in stable beta; do
        version=""; stamp=""
        json="$(curl -fsSL --max-time 10 "$(manifest_url "${comp}" "${channel}")" 2>/dev/null || true)"
        if [ -n "${json}" ]; then
            version="$(printf '%s\n' "${json}" | json_str version)"
            stamp="$(printf '%s\n' "${json}" | json_str stamp)"
        fi
        # These remote-sourced values are embedded into a .js file that is
        # EXECUTED as script on clawee.org. The [^"]* extraction above already
        # prevents quote breakout; validate the shape too, so a corrupted or
        # hostile manifest value (spaces, backslashes, garbage) cannot
        # propagate verbatim. On mismatch, fall back as if it were absent.
        if [ -n "${version}" ] && ! printf '%s' "${version}" | grep -Eq '^[0-9][0-9A-Za-z.+-]*$'; then
            echo "⚠ ${comp}/${channel}: malformed manifest version '${version}' — ignoring it" >&2
            version=""
        fi
        if [ -n "${stamp}" ] && ! printf '%s' "${stamp}" | grep -Eq '^v[0-9A-Za-z.]*$'; then
            echo "⚠ ${comp}/${channel}: malformed manifest stamp '${stamp}' — omitting stamp" >&2
            stamp=""
        fi
        # A stamp must belong to the channel it was read for. A stable stamp in
        # the beta manifest is a publisher bug, and a badge that repeated it
        # would tell every reader a beta cycle is open when none is.
        if [ -n "${stamp}" ]; then
            case "${channel}:${stamp}" in
                beta:*.beta.*)   ;;
                stable:*.beta.*) echo "⚠ ${comp}/stable: manifest names the beta stamp '${stamp}' — ignoring it" >&2; version=""; stamp="" ;;
                beta:*)          echo "⚠ ${comp}/beta: manifest names the stable stamp '${stamp}' — ignoring it" >&2; version=""; stamp="" ;;
            esac
        fi
        # Offline fallback, STABLE only: the local marketing version, no stamp.
        if [ -z "${version}" ] && [ "${channel}" = stable ]; then
            version="$(cat "${ROOT}/versions/${comp}" 2>/dev/null || true)"
        fi
        if [ -z "${version}" ] && [ "${channel}" = stable ]; then
            echo "✗ no stable version for ${comp} (manifest + versions/${comp} both empty)" >&2
            exit 1
        fi

        out="$(out_path "${comp}" "${channel}")"
        mkdir -p "${ROOT}/${comp}"
        tmp="${out}.tmp.$$"
        printf '%s({"component":"%s","channel":"%s","version":"%s","stamp":"%s"});\n' \
            "${CALLBACK}" "${comp}" "${channel}" "${version}" "${stamp}" > "${tmp}"
        mv -f "${tmp}" "${out}"
        if [ -n "${version}" ]; then
            echo "✓ wrote ${out} (${comp} ${channel} ${version})"
        else
            echo "✓ wrote ${out} (${comp} ${channel}: nothing promoted)"
        fi
    done
done
