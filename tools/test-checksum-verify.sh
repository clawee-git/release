#!/bin/sh
# tools/test-checksum-verify.sh — the outer bootstrap's checksum gate must work
# on hashers that predate `--ignore-missing`.
#
# The gate used to run `shasum -a 256 -c --ignore-missing SHA256SUMS.txt`. That
# option is a 2016-era addition (Digest::SHA 5.96 / coreutils 8.25): the stock
# shasum on an older macOS answers "Unknown option: ignore-missing" and exits
# non-zero, which the `||` reported as "checksum mismatch". Every install on
# such a host therefore accused an intact, correctly signed zip of tampering
# (seen 2026-08-25, gateway install on a 2012 Mac mini).
#
#     sh tools/test-checksum-verify.sh          # this shell
#     dash tools/test-checksum-verify.sh        # and this one, always
#
# (1) STATIC   — no bootstrap, template or generated, may name --ignore-missing,
#     and the advice a human READS — site/index.html, the release notes in
#     tools/release.sh — must verify one file by exact name rather than run
#     `-c` over the whole sums file, which fails on an intact download.
# (2) BEHAVIOR — the shipped block, extracted verbatim from clawee/install.sh, is
#     driven against a stub `shasum` that models the old one: good zip verifies,
#     tampered zip aborts, absent entry aborts, and a name that merely CONTAINS
#     the wanted one is not mistaken for it.
set -eu

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
W="$(mktemp -d)"
trap 'rm -rf "$W"' EXIT

say() { printf '\n=== %s ===\n' "$*"; }
die() { printf '\n✗ FAILED: %s\n' "$*" >&2; exit 1; }

# ---- (1) STATIC: the option must not come back ------------------------------
say "STATIC: no --ignore-missing in any bootstrap"
# Code lines only: the block carries a comment naming the option it dropped.
found="$(grep -lE '^[^#]*--ignore-missing' \
    "$REPO_ROOT"/tools/bootstrap.template.sh \
    "$REPO_ROOT"/*/install.sh \
    "$REPO_ROOT"/*/upgrade.sh 2>/dev/null || true)"
[ -z "$found" ] \
    || die "these still verify with --ignore-missing (breaks pre-2016 hashers):
$found"
printf '  OK: no --ignore-missing\n'

# The hand-verify recipe an operator READS is a third surface, and nothing gated
# it: the glob above only ever looked at bootstraps, so the download page and
# the generated GitHub release notes went on printing `-c` over the whole sums
# file after the READMEs were corrected (2026-08-26). That advice accuses an
# intact download of tampering — the reader has one file, the checklist names
# every file — and `sha256sum -c` on an empty or malformed checklist exits 0,
# so the obvious repair reports success having verified nothing. Both surfaces
# must select the entry by exact name and shout when there is none, as the
# installer's own gate does.
# tools/release.sh USED TO BE IN THIS LIST, because it wrote the GitHub release
# notes and the recipe was in that heredoc. A cut no longer creates a GitHub
# Release at all — the notes are written at PROMOTE — so release.sh carries no
# operator-facing verify advice for this to check, and listing it would only
# assert that a file it no longer writes still says something.
#
# WHEN PROMOTE LANDS, ADD ITS NOTES WRITER TO THIS LIST. The advice is not
# retired, it moved, and a surface nothing gates is exactly how the download
# page went on printing the wrong recipe for two months after the READMEs were
# fixed.
say "STATIC: operator-facing verify advice selects one file by name, not -c over the sums file"
for advice in "$REPO_ROOT/site/index.html"; do
    rel="${advice#"$REPO_ROOT"/}"
    [ -f "$advice" ] || { printf '  (no %s — skipped)\n' "$rel"; continue; }
    # Command-shaped hits only: quote, entity and tag boundaries end the match,
    # so prose that merely names the old flag does not trip this.
    bad="$(grep -nE '(shasum|sha256sum)[^<&"]*-c[^<&"]*SHA256SUMS' "$advice" || true)"
    [ -z "$bad" ] || die "$rel still tells the reader to verify with -c over the whole SHA256SUMS.txt
(one downloaded file + a checklist naming every file = a false tampering verdict):
$bad"
    # shellcheck disable=SC2016  # the literal text $f is the point — this greps
    # for the recipe's no-entry arm. `\\?` allows the release-notes copy, which
    # lives in an unquoted heredoc and so writes that dollar as \$f.
    grep -qE 'NO ENTRY for \\?\$f in SHA256SUMS\.txt' "$advice" \
        || die "$rel has lost the no-entry arm of the verify recipe — it must fail loudly when the downloaded name is not listed, not report success on an empty selection"
    printf '  OK: %s selects by exact name and shouts when there is none\n' "$rel"
done

# ---- (2) extract the shipped block ------------------------------------------
# Out of the GENERATED clawee/install.sh, so the test drives the bytes that ship.
say "BEHAVIOR: extracting the checksum-verify block from clawee/install.sh"
sed -n '/^# BEGIN verify-checksum/,/^# END verify-checksum/p' \
    "$REPO_ROOT/clawee/install.sh" > "$W/verify.sh"
grep -q '^# END verify-checksum' "$W/verify.sh" \
    || die "could not extract the checksum-verify block from clawee/install.sh (markers missing or renamed)"
sed -n '/^sha256_of()/,/^}/p' "$REPO_ROOT/clawee/install.sh" > "$W/sha256_of.sh"
grep -q '^sha256_of()' "$W/sha256_of.sh" \
    || die "could not extract sha256_of() from clawee/install.sh"

# claweed shares the same verify-checksum module as clawee (both are @INCLUDE:
# splices of the same tools/modules/verify-checksum.sh) — extract it out of the
# GENERATED claweed/install.sh too, so both of Clawee's shipped copies of the
# portable checksum gate are driven by this test, not just clawee's. Same
# marker names as clawee's block ("verify-checksum") because it's the same
# module; only the source file differs.
say "BEHAVIOR: extracting the verify-checksum block from claweed/install.sh"
sed -n '/^# BEGIN verify-checksum/,/^# END verify-checksum/p' \
    "$REPO_ROOT/claweed/install.sh" > "$W/verify_claweed.sh"
grep -q '^# END verify-checksum' "$W/verify_claweed.sh" \
    || die "could not extract the verify-checksum block from claweed/install.sh (markers missing or renamed)"
sed -n '/^sha256_of()/,/^}/p' "$REPO_ROOT/claweed/install.sh" > "$W/sha256_of_claweed.sh"
grep -q '^sha256_of()' "$W/sha256_of_claweed.sh" \
    || die "could not extract sha256_of() from claweed/install.sh"

# ---- stub hashers -----------------------------------------------------------
# `shasum` as an old Digest::SHA ships it: -a 256 <file> works, --ignore-missing
# does not exist. Anything the block relies on beyond that is a portability bug.
mkdir -p "$W/bin"
cat > "$W/bin/shasum" <<'STUB'
#!/bin/sh
for a in "$@"; do
    case "$a" in
        --ignore-missing)
            echo "Unknown option: ignore-missing" >&2
            echo "Type shasum -h for help" >&2
            exit 1 ;;
    esac
