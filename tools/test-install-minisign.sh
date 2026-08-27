#!/bin/sh
# tools/test-install-minisign.sh — the bootstrap provides minisign before it
# requires it, and the trust gate is not weakened by that.
#
#     sh tools/test-install-minisign.sh          # this shell (macOS /bin/sh = bash 3.2)
#     dash tools/test-install-minisign.sh        # harness under dash; the BLOCKS run under
#                                                # dash only where `sh` IS dash (the CI container)
#
# The install-minisign-* blocks are extracted verbatim from a GENERATED
# bootstrap and driven under a controlled PATH / PREFIX / TMP with DL_BASE
# pointing at a file:// directory of fixture archives. The only edits ever
# made to the extracted copy are (a) the pin constants, replaced by the
# fixture's real sha256 so a fixture can pass the pin, and (b) the known-paths
# list, pointed at nowhere so a Homebrew minisign on this machine is invisible.
# Cases:
#   PRESENT       minisign on PATH -> no fetch, no write, MINISIGN empty
#   DEST-PRESENT  minisign at PREFIX/bin, off PATH -> no fetch, untouched, require uses it
#   USER-HAPPY    non-root, sudo -n fails -> package manager never called,
#                 pinned fixture installs into PREFIX/bin, MINISIGN set
#   PIN-MISMATCH  fixture bytes != pin -> nothing installed; require refuses
#   MUTATION      with the pin comparison removed, PIN-MISMATCH would install ->
#                 the case can fail, so it means something
#   SEAL-FAILS    fixture minisign rejects the .minisig -> binary removed
#   ROOT-PM       root + apt-get that "installs" -> package manager path taken,
#                 no fetch; SKIP-PM -> apt-get not called, fetch taken
#   DARWIN        arm64 fixture zip installs; amd64 without brew refuses with
#                 the brew hint
#   REAL          the real pinned upstream release archive for THIS host, if
#                 reachable, else SKIPPED (offline) on its own line;
#                 RELEASE_TESTS_OFFLINE=1 skips it by request (cut on an
#                 air-gapped host)
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }
sha_of() {
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    else sha256sum "$1" | awk '{print $1}'; fi
}

# ---- locate a generated bootstrap that carries the blocks -------------------
GEN=""
for f in "$REPO_ROOT"/*/install.sh; do
    case "$f" in */inner/*) continue ;; esac
    grep -q '^# BEGIN install-minisign-common$' "$f" 2>/dev/null && { GEN="$f"; break; }
done
[ -n "$GEN" ] || die "no generated */install.sh carries '# BEGIN install-minisign-common' — is the template wired and regenerated?"
say "extracting the blocks from ${GEN#"$REPO_ROOT"/}"
extract() { sed -n "/^# BEGIN $1\$/,/^# END $1\$/p" "$GEN"; }
{
    extract helpers; extract sha256
    extract install-minisign-common; extract install-minisign-linux; extract install-minisign-darwin
} > "$W/block.sh"
extract require-minisign > "$W/require.sh"
for m in helpers sha256 install-minisign-common install-minisign-linux install-minisign-darwin; do
    grep -q "^# END $m\$" "$W/block.sh" || die "could not extract the $m block (markers missing or renamed)"
done
grep -q '^# END require-minisign$' "$W/require.sh" || die "could not extract the require-minisign block"
# The pins must be single-line assignments, or the substitutions below miss.
for v in MINISIGN_VERSION MINISIGN_LINUX_SHA256 MINISIGN_MACOS_SHA256 MINISIGN_KNOWN_PATHS; do
    [ "$(grep -c "^$v=" "$W/block.sh")" = 1 ] || die "expected exactly one '$v=' line in the extracted block"
done
# Read the pinned version from the same extracted block the fixtures below have
# to match — a pin bump never has to touch this suite.
MV="$(sed -n 's/^MINISIGN_VERSION="\(.*\)"$/\1/p' "$W/block.sh")"
[ -n "$MV" ] || die "could not read MINISIGN_VERSION from the extracted block"
sed -i.orig "s|^MINISIGN_KNOWN_PATHS=.*|MINISIGN_KNOWN_PATHS=\"$W/nowhere/minisign\"|" "$W/block.sh"
rm -f "$W/block.sh.orig"
printf '  OK\n'

