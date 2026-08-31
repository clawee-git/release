#!/usr/bin/env bash
# prune-releases.test.sh — channel-anchored retention: a stable prune must never
# count or delete a beta tag, and a beta prune must never touch a stable one.
# Stubs ghp so no network call and no deletion happens.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
STUB="$(mktemp -d)"
trap 'rm -rf "${STUB}"' EXIT

# Twelve stable tags and two beta tags for one component.
cat > "${STUB}/tags.txt" <<'TAGS'
clawee/v0.1.1.2026.06.13.aaaaaaaa
clawee/v0.1.2.2026.06.13.bbbbbbbb
clawee/v0.1.3.2026.06.14.cccccccc
clawee/v0.1.4.2026.06.14.dddddddd
clawee/v0.1.5.2026.06.15.eeeeeeee
clawee/v0.1.6.2026.06.15.ffffffff
clawee/v0.1.7.2026.06.16.11111111
clawee/v0.1.8.2026.06.16.22222222
clawee/v0.1.9.2026.06.17.33333333
clawee/v0.1.10.2026.06.17.44444444
clawee/v0.1.11.2026.06.18.55555555
clawee/v0.1.12.2026.06.18.66666666
clawee/v0.2.1.beta.2026.08.20.77777777
clawee/v0.2.2.beta.2026.08.21.88888888
TAGS

# ghp stub: `api` prints the tag list, `release delete` records the tag.
cat > "${STUB}/ghp" <<STUBEOF
#!/usr/bin/env bash
case "\$1" in
  api)     cat "${STUB}/tags.txt" ;;
  release) if [ "\$2" = delete ]; then echo "\$3" >> "${STUB}/deleted.txt"; fi ;;
esac
exit 0
STUBEOF
chmod +x "${STUB}/ghp"

# CLAWEE_DOWNLOADS_BASE= (empty) disables the R2-presence guard entirely, per
# the script's own documentation — no curl stub needed (and a stub would be
# dead code anyway: the script prepends /usr/bin:/bin:/opt/homebrew/bin to
# PATH ahead of any test-stub dir, so the real curl always wins).
#
# "$@" here is per-call overrides (e.g. KEEP=10 CHANNEL=stable). Those words
# come from function positional params, not literal script text, so bash's
# own assignment-prefix parsing does NOT treat them as env assignments — it
# tries to run "KEEP=10" as a command and fails with 127, silently, under the
# `|| true` below. Routing them through the external `env` avoids that: env
# parses its own argv, so the parameter-expanded NAME=value words work.
run() { env PATH="${STUB}:${PATH}" COMPONENTS=clawee CLAWEE_DOWNLOADS_BASE= "$@" bash "${HERE}/prune-releases.sh" --execute; }

fail=0

# --- stable prune: 12 stable tags, keep 10 → deletes the 2 oldest STABLE ones,
# --- and never a beta tag.
: > "${STUB}/deleted.txt"
run KEEP=10 CHANNEL=stable >/dev/null 2>&1 || true
if grep -q 'beta' "${STUB}/deleted.txt"; then
    echo "✗ stable prune deleted a beta tag:"; grep beta "${STUB}/deleted.txt"; fail=1
fi
if [ "$(grep -c . "${STUB}/deleted.txt")" -ne 2 ]; then
    echo "✗ stable prune deleted $(grep -c . "${STUB}/deleted.txt") tag(s), want 2"; fail=1
fi

# --- beta prune: 2 beta tags, keep 1 → deletes the older BETA one only.
: > "${STUB}/deleted.txt"
run CHANNEL=beta >/dev/null 2>&1 || true
if [ "$(grep -c . "${STUB}/deleted.txt")" -ne 1 ]; then
    echo "✗ beta prune deleted $(grep -c . "${STUB}/deleted.txt") tag(s), want 1"; fail=1
fi
if ! grep -q 'v0.2.1.beta' "${STUB}/deleted.txt"; then
    echo "✗ beta prune deleted the wrong tag: $(cat "${STUB}/deleted.txt")"; fail=1
fi

[ "${fail}" -eq 0 ] && echo "✓ prune-releases channel anchoring OK"
exit "${fail}"
