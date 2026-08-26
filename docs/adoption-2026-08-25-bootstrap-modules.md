# Adoption: Clawee takes the shared bootstrap modules (2026-08-25)

Task 10 of the outer-bootstrap trust-chain plan. Clawee's outer bootstrap
(`tools/bootstrap.template.sh`) carried its own copy of the same trust chain
Burrowee built shared, versioned modules for (Tasks 1–9, in
`Burrowee/release/code/.worktrees/shasum-portable-checksum`). This is an
**adoption**, not a byte-identical extraction: Clawee's blocks are near-twins
of Burrowee's, not identical, so the gate here is not "bytes must not change"
but "every changed line is one we can explain."

Ten shared modules exist. Eight were adopted into Clawee's template via
`@INCLUDE:<name>@` (expanded at generation time by `tools/gen-bootstraps.sh`,
never at runtime — the bootstrap is a trust anchor delivered as `curl … | sh`
and must never fetch executable logic before the minisign gate). Two —
`download` and `version-resolve` — are recorded as **LOCAL FORK** and left as
Clawee's own blocks, with the `@INCLUDE:` line never committed for either —
but for two different reasons, not one. `download`'s shared module would
delete a working fallback Clawee has today. `version-resolve`'s shared module
is not a drop-in at all: it hardcodes Burrowee's four components and calls
three helpers (`resolve_latest`, `$CONSOLE_URL`, `assert_version_floor`) that
exist nowhere in Clawee — adopting it would be a rewrite of the module plus
those three helpers plus a generator bake, not a splice. See the LOCAL FORK
detail section below for each.

`tools/modules/download-r2-only.sh` was deleted before any of this: it mints
a console R2 URL for Burrowee's private/gated `relay` channel, and Clawee has
no gated channel at all.

## Per-module verdicts

| Module | Verdict | Notes |
|---|---|---|
| `helpers` | adopted unchanged | `fail`/`info`/`ok` — byte-identical after markers |
| `sha256` | adopted (new) | `sha256_of` is **new to Clawee** — see below |
| `platform-detect` | adopted unchanged | OS/arch detection + banner — byte-identical after `@brand@`→`clawee` |
| `pubkey-guard` | adopted unchanged | TEMP/placeholder pubkey guard — byte-identical |
| `tmp-workspace` | adopted unchanged | `mktemp -d` + `trap` — byte-identical after `@brand@`→`clawee` |
| `require-minisign` | adopted, behaviour change | darwin hint gains a Homebrew-bootstrap one-liner — see below |
| `verify-signature` | adopted, behaviour change | now captures `verify_out` — see below |
| `verify-checksum` | adopted, behaviour change | **the fix** — replaces `shasum -c --ignore-missing` — see below |
| `download` | **LOCAL FORK** | would delete Clawee's own `downloads.clawee.org` no-auth mirror fallback |
| `version-resolve` | **LOCAL FORK** | hardcodes Burrowee's 4 components and calls 3 helpers (`resolve_latest`, `$CONSOLE_URL`, `assert_version_floor`) absent from Clawee entirely — not a splice, a rewrite |

## Behaviour changes (exact lines, and why each is safe)

### 1. `sha256_of` — new to Clawee

Clawee had no standalone `sha256_of` helper before this change; the checksum
gate called `shasum`/`sha256sum` directly. The shared `sha256.sh` module
adds:

```sh
sha256_of() {
    if command -v shasum >/dev/null 2>&1; then shasum -a 256 "$1" | awk '{print $1}'
    elif command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | awk '{print $1}'
    else return 1; fi
}
```

spliced directly above the checksum gate (per the module order in the task
brief: `helpers, sha256, platform-detect, pubkey-guard, tmp-workspace,
require-minisign, verify-signature, verify-checksum, download,
version-resolve`). It is inert on its own — nothing calls it until the
`verify-checksum` module (below) is spliced in — and it uses neither spelling's
`--ignore-missing`/`--check` flags, so it is pre-2016-safe on both macOS and
Linux. Safe: pure addition, no existing call site touched.