done
exec /usr/bin/shasum "$@"
STUB
chmod +x "$W/bin/shasum"
[ -x /usr/bin/shasum ] || die "no /usr/bin/shasum to back the stub"

# The stub must actually model the old tool, or every case below passes vacuously.
if PATH="$W/bin:$PATH" shasum -a 256 -c --ignore-missing /dev/null >/dev/null 2>&1; then
    die "the stub accepted --ignore-missing — it does not model a pre-2016 shasum"
fi
printf '  OK: stub shasum rejects --ignore-missing, as the old one does\n'

# ---- driver -----------------------------------------------------------------
# Sources the two extracted blocks in a child shell so `fail` can exit without
# killing this script. $TMP and $ZIP are the block's inputs.
cat > "$W/run.sh" <<'RUNNER'
#!/bin/sh
set -eu
info() { :; }
ok()   { printf 'OK: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
. "$BLOCK_DIR/sha256_of.sh"
TMP="$FIXTURE"
ZIP="$WANT_ZIP"
. "$BLOCK_DIR/verify.sh"
ok "checksum verified"
RUNNER

# fixture: the wanted zip, a decoy whose NAME CONTAINS the wanted one, and a
# third entry for a file that was never downloaded (the case --ignore-missing
# was there to tolerate).
F="$W/fixture"
mkdir -p "$F"
printf 'the real payload\n'  > "$F/clawee-clawee-darwin-amd64.zip"
printf 'not the payload\n'   > "$F/x-clawee-clawee-darwin-amd64.zip"
{
    /usr/bin/shasum -a 256 "$F/x-clawee-clawee-darwin-amd64.zip" | sed 's| .*/| |'
    /usr/bin/shasum -a 256 "$F/clawee-clawee-darwin-amd64.zip"   | sed 's| .*/| |'
    echo "0000000000000000000000000000000000000000000000000000000000000000  clawee-clawee-linux-arm64.zip"
} > "$F/SHA256SUMS.txt"

run() {  # run <fixture-dir> <zip>
    FIXTURE="$1" WANT_ZIP="$2" BLOCK_DIR="$W" PATH="$W/bin:$PATH" \
        sh "$W/run.sh" 2>&1
}

# claweed's driver: same shape as clawee's — both are @INCLUDE splices — sources sha256_of and
# reads $ZIP, since both share the verify-checksum module.
cat > "$W/run_claweed.sh" <<'RUNNER'
#!/bin/sh
set -eu
info() { :; }
ok()   { printf 'OK: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
. "$BLOCK_DIR/sha256_of_claweed.sh"
TMP="$FIXTURE"
ZIP="$WANT_ZIP"
. "$BLOCK_DIR/verify_claweed.sh"
ok "checksum verified"
RUNNER

run_claweed() {  # run_claweed <fixture-dir> <zip>
    FIXTURE="$1" WANT_ZIP="$2" BLOCK_DIR="$W" PATH="$W/bin:$PATH" \
        sh "$W/run_claweed.sh" 2>&1
}

say "CASE: intact zip, old shasum"
out="$(run "$F" clawee-clawee-darwin-amd64.zip)" \
    || die "intact zip rejected on a pre-2016 shasum — the bug is back:
$out"
case "$out" in *"checksum verified"*) ;; *) die "no 'checksum verified' in: $out" ;; esac
printf '  OK: verified\n'

