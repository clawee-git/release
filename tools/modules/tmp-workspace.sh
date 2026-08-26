# module: tmp-workspace  v1
# needs:  helpers
# since:  2026-08-25
TMP="$(mktemp -d "${TMPDIR:-/tmp}/@brand@-${COMP}-XXXXXX")" || fail "could not create temp dir"
trap 'rm -rf "$TMP"' EXIT INT TERM
