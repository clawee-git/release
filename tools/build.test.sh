#!/usr/bin/env bash
# build.test.sh — tools/build.sh names its outputs and stamps its ldflags from
# the CHANNEL, and refuses a channel it cannot spell.
#
# The real compiler is replaced by a stub `go` that RECORDS its argv and touches
# the -o target. That is the whole point: what is under test is not whether Go
# compiles, it is what build.sh ASKS Go for — the -X main.channel value and the
# output basename — and those two agreeing is the thing that keeps a beta twin
# from writing into a stable tree. A test that ran the real compiler would need
# the cli and daemon worktrees and would still not assert on the flags.
#
# `go run ./cmd/channel-names` is NOT stubbed: it is this kit's own code and the
# single source the test exists to pin. It runs against the real toolchain.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_SH="${REPO_ROOT}/tools/build.sh"

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

REAL_GO="${GO_BIN:-$(command -v go 2>/dev/null || true)}"
[ -x "${REAL_GO}" ] || die "no Go toolchain found (set GO_BIN)"

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

LOG="${WORK}/go.log"
STUB="${WORK}/stub"; mkdir -p "${STUB}"
# The stub answers `build` itself (logging the argv and creating the output so
# the caller's own checks pass) and forwards everything else — `run` — to the
# real toolchain, so cmd/channel-names is genuinely exercised.
cat > "${STUB}/go" <<EOF
#!/usr/bin/env bash
if [ "\$1" = build ]; then
    echo "go \$*" >> "${LOG}"
    out=""
    prev=""
    for a in "\$@"; do
        [ "\${prev}" = -o ] && out="\${a}"
        prev="\${a}"
    done
    [ -n "\${out}" ] && : > "\${out}"
    exit 0
fi
exec "${REAL_GO}" "\$@"
EOF
chmod +x "${STUB}/go"

# A source worktree that only has to EXIST — the stub never reads it.
SRC="${WORK}/src"; mkdir -p "${SRC}"

run_build() {
    local comp="$1" channel="$2" out="$3"
    : > "${LOG}"
    rm -rf "${out}"; mkdir -p "${out}"
    COMP="${comp}" SRC_DIR="${SRC}" TARGETOS=linux TARGETARCH=arm64 \
        STAMP="v0.3.0.beta.2026.09.05.deadbeef" OUT_DIR="${out}" \
        GO_BIN="${STUB}/go" CHANNEL="${channel}" \
        bash "${BUILD_SH}" >/dev/null 2>&1
}

# ---- stable: the names every README already carries -------------------------
say "CHANNEL defaults to stable and keeps the stable names"
OUT="${WORK}/out"
: > "${LOG}"; mkdir -p "${OUT}"
COMP=clawee SRC_DIR="${SRC}" TARGETOS=linux TARGETARCH=arm64 \
    STAMP=v0.2.28.2026.09.05.deadbeef OUT_DIR="${OUT}" GO_BIN="${STUB}/go" \
    bash "${BUILD_SH}" >/dev/null 2>&1 || die "a default-channel build failed"
[ -f "${OUT}/clawee" ] && [ -f "${OUT}/clawee-updater" ] \
    || die "default channel did not emit clawee + clawee-updater: $(ls -A "${OUT}")"
grep -q -- '-X main.channel=stable' "${LOG}" \
    || die "no -X main.channel=stable in the default build:
$(cat "${LOG}")"
printf '  ✓ clawee, clawee-updater, -X main.channel=stable\n'

# ---- beta: the twin names, from the same source packages --------------------
say "CHANNEL=beta names the twins and stamps the beta channel"
run_build clawee beta "${OUT}" || die "a beta clawee build failed"
[ -f "${OUT}/claweeb" ] && [ -f "${OUT}/claweeb-updater" ] \
    || die "beta did not emit claweeb + claweeb-updater: $(ls -A "${OUT}")"
[ -e "${OUT}/clawee" ] && die "a beta build wrote the STABLE basename clawee"
grep -q -- '-X main.channel=beta' "${LOG}" || die "no -X main.channel=beta:
$(cat "${LOG}")"
# The PACKAGE is never renamed — a twin is one source built twice, and
# ./cmd/claweeb does not exist.
grep -qF -- './cmd/clawee' "${LOG}" \
    || die "the beta build did not compile ./cmd/clawee:
$(cat "${LOG}")"
! grep -qF -- './cmd/claweeb' "${LOG}" \
    || die "the beta build asked for ./cmd/claweeb, which does not exist"
printf '  ✓ claweeb, claweeb-updater, from ./cmd/clawee, -X main.channel=beta\n'

say "CHANNEL=beta names the daemon twins"
run_build claweed beta "${OUT}" || die "a beta claweed build failed"
[ -f "${OUT}/claweedb" ] && [ -f "${OUT}/claweedb-updater" ] \
    || die "beta did not emit claweedb + claweedb-updater: $(ls -A "${OUT}")"
[ -e "${OUT}/claweed" ] && die "a beta build wrote the STABLE basename claweed"
printf '  ✓ claweedb, claweedb-updater\n'

# ---- a channel nobody can spell is a usage error, not a stable build --------
# Defaulting an unknown channel to stable is how a typo'd cycle ships a beta
# tree into the stable catalog.
say "an unknown CHANNEL is refused"
set +e
COMP=clawee SRC_DIR="${SRC}" TARGETOS=linux TARGETARCH=arm64 \
    STAMP=v1.2.3.2026.09.05.deadbeef OUT_DIR="${WORK}/out-bad" GO_BIN="${STUB}/go" \
    CHANNEL=nightly bash "${BUILD_SH}" >/dev/null 2>&1
rc=$?
set -e
[ "${rc}" -eq 2 ] || die "CHANNEL=nightly should be a usage error (exit 2), got ${rc}"
[ -d "${WORK}/out-bad" ] && [ -n "$(ls -A "${WORK}/out-bad" 2>/dev/null)" ] \
    && die "a refused build still produced binaries"
printf '  ✓ exit 2, nothing built\n'

printf '\n✓ build.test.sh passed\n'
