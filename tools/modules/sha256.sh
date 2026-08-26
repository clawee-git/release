# module: sha256  v1
# since:  2026-08-25
# sha256 of a file, as a bare hex digest. shasum on macOS, sha256sum on stock
# Debian/Ubuntu (which ships no perl and therefore no shasum). Both spellings
# are pre-2016-safe: no --ignore-missing, no --check.
sha256_of() {
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    elif command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    else return 1; fi
}
