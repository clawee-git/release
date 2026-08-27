# module: require-minisign  v2
# needs:  helpers platform-detect install-minisign-common
# since:  2026-08-25
# minisign is the trust root of this install. The install-minisign-* modules
# above try to PROVIDE it: the OS package manager first, then the official
# upstream archive whose SHA-256 is pinned in this bootstrap — the bootstrap is
# the install's trust root already, so a hash it carries makes the fetched
# verifier exactly as trusted as the script itself (see install-minisign-common).
# This module only DECIDES: an executable $MINISIGN set by those modules, else
# PATH, else a copy at the install destination or the Homebrew locations a
# daemon-hosted shell cannot see (minisign_known), else refuse. Verification
# is mandatory and is never skipped.
if [ -n "$MINISIGN" ] && [ -x "$MINISIGN" ]; then
    :
elif command -v minisign >/dev/null 2>&1; then
    MINISIGN=minisign
else
    MINISIGN="$(minisign_known)" || MINISIGN=""
fi
if [ -z "$MINISIGN" ]; then
    case "$OS" in
        darwin) hint="install Homebrew if you don't have it, then minisign:
      /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\"
      brew install minisign" ;;
        *)      hint="the package manager and the pinned upstream download both failed —
      check the network and the @BRAND@_GH_PROXY mirrors, or install it by hand:
      https://github.com/jedisct1/minisign/releases/tag/$MINISIGN_VERSION" ;;
    esac
    fail "minisign is required and could not be provided — install it and re-run.
    $hint
    upstream: https://github.com/jedisct1/minisign
    Verification is mandatory; this installer will NOT proceed without a verifier."
fi
