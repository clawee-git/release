# module: platform-detect  v1
# needs:  helpers
# since:  2026-08-25
case "$(uname -s)" in
    Darwin) OS=darwin ;;
    Linux)  OS=linux ;;
    *)      fail "unsupported OS: $(uname -s) (@brand@ ships darwin + linux only)" ;;
esac
case "$(uname -m)" in
    arm64|aarch64) ARCH=arm64 ;;
    x86_64|amd64)  ARCH=amd64 ;;
    *)             fail "unsupported arch: $(uname -m) (@brand@ ships arm64 + amd64 only)" ;;
esac

printf '\n  @brand@ %s installer  (%s/%s)\n\n' "$COMP" "$OS" "$ARCH"