### 2. `verify-checksum` — replaces `shasum -c --ignore-missing` (the fix)

This is the actual defect this whole effort exists to close. Before:

```sh
grep -qF "$ZIP" "$TMP/SHA256SUMS.txt" \
    || fail "no checksum entry for $ZIP — release incomplete or tampered; aborting"
if command -v shasum >/dev/null 2>&1; then
    ( cd "$TMP" && shasum -a 256 -c --ignore-missing SHA256SUMS.txt >/dev/null ) \
        || fail "checksum mismatch — aborting (zip tampered or download corrupted)"
elif command -v sha256sum >/dev/null 2>&1; then
    ( cd "$TMP" && sha256sum -c --ignore-missing SHA256SUMS.txt >/dev/null ) \
        || fail "checksum mismatch — aborting (zip tampered or download corrupted)"
else
    fail "neither shasum nor sha256sum found — cannot verify; aborting"
fi
```

After:

```sh
want="$(awk -v f="$ZIP" '{ n = $2; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' "$TMP/SHA256SUMS.txt")"
[ -n "$want" ] \
    || fail "no checksum entry for $ZIP — release incomplete or tampered; aborting"
got="$(sha256_of "$TMP/$ZIP")" \
    || fail "neither shasum nor sha256sum found — cannot verify; aborting"
[ -n "$got" ] && [ "$want" = "$got" ] \
    || fail "checksum mismatch — aborting (zip tampered or download corrupted)"
```

`--ignore-missing` is a 2016-era addition (Digest::SHA 5.96 / coreutils 8.25).
On a macOS whose stock `shasum` predates that, the flag itself is rejected
("Unknown option: ignore-missing"), the command exits non-zero, and the `||`
reports that as "checksum mismatch" — accusing an intact, correctly signed
zip of tampering. A real 2012 Mac mini hit exactly this on 2026-08-25; that
report is why this whole effort exists. The replacement compares ONE hash
directly (`sha256_of`, which uses neither flag) against the line picked by
exact filename match (the `awk`, handling both the plain and binary-mode `*`
prefix spellings) — strictly stricter than the substring `grep -qF` it
replaces, and it never invokes a `shasum`/`sha256sum` flag from any particular
era. Safe: this changes an error case (false-positive tamper report on old
hashers) into a correct pass, and the true-positive case (a genuinely wrong
hash) still fails exactly the same way.

### 3. `verify-signature` — now captures minisign's stdout

Before:

```sh
"$MINISIGN" -V -P "$PUBKEY" -m "$TMP/SHA256SUMS.txt" -x "$TMP/SHA256SUMS.txt.minisig" >/dev/null \
    || fail "signature verification failed — aborting (refusing to install unverified bytes)"
```

After:

```sh
verify_out="$("$MINISIGN" -V -P "$PUBKEY" -m "$TMP/SHA256SUMS.txt" -x "$TMP/SHA256SUMS.txt.minisig")" \
    || fail "signature verification failed — aborting (refusing to install unverified bytes)"
```

`verify_out` now holds minisign's stdout (the signed "Trusted comment:"
line), where before it was redirected to `/dev/null`. **Inert for Clawee**:
Burrowee's template consumes `$verify_out` in a tag-binding block right after
the signature check (comparing the release's trusted comment against the
resolved tag, to defeat a mirror silently serving an older signed release).
Clawee's template has no such block — `$verify_out` is set and never read.
Safe: capturing a command's stdout into a variable instead of discarding it
changes no control flow and produces no observable output difference (stderr
is unaffected either way, so minisign's own diagnostics on failure are still
shown).

### 4. `require-minisign` — darwin's missing-minisign hint gains a Homebrew bootstrap line

Before:

```sh
case "$OS" in
    darwin) hint="brew install minisign" ;;
    *)      hint="apt-get install minisign  (or your distro's package manager)" ;;
esac
```

After:

```sh
case "$OS" in
    darwin) hint="install Homebrew if you don't have it, then minisign:
      /bin/bash -c \"\$(curl -fsSL https://raw.githubusercontent.com/Homebrew/install/HEAD/install.sh)\"
      brew install minisign" ;;
    *)      hint="apt-get install minisign  (or your distro's package manager)" ;;
esac
```

**Why this needed the argument spelled out, not just "additive":** the new
`hint` text contains a `curl -fsSL … | ` — style command (piped through
`/bin/bash -c "$(...)"`) — a shape that, printed inside an installer whose
own header says "this installer will NOT run an unverified verifier," reads
at a glance like exactly the kind of unverified-fetch-and-run the trust chain
exists to prevent. That tension is real enough to need resolving explicitly
rather than waved off as "just a string."

What it actually is, resolved:

- `$hint` is **only ever interpolated into a `fail "…$hint…"` call**, and
  `fail()` is `printf '\n  ✗ %s\n\n' "$*" >&2; exit 1`. The Homebrew line is
  printed to stderr as advisory text and the script then exits 1. Nothing in
  `require-minisign.sh`, or anywhere else in the generated bootstrap,
  `eval`s, sources, or execs `$hint` — it is inert data from the shell's own
  point of view, on a path that has already decided to abort.
- It is shown **only** when `command -v minisign` has already failed on
  macOS — i.e. only after the installer has independently decided it cannot
  proceed. No install, download, or verification step runs after this point;
  the advisory text is the last thing printed before `exit 1`.
- Running the suggested command is an action the **operator** takes
  afterward, by hand, if they choose to. It is Homebrew's own official
  install one-liner (`raw.githubusercontent.com/Homebrew/install`), the same
  command Homebrew's own documentation publishes and the same trust decision
  an operator already makes on any fresh macOS box that wants Homebrew at
  all — this text does not ask them to trust anything new, only surfaces the
  standard bootstrap command instead of assuming Homebrew is already present.
  It is also the same shape of suggestion the very same message already made
  for Linux (`apt-get install minisign` — also "go run your package manager,"
  also not executed by this script).
- This module (and this exact hint text) is not new to the trust chain in
  the abstract: it is Burrowee's own `require-minisign` module, already
  shipping unchanged in `cli/gateway/edge/agent`'s real, production outer
  bootstraps before this task. Adopting it does not introduce a new pattern
  to evaluate in isolation; it brings Clawee's copy in line with Burrowee's
  existing one.

**Conclusion: safe.** "This installer will NOT run an unverified verifier" is
a claim about what the *script itself executes automatically* — it downloads
no verifier, execs no fetched binary, and the trust chain (minisign → sha256
→ unzip → exec) is untouched by this change. Advisory text on an abort path,
naming a well-known official bootstrap command for the operator to run (or
not) at their own discretion, is a different thing from that claim and does
not weaken it.

## LOCAL FORK detail

### `download` — kept Clawee's own block

The shared `download.sh` module's fallback chain, once GitHub and the
`GH_PROXIES` mirrors are exhausted, is a **grant-gated** R2 lookup: it shells
out to `clawee download-url <comp> <tag> <asset>` (Burrowee's console /
device-grant mechanism — `clawee login` renews the grant) and treats no
authorized CLI on PATH as a hard failure.

Clawee's own `dl()` instead falls back, last, to a **public, no-auth** mirror:
`downloads.clawee.org/<comp>/<stamp>/<file>` (`$DOWNLOADS_BASE`), a plain
bucket the operator controls, reachable by a fresh host that has never run
`clawee login` and may not even have `clawee` installed yet (e.g. a brand new
machine bootstrapping via `curl … | sh`). Forcing the shared module would
delete that path outright — a real fallback Clawee has today, not a
duplicate of anything the shared module offers — so per the task brief this
is recorded as a LOCAL FORK rather than forced. Clawee's own download block
is left in place in `tools/bootstrap.template.sh`, with a comment pointing at
this note; the `@INCLUDE:download@` substitution was tried, its diff read,
and reverted rather than committed.

### `version-resolve` — kept Clawee's own block