say "CASE: binary-format sums line (hash *name)"
B="$W/binfmt"; mkdir -p "$B"
cp "$F/clawee-clawee-darwin-amd64.zip" "$B/"
/usr/bin/shasum -a 256 "$B/clawee-clawee-darwin-amd64.zip" \
    | sed 's|  .*/|  *|' > "$B/SHA256SUMS.txt"
grep -q ' \*clawee-clawee-darwin-amd64.zip$' "$B/SHA256SUMS.txt" \
    || die "fixture is not in binary format: $(cat "$B/SHA256SUMS.txt")"
out="$(run "$B" clawee-clawee-darwin-amd64.zip)" \
    || die "binary-format entry not matched: $out"
printf '  OK: verified\n'

say "CASE: tampered zip aborts"
T="$W/tampered"; mkdir -p "$T"
cp "$F/SHA256SUMS.txt" "$T/"
printf 'the real payloadX\n' > "$T/clawee-clawee-darwin-amd64.zip"
if out="$(run "$T" clawee-clawee-darwin-amd64.zip)"; then
    die "tampered zip was ACCEPTED: $out"
fi
case "$out" in *"checksum mismatch"*) ;; *) die "wrong refusal for a tampered zip: $out" ;; esac
printf '  OK: aborted with "checksum mismatch"\n'

say "CASE: no entry for the downloaded zip aborts"
if out="$(run "$F" clawee-clawee-windows-amd64.zip)"; then
    die "a zip with no sums entry was ACCEPTED: $out"
fi
case "$out" in *"no checksum entry"*) ;; *) die "wrong refusal for a missing entry: $out" ;; esac
printf '  OK: aborted with "no checksum entry"\n'

say "CASE: a longer name containing the wanted one is not a match"
# Only the decoy is listed; the wanted zip must NOT borrow its entry.
D="$W/decoy"; mkdir -p "$D"
cp "$F/clawee-clawee-darwin-amd64.zip" "$D/"
/usr/bin/shasum -a 256 "$F/x-clawee-clawee-darwin-amd64.zip" | sed 's| .*/| |' > "$D/SHA256SUMS.txt"
if out="$(run "$D" clawee-clawee-darwin-amd64.zip)"; then
    die "matched a DIFFERENT file's entry by substring: $out"
fi
case "$out" in *"no checksum entry"*) ;; *) die "wrong refusal for a substring-only name: $out" ;; esac
printf '  OK: aborted with "no checksum entry"\n'

# ---- same cases again, against claweed's independently-spliced verify-checksum block -------
# Same fixtures (F/B/T/D built above), same stub shasum — only the driver
# differs (run_claweed instead of run).

say "CASE (claweed): intact zip, old shasum"
out="$(run_claweed "$F" clawee-clawee-darwin-amd64.zip)" \
    || die "claweed: intact zip rejected on a pre-2016 shasum — the bug is back:
$out"
case "$out" in *"checksum verified"*) ;; *) die "claweed: no 'checksum verified' in: $out" ;; esac
printf '  OK: verified\n'

say "CASE (claweed): binary-format sums line (hash *name)"
out="$(run_claweed "$B" clawee-clawee-darwin-amd64.zip)" \
    || die "claweed: binary-format entry not matched: $out"
case "$out" in *"checksum verified"*) ;; *) die "claweed: no 'checksum verified' in: $out" ;; esac
printf '  OK: verified\n'

say "CASE (claweed): tampered zip aborts"
if out="$(run_claweed "$T" clawee-clawee-darwin-amd64.zip)"; then
    die "claweed: tampered zip was ACCEPTED: $out"
fi
case "$out" in *"checksum mismatch"*) ;; *) die "claweed: wrong refusal for a tampered zip: $out" ;; esac
printf '  OK: aborted with "checksum mismatch"\n'

say "CASE (claweed): no entry for the downloaded zip aborts"
if out="$(run_claweed "$F" clawee-clawee-windows-amd64.zip)"; then
    die "claweed: a zip with no sums entry was ACCEPTED: $out"
fi
case "$out" in *"no checksum entry"*) ;; *) die "claweed: wrong refusal for a missing entry: $out" ;; esac
printf '  OK: aborted with "no checksum entry"\n'

say "CASE (claweed): a longer name containing the wanted one is not a match"
if out="$(run_claweed "$D" clawee-clawee-darwin-amd64.zip)"; then
    die "claweed: matched a DIFFERENT file's entry by substring: $out"
fi
case "$out" in *"no checksum entry"*) ;; *) die "claweed: wrong refusal for a substring-only name: $out" ;; esac
printf '  OK: aborted with "no checksum entry"\n'

printf '\nALL OK — the checksum gate works on hashers without --ignore-missing\n'
