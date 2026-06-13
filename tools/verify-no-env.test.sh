#!/usr/bin/env bash
# verify-no-env.test.sh — proves tools/verify-no-env.sh fails on a binary that
# embeds a forbidden config-env literal and passes on one built from claweed main.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
GUARD="${HERE}/verify-no-env.sh"
GO_BIN="${GO_BIN:-go}"
command -v "${GO_BIN}" >/dev/null 2>&1 || GO_BIN=/opt/homebrew/bin/go
CLAWEED_SRC="${CLAWEED_SRC:-/Volumes/MacintoshED/Workstation/Coding/Clawee/daemon/code/daemon}"
TMP="$(mktemp -d)"; trap 'rm -rf "$TMP"' EXIT

mkdir -p "${TMP}/stale"
cat > "${TMP}/stale/main.go" <<'GO'
package main
import "fmt"
func mustEnv(k string) string { return k }
func main() { fmt.Println(mustEnv("CLAWEE_DATA_DIR")) }
GO
( cd "${TMP}/stale" && "${GO_BIN}" mod init stale >/dev/null 2>&1 && "${GO_BIN}" build -o "${TMP}/stale-bin" . )

echo "# expect FAIL on the stale binary"
if "${GUARD}" "${TMP}/stale-bin"; then echo "FAIL: guard passed a stale binary"; exit 1; fi
echo "stale-binary correctly rejected"

( cd "${CLAWEED_SRC}" && CGO_ENABLED=0 "${GO_BIN}" build -trimpath -o "${TMP}/claweed-bin" ./cmd/claweed )
echo "# expect PASS on the main claweed binary"
"${GUARD}" "${TMP}/claweed-bin" || { echo "FAIL: guard rejected the main claweed binary"; exit 1; }

echo "ALL OK"