**Correction (fix round 1):** an earlier draft of this note justified this
fork by saying Clawee has "no `versions/` directory, no per-cut stamp." That
claim is false and has been removed. Clawee has `versions/clawee` (currently
`0.2.14`) and `versions/claweed` (`0.2.12`), and `tools/version.sh:32-41`
already treats each as that component's SemVer source of truth, read and
staged at cut time (`read_semver`/`write_semver`) — exactly the kind of input
Burrowee bakes `@MIN_VERSION@` from. Baking a `@MIN_VERSION@` placeholder from
`versions/<comp>` into Clawee's generator would be a small, mechanical
addition (read the file, substitute it in, same shape as the existing
`@COMP@`/`@MODE@`/`@PUBKEY@` bakes) — not an absent mechanism. The fork is
still correct; it just is not justified by a missing version file.

The actual, decisive reasons the shared module cannot be spliced in as-is:

1. **It hardcodes Burrowee's component set and aborts on Clawee's.**
   `tools/modules/version-resolve.sh:6-11`:
   ```sh
   case "$COMP" in
       cli)     PIN="${@BRAND@_CLI_VERSION:-}" ;;
       gateway) PIN="${@BRAND@_GATEWAY_VERSION:-}" ;;
       edge)    PIN="${@BRAND@_EDGE_VERSION:-}" ;;
       agent)   PIN="${@BRAND@_AGENT_VERSION:-}" ;;
       *)       fail "unknown component '$COMP' — cannot resolve its version pin" ;;
   esac
   ```
   `$COMP` is `clawee` or `claweed` for every render Clawee's generator does.
   Neither matches any of the four cases, so this is the module's first
   statement and it fails immediately: `✗ unknown component 'clawee' —
   cannot resolve its version pin`. Every unpinned AND pinned install (the
   pin lookup itself lives inside this same `case`) would abort at the very
   first line of version resolution.
2. **It calls three things that do not exist anywhere in Clawee**:
   `resolve_latest()` (a paginated GitHub `/releases` walker, defined inline
   in Burrowee's `tools/bootstrap.template.sh`, never spliced as a module —
   see its "BEGIN release-resolver" block), `$CONSOLE_URL` (Burrowee's
   console base for the catalog fallback), and `assert_version_floor()` (the
   "BEGIN version-floor" block, which reads `$MIN_VERSION`). None of these
   three has a Clawee equivalent to call. Splicing the module in would still
   fail even with `$COMP`'s case fixed — every one of these three names would
   resolve to "command not found" / an unbound variable at runtime.
3. Clawee's own resolver already does its own anti-rollback ordering that has
   no shared-module equivalent: it tries the public
   `downloads.clawee.org/<comp>/latest.json` (no auth, same first-party
   domain) *before* the third-party `GH_PROXIES` mirrors, specifically so a
   GitHub-blocking on-path attacker cannot steer resolution to a stale
   third-party mirror. The shared module's console-catalog step is a
   different, Burrowee-console-specific answer with no Clawee counterpart.

Taken together: adopting this module is not a splice-and-diff the way the
other eight were. It would require rewriting the module's component
dispatch, writing three new helper functions Clawee has no use for anywhere
else, and adding a `@MIN_VERSION@` bake to the generator — a real feature
addition, not an adoption. That is out of scope for Task 10, so it is
recorded as LOCAL FORK and Clawee's own version-resolution block is left in
place untouched.

## Sync verdict

`tools/sync-modules.sh` was run against the Burrowee worktree after
adoption; see `task-10-report.md` for the actual output. Every **adopted**
module (the eight above) reports `ok` — Clawee's copies match Burrowee's
authored module text byte-for-byte, confirmed by the matching
`tools/modules/MODULES.lock` entries. `download` and `version-resolve` are
not part of that comparison in the sense of "must match" — they are the two
modules Clawee does not include in its generated output at all (their
`.sh` files still exist under `tools/modules/` for `lock-modules.sh` and
future re-evaluation, but no `@INCLUDE:` line in Clawee's template ever
references them).