# ---- a PATH that holds only what the blocks need --------------------------
# Neither this Mac's /opt/homebrew/bin nor the container's /usr/bin may leak a
# real minisign, brew, sudo or apt-get into a case.
mkdir -p "$W/sys" "$W/bin"
for t in sh mkdir install curl tar unzip rm basename dirname awk sed shasum sha256sum cat gzip chmod; do
    p="$(command -v "$t" 2>/dev/null || true)"
    [ -n "$p" ] && ln -s "$p" "$W/sys/$t"
done
[ -x "$W/sys/curl" ] || die "curl not found"
[ -x "$W/sys/tar" ] || die "tar not found"
SYSPATH="$W/bin:$W/sys"

# stub_id <uid>  — `id -u` answers <uid>
stub_id() { printf '#!/bin/sh\n[ "$1" = -u ] && { echo %s; exit 0; }\nexec /usr/bin/id "$@"\n' "$1" > "$W/bin/id"; chmod +x "$W/bin/id"; }
# stub_sudo — `sudo -n …` fails (no cached credential); anything else is a PROMPT, recorded
stub_sudo() {
    cat > "$W/bin/sudo" <<'STUB'
#!/bin/sh
[ "$1" = -n ] && exit 1
echo "SUDO PROMPT: $*" >> "$LOG"; exit 1
STUB
    chmod +x "$W/bin/sudo"
}
# stub_apt <installs:0|1> — records every call; with 1, drops a fake minisign on PATH
stub_apt() {
    cat > "$W/bin/apt-get" <<STUB
#!/bin/sh
echo "APT: \$*" >> "\$LOG"
[ "$1" = 1 ] && [ "\$1" = install ] && { printf '#!/bin/sh\necho minisign $MV (apt stub)\n' > "$W/bin/minisign"; chmod +x "$W/bin/minisign"; }
exit 0
STUB
    chmod +x "$W/bin/apt-get"
}
# also resets SKIP_PM / KEEP_PREFIX / CASE_MINISIGN: in dash, `SKIP_PM=1 run …`
# leaves the variable set after the call
clear_stubs() { rm -f "$W/bin/id" "$W/bin/sudo" "$W/bin/apt-get" "$W/bin/minisign" "$W/bin/brew"; SKIP_PM=""; KEEP_PREFIX=""; CASE_MINISIGN=""; }

# ---- fixtures: a fake static minisign inside the upstream archive layouts ---
# fake_minisign <path> <verify-rc>  — `-v` prints a version; `-V*` exits <rc>
fake_minisign() {
    mkdir -p "$(dirname "$1")"
    printf '#!/bin/sh\ncase "$1" in -v) echo "minisign '"$MV"' (fixture)" ;; -V*) exit %s ;; esac\n' "$2" > "$1"
    chmod +x "$1"
}
# linux_fixture <serve-dir> <verify-rc> — builds minisign-$MV-linux.tar.gz (+ .minisig) and prints its sha256
linux_fixture() {
    _lf="$W/lf.$$"; rm -rf "$_lf"; mkdir -p "$_lf" "$1"
    fake_minisign "$_lf/minisign-linux/x86_64/minisign" "$2"
    fake_minisign "$_lf/minisign-linux/aarch64/minisign" "$2"
    (cd "$_lf" && tar czf "$1/minisign-$MV-linux.tar.gz" minisign-linux)
    printf 'untrusted comment: fixture\nsig\ntrusted comment: fixture\nsig\n' > "$1/minisign-$MV-linux.tar.gz.minisig"
    sha_of "$1/minisign-$MV-linux.tar.gz"
}
# macos_fixture <serve-dir> <verify-rc> — builds minisign-$MV-macos.zip (+ .minisig) and prints its sha256
macos_fixture() {
    _mf="$W/mf.$$"; rm -rf "$_mf"; mkdir -p "$_mf" "$1"
    fake_minisign "$_mf/minisign" "$2"
    (cd "$_mf" && rm -f "$1/minisign-$MV-macos.zip" && zip -q "$1/minisign-$MV-macos.zip" minisign)
    printf 'untrusted comment: fixture\nsig\ntrusted comment: fixture\nsig\n' > "$1/minisign-$MV-macos.zip.minisig"
    sha_of "$1/minisign-$MV-macos.zip"
}
# pin <block-copy> <VAR> <sha> — substitute a pin in a COPY of the block
pin() { sed -i.bak "s|^$2=.*|$2=\"$3\"|" "$1"; rm -f "$1.bak"; }

