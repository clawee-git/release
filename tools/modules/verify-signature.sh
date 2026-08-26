# module: verify-signature  v1
# needs:  helpers require-minisign
# since:  2026-08-25
info "verifying signature"
# 1) signature over the sums file, using the baked pubkey (inline, no key fetch).
# Capture stdout — minisign prints the SIGNED "Trusted comment:" line there, and
# that comment is the only version-bearing field in the whole verified set (the
# zip name and SHA256SUMS.txt are both version-independent). stderr is left
# attached so a verification failure still shows minisign's own diagnostics.
verify_out="$("$MINISIGN" -V -P "$PUBKEY" -m "$TMP/SHA256SUMS.txt" -x "$TMP/SHA256SUMS.txt.minisig")" \
    || fail "signature verification failed — aborting (refusing to install unverified bytes)"
ok "minisign signature valid"
