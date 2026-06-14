#!/usr/bin/env bash
# verify-no-env.sh — fail if a built Clawee binary embeds a forbidden config-env
# runtime literal. claweed + clawee-spawn take ALL config from flags
# (`claweed serve --data-dir/--spawn-helper/--register-socket`) and root-owned
# files (the spawn allowlist) — never from env. This is the release-channel
# guard: run it on every freshly built component binary before publishing.
#
# Usage: verify-no-env.sh <binary> [<binary> ...]
# Forbidden literals (config-env names that must NOT drive runtime config):
#   CLAWEE_DATA_DIR       data-dir must come from --data-dir, not env
#   CLAWEE_SOCKET         the transport socket must come from a flag, not env
#   CLAWEE_SPAWN_HELPER   the spawn-helper path must come from --spawn-helper
#   mustEnv               the helper pattern that fatals on a missing required env
#
# NOTE: legitimate read-only env knobs are NOT forbidden — e.g. clawee reads
# $BURROWEE_TRANSPORT_SOCK as a socket OVERRIDE (flag > env > config > default),
# which is allowed; only the CLAWEE_* config-as-env anti-pattern is rejected.
#
# Exit 0 = clean; 1 = a forbidden literal is present; 2 = usage/strings error.
set -euo pipefail

[ "$#" -ge 1 ] || { echo "usage: verify-no-env.sh <binary> [<binary> ...]" >&2; exit 2; }
command -v strings >/dev/null 2>&1 || { echo "✗ 'strings' not found" >&2; exit 2; }

FORBIDDEN='CLAWEE_DATA_DIR|CLAWEE_SOCKET|CLAWEE_SPAWN_HELPER|mustEnv'
rc=0
for bin in "$@"; do
    [ -f "${bin}" ] || { echo "✗ not a file: ${bin}" >&2; exit 2; }
    hits="$(strings "${bin}" | grep -E -c "${FORBIDDEN}" || true)"
    if [ "${hits}" -ne 0 ]; then
        echo "✗ ${bin}: ${hits} forbidden env literal(s):" >&2
        strings "${bin}" | grep -E -n "${FORBIDDEN}" | sed 's/^/    /' >&2
        rc=1
    else
        echo "✓ ${bin}: no forbidden env literals"
    fi
done
exit "${rc}"