# ---- driver -----------------------------------------------------------------
# Runs the block (and, with WITH_REQUIRE=1, the require block) in a child sh so
# `fail` exits the child, not this script. Prints the block's output, then a
# final MINISIGN=<value> line.
cat > "$W/run.sh" <<'RUNNER'
#!/bin/sh
set -eu
OS="$CASE_OS"; ARCH="$CASE_ARCH"; PREFIX="$CASE_PREFIX"; TMP="$CASE_TMP"
DL_BASE="$CASE_DL_BASE"; GH_PROXIES=""; CURL="curl -fsSL"; COMP=test
MINISIGN="${CASE_MINISIGN:-}"
mkdir -p "$TMP"
. "$BLOCK"
[ -z "${WITH_REQUIRE:-}" ] || . "$REQUIRE"
printf 'MINISIGN=%s\n' "${MINISIGN:-}"
RUNNER

# run <block> <os> <arch> <serve-dir> [WITH_REQUIRE=1] — fresh PREFIX/TMP per call
run() {
    [ -n "${KEEP_PREFIX:-}" ] || rm -rf "$W/prefix"
    rm -rf "$W/tmp"; mkdir -p "$W/prefix" "$W/tmp"
    CASE_OS="$2" CASE_ARCH="$3" CASE_PREFIX="$W/prefix" CASE_TMP="$W/tmp" CASE_DL_BASE="file://$4" \
    CASE_MINISIGN="${CASE_MINISIGN:-}" \
    BLOCK="$1" REQUIRE="$W/require.sh" WITH_REQUIRE="${5:-}" LOG="$W/log" \
    MINISIGN_SKIP_PM="${SKIP_PM:-}" PATH="$SYSPATH" HOME="$W/home" \
        sh "$W/run.sh" 2>&1
}
has() { case "$1" in *"$2"*) return 0 ;; esac; return 1; }

# ============================================================================
say "CASE PRESENT: minisign already on PATH -> nothing fetched, nothing written"
clear_stubs; rm -f "$W/log"; stub_id 1000; stub_sudo
printf '#!/bin/sh\necho minisign '"$MV"' (present)\n' > "$W/bin/minisign"; chmod +x "$W/bin/minisign"
mkdir -p "$W/serve-empty"
out="$(run "$W/block.sh" linux arm64 "$W/serve-empty")" || die "PRESENT: block exited non-zero:
$out"
has "$out" "minisign:" && die "PRESENT: the block spoke although minisign is present:
$out"
[ ! -e "$W/prefix/bin/minisign" ] || die "PRESENT: wrote into PREFIX/bin although minisign is present"
[ "$(printf '%s\n' "$out" | tail -1)" = "MINISIGN=" ] || die "PRESENT: MINISIGN should be empty: $out"
out="$(run "$W/block.sh" linux arm64 "$W/serve-empty" 1)" || die "PRESENT+require: refused although minisign is present:
$out"
has "$out" "MINISIGN=minisign" || die "PRESENT+require: expected MINISIGN=minisign (PATH lookup): $out"
printf '  OK\n'

# ============================================================================
say "CASE PRESEED: a pre-seeded \$MINISIGN in the environment is discarded, not trusted"
clear_stubs; rm -f "$W/log"; stub_id 1000; stub_sudo
# the unmodified block carries the REAL pin; "$W/serve-empty" holds no fixture
# that could match it, so nothing installs here either — same stubs as PIN-MISMATCH
mkdir -p "$W/serve-empty"
if out="$(CASE_MINISIGN=/bin/echo run "$W/block.sh" linux amd64 "$W/serve-empty" 1)"; then
    die "PRESEED: the trust gate ACCEPTED a pre-seeded \$MINISIGN with nothing verified:
$out"
fi
has "$out" "could not be provided" || die "PRESEED: expected the refusal: $out"
printf '  OK\n'

# ============================================================================
say "CASE USER-HAPPY: non-root, no passwordless sudo -> no package manager, pinned fixture installs"
clear_stubs; rm -f "$W/log"; stub_id 1000; stub_sudo; stub_apt 0
S="$W/serve-good"; rm -rf "$S"; sha="$(linux_fixture "$S" 0)"
cp "$W/block.sh" "$W/block-good.sh"; pin "$W/block-good.sh" MINISIGN_LINUX_SHA256 "$sha"
out="$(run "$W/block-good.sh" linux arm64 "$S" 1)" || die "USER-HAPPY: refused:
$out"
[ ! -f "$W/log" ] || die "USER-HAPPY: the package manager or a sudo prompt was invoked:
$(cat "$W/log")"
has "$out" "skipping the package manager" || die "USER-HAPPY: expected the no-root/no-sudo line: $out"
[ -x "$W/prefix/bin/minisign" ] || die "USER-HAPPY: PREFIX/bin/minisign was not installed:
$out"
has "$out" "MINISIGN=$W/prefix/bin/minisign" || die "USER-HAPPY: MINISIGN should point at PREFIX/bin/minisign: $out"
has "$out" "pinned upstream build)" || die "USER-HAPPY: expected the ok line: $out"
v="$("$W/prefix/bin/minisign" -v)"; has "$v" "fixture" || die "USER-HAPPY: installed binary is not the fixture: $v"
printf '  OK\n'

# ============================================================================
say "CASE DEST-PRESENT: minisign already at PREFIX/bin but not on PATH -> nothing fetched, not overwritten, require resolves to it"
clear_stubs; rm -f "$W/log"; stub_id 1000; stub_sudo
rm -rf "$W/prefix"; mkdir -p "$W/prefix/bin"
printf '#!/bin/sh\necho minisign '"$MV"' (operator copy)\n' > "$W/prefix/bin/minisign"; chmod +x "$W/prefix/bin/minisign"
before="$(sha_of "$W/prefix/bin/minisign")"
# block-good.sh + the good fixture: if the guard failed, this WOULD install over the copy
out="$(KEEP_PREFIX=1 run "$W/block-good.sh" linux arm64 "$S" 1)" || die "DEST-PRESENT: refused:
$out"
has "$out" "minisign:" && die "DEST-PRESENT: the block spoke although a minisign is at PREFIX/bin:
$out"
[ "$(sha_of "$W/prefix/bin/minisign")" = "$before" ] || die "DEST-PRESENT: the operator's minisign at PREFIX/bin was overwritten"
has "$out" "MINISIGN=$W/prefix/bin/minisign" || die "DEST-PRESENT: require should resolve to PREFIX/bin/minisign: $out"
printf '  OK\n'

# ============================================================================
say "CASE DEST-UNWRITABLE: PREFIX/bin exists but cannot be written -> the write failure is reported, not swallowed"
if [ "$(/usr/bin/id -u)" = 0 ]; then
    printf '  SKIPPED (running as root)\n'
else
    clear_stubs; rm -f "$W/log"; stub_id 1000; stub_sudo
    rm -rf "$W/prefix"; mkdir -p "$W/prefix/bin"; chmod 0555 "$W/prefix/bin"
    out="$(KEEP_PREFIX=1 run "$W/block-good.sh" linux arm64 "$S" 1)" || true
    chmod 0755 "$W/prefix/bin"
    has "$out" "cannot write" || die "DEST-UNWRITABLE: expected install_file's own write-failure message: $out"
    [ ! -e "$W/prefix/bin/minisign" ] || die "DEST-UNWRITABLE: something was written into a read-only bin"
    printf '  OK\n'
fi

# ============================================================================
say "CASE PIN-MISMATCH: fixture bytes != pinned sha256 -> nothing installed, require refuses"
clear_stubs; rm -f "$W/log"; stub_id 1000; stub_sudo
# the unmodified block carries the REAL pin; the fixture cannot match it
out="$(run "$W/block.sh" linux amd64 "$S")" || die "PIN-MISMATCH: block (without require) must not exit non-zero:
$out"
has "$out" "does not match the pinned sha256" || die "PIN-MISMATCH: expected the discard line: $out"
[ ! -e "$W/prefix/bin/minisign" ] || die "PIN-MISMATCH: a mismatching archive was INSTALLED"
has "$out" "MINISIGN=/" && die "PIN-MISMATCH: MINISIGN set although nothing verified: $out"
if out="$(run "$W/block.sh" linux amd64 "$S" 1)"; then
    die "PIN-MISMATCH+require: the trust gate ACCEPTED with no verifier:
$out"
fi
has "$out" "could not be provided" || die "PIN-MISMATCH+require: wrong refusal: $out"
has "$out" "_GH_PROXY" || die "PIN-MISMATCH+require: the Linux hint should name the mirror env: $out"
printf '  OK\n'

# ============================================================================
say "CASE MUTATION: with the pin comparison removed, PIN-MISMATCH would install"
cp "$W/block.sh" "$W/block-mut.sh"
# the comparison line, in the shipped block:  [ "$_mf_got" = "$_mf_want" ] && return 0
sed -i.bak 's|^\([[:space:]]*\)\[ "\$_mf_got" = "\$_mf_want" \] && return 0$|\1return 0|' "$W/block-mut.sh"; rm -f "$W/block-mut.sh.bak"
cmp -s "$W/block.sh" "$W/block-mut.sh" && die "MUTATION: the pin comparison line was not found — the mutation did not apply (has the module changed shape?)"
out="$(run "$W/block-mut.sh" linux amd64 "$S")" || true
[ -x "$W/prefix/bin/minisign" ] || die "MUTATION: with the pin check removed the fixture should have installed — PIN-MISMATCH cannot detect a missing pin check:
$out"
printf '  OK (the case can fail)\n'

# ============================================================================
say "CASE SEAL-FAILS: archive matches the pin, its .minisig does not verify -> binary removed"
clear_stubs; rm -f "$W/log"; stub_id 1000; stub_sudo
S2="$W/serve-badsig"; rm -rf "$S2"; sha="$(linux_fixture "$S2" 1)"
cp "$W/block.sh" "$W/block-badsig.sh"; pin "$W/block-badsig.sh" MINISIGN_LINUX_SHA256 "$sha"
out="$(run "$W/block-badsig.sh" linux arm64 "$S2")" || die "SEAL-FAILS: block exited non-zero:
$out"
has "$out" "did not verify — removing" || die "SEAL-FAILS: expected the removal line: $out"
[ ! -e "$W/prefix/bin/minisign" ] || die "SEAL-FAILS: the binary was left installed after a failed seal"
has "$out" "MINISIGN=/" && die "SEAL-FAILS: MINISIGN set after a failed seal: $out"
printf '  OK\n'

# ============================================================================
say "CASE ROOT-PM: root + a package manager that installs -> no fetch"
clear_stubs; rm -f "$W/log"; stub_id 0; stub_apt 1
out="$(run "$W/block-good.sh" linux arm64 "$S")" || die "ROOT-PM: block exited non-zero:
$out"
grep -q '^APT: update' "$W/log" 2>/dev/null || die "ROOT-PM: apt-get update was not called as root: $(cat "$W/log" 2>/dev/null)"
grep -q '^APT: install -y minisign' "$W/log" || die "ROOT-PM: apt-get install was not called: $(cat "$W/log")"
has "$out" "installed by the package manager" || die "ROOT-PM: expected the package-manager ok line: $out"
[ ! -e "$W/prefix/bin/minisign" ] || die "ROOT-PM: fetched the upstream build although the package manager succeeded"
printf '  OK\n'

say "CASE SKIP-PM: root, MINISIGN_SKIP_PM=1 -> package manager not called, fetch taken"
clear_stubs; rm -f "$W/log"; stub_id 0; stub_apt 1
out="$(SKIP_PM=1 run "$W/block-good.sh" linux arm64 "$S")" || die "SKIP-PM: block exited non-zero:
$out"
[ ! -f "$W/log" ] || die "SKIP-PM: the package manager was called despite MINISIGN_SKIP_PM: $(cat "$W/log")"
[ -x "$W/prefix/bin/minisign" ] || die "SKIP-PM: the pinned build was not installed:
$out"
printf '  OK\n'

# ============================================================================
say "CASE DARWIN arm64: no brew -> pinned fixture zip installs"
clear_stubs; rm -f "$W/log"; stub_id 501
if command -v zip >/dev/null 2>&1 && [ -x "$W/sys/unzip" ]; then
    S3="$W/serve-mac"; rm -rf "$S3"; sha="$(macos_fixture "$S3" 0)"
    cp "$W/block.sh" "$W/block-mac.sh"; pin "$W/block-mac.sh" MINISIGN_MACOS_SHA256 "$sha"
    out="$(run "$W/block-mac.sh" darwin arm64 "$S3" 1)" || die "DARWIN arm64: refused:
$out"
    [ -x "$W/prefix/bin/minisign" ] || die "DARWIN arm64: PREFIX/bin/minisign not installed:
$out"
    has "$out" "MINISIGN=$W/prefix/bin/minisign" || die "DARWIN arm64: MINISIGN should point at PREFIX/bin/minisign: $out"
    printf '  OK\n'
else
    printf '  SKIPPED (no zip/unzip to build and open the macOS fixture)\n'
fi

say "CASE DARWIN amd64: no brew -> names the gap, require refuses with the brew recipe"
clear_stubs; rm -f "$W/log"; stub_id 501
if out="$(run "$W/block.sh" darwin amd64 "$W/serve-empty" 1)"; then
    die "DARWIN amd64: the trust gate ACCEPTED with no verifier:
$out"
fi
has "$out" "no Intel build" || die "DARWIN amd64: expected the Intel gap line: $out"
has "$out" "brew install minisign" || die "DARWIN amd64: the refusal should carry the brew recipe: $out"
printf '  OK\n'

# ============================================================================
say "CASE REAL: the pinned $MV archive for this host (network; SKIPPED if unreachable)"
clear_stubs; rm -f "$W/log"; stub_id 1000; stub_sudo
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/release-tests/minisign-$MV"
case "$(uname -s)/$(uname -m)" in
    Linux/x86_64)  ros=linux;  rarch=amd64; asset=minisign-$MV-linux.tar.gz ;;
    Linux/aarch64) ros=linux;  rarch=arm64; asset=minisign-$MV-linux.tar.gz ;;
    Darwin/arm64)  ros=darwin; rarch=arm64; asset=minisign-$MV-macos.zip ;;
    *)             ros=""; asset="" ;;
esac
if [ -z "$ros" ]; then
    printf '  SKIPPED (no upstream build for %s/%s)\n' "$(uname -s)" "$(uname -m)"
else
    # The pin the shipped block carries for THIS host's archive — read from the
    # unmodified extracted copy, so a stale or truncated cache file is rejected
    # by the same value the bootstrap enforces, and refetched.
    case "$asset" in
        *linux*) want="$(sed -n 's/^MINISIGN_LINUX_SHA256="\(.*\)"$/\1/p' "$W/block.sh")" ;;
        *)       want="$(sed -n 's/^MINISIGN_MACOS_SHA256="\(.*\)"$/\1/p' "$W/block.sh")" ;;
    esac
    [ -n "$want" ] || die "REAL: could not read the pin for $asset from the extracted block"
    if [ -s "$CACHE/$asset" ] && [ "$(sha_of "$CACHE/$asset")" != "$want" ]; then
        printf '  cached %s does not match the pin — refetching\n' "$asset"
        rm -f "$CACHE/$asset" "$CACHE/$asset.minisig"
    fi
    if [ -n "${RELEASE_TESTS_OFFLINE:-}" ]; then
        printf '  SKIPPED (offline, by request): RELEASE_TESTS_OFFLINE is set\n'
    else
        mkdir -p "$CACHE"
        for a in "$asset" "$asset.minisig"; do
            [ -s "$CACHE/$a" ] || curl -fsSL --connect-timeout 5 --max-time 60 -o "$CACHE/$a" \
                "https://github.com/jedisct1/minisign/releases/download/$MV/$a" 2>/dev/null || rm -f "$CACHE/$a"
        done
        if [ -s "$CACHE/$asset" ] && [ -s "$CACHE/$asset.minisig" ]; then
            out="$(run "$W/block.sh" "$ros" "$rarch" "$CACHE" 1)" || die "REAL: the real archive did not install (cache: $CACHE — rm -rf it to refetch; if that does not help the pin is stale):
$out"
            [ -x "$W/prefix/bin/minisign" ] || die "REAL: not installed:
$out"
            v="$("$W/prefix/bin/minisign" -v 2>&1)"; has "$v" "minisign $MV" || die "REAL: installed binary reports '$v'"
            printf '  OK: real minisign '"$MV"' installed, pin and upstream signature both hold\n'
        else
            printf '  SKIPPED (offline): could not fetch %s from GitHub (cache: %s)\n' "$asset" "$CACHE"
        fi
    fi
fi

printf '\nALL OK — minisign is provided before it is required, and the gate still fails closed\n'
