# Clawee release channel

Public, signed, self-service install channel for the Clawee terminal client and
its PTY daemon. Every download is verified end-to-end (minisign signature →
SHA-256 → unzip → exec a verified inner installer).

Two components are published here:

| Component | Binaries | What it is | Cross-channel dependency |
|---|---|---|---|
| `clawee` | `clawee`, `clawee-updater` | the Clawee terminal client | `burrowee-cli` (from `release.burrowee.com/cli`) when missing |
| `claweed` | `claweed`, `claweed-updater` | the PTY daemon + its self-updater | `burrowee-gateway` (from `release.burrowee.com/gateway`) when missing/older |

There is **no universal dispatcher** — clawee's binaries are invoked directly.

## Install

```sh
# Client
curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/clawee/install.sh | sh
# Daemon (run AS YOUR USER — it escalates with sudo only for the steps that need root)
curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/claweed/install.sh | sh
```

While a beta cycle is open, each component also serves a **beta twin** —
`<comp>/beta.install.sh`, resolving the beta channel manifest instead of the
stable one. The site offers that line **only while a beta is actually
promoted**, because a beta command that outlives its cycle installs the last
beta forever after it graduated.

`https://release.clawee.org` is the manage service's public face, rendered from
the promoted catalog: `/` (install), `/downloads` (per-platform zips, checksums
and signatures per promoted release, both channels), `/verify`, `/platforms` and
`/docs`. Nothing `staged` appears on any of them — a staged cut is a build no
operator has approved.

Each installer detects your OS/arch, resolves the latest published release for
that component, downloads the zip + `SHA256SUMS.txt` + `SHA256SUMS.txt.minisig`,
**verifies the minisign signature against the baked public key**, checks the
SHA-256, then unzips and runs the inner installer.
If `minisign` is missing, the installer provides it first — through your package
manager where the installer has root (or, for a user-level install, passwordless
sudo), otherwise the official upstream 0.12 build whose SHA-256 is pinned inside
the installer itself and whose own signature is then checked against upstream's
key — and refuses to continue if neither works; it never runs an unverified
verifier.
An uninstall never touches your package manager; if it had to fetch the pinned
build to verify its payload, that single `minisign` file stays in the bin
directory afterwards.

- **clawee** lands in `$HOME/.local/bin` (override with `PREFIX`), then ensures
  `burrowee-cli` is present (installed from burrowee's public channel if missing).
- **claweed** is the canonical sudo-minimal daemon installer. Run it **as your
  user**, never under `sudo`: the data-dir, the `burrowee-gateway` dependency and
  the closing doctor run take **no** privilege. It escalates with `sudo` for
  four steps:

  1. the root-owned `claweed` + `claweed-updater` binaries in `/usr/local/bin`;
  2. the root-owned spawn *policy* files (which uids may host a session) — this
     step writes no binary;
  3. the system boot unit (`/Library/LaunchDaemons` on macOS,
     `/etc/systemd/system` on Linux), written only when its content changed;
  4. **macOS only** — a sudoers drop-in at
     `/etc/sudoers.d/clawee-powermetrics` (0440, root:wheel) that grants the
     user running the installer **passwordless `sudo`** for one fixed
     command: `/usr/bin/powermetrics --samplers thermal -n 1`. It is validated
     with `visudo -c` first and skipped if that fails, and it grants no shell
     and no script — but it is a standing NOPASSWD rule, so delete the file to
     revoke it. Without it claweed reports `thermal=unknown` and everything
     else still works.

  **Force this line's state migrations** with `upgrade.sh` instead of
  `install.sh` — same verified kit, plus a forced run of
  `migrations/upgrade.sh` after the install:

  ```sh
  curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/claweed/upgrade.sh | sh -s -- 0.1.15
  ```

  This is the exception path, not the routine upgrade: `install.sh` already
  runs the ladder gated on every install, and `<line>` is optional (omitted,
  the latest release is used). It exists for the case the gate cannot see —
  a build that changed without the semver changing. `clawee` (the terminal
  client) ships no migration ladder today; its `upgrade.sh` still renders (so
  the URL never 404s) and refuses at runtime, naming the component and the
  version it just installed.

  If no `sudo` path exists at all — no passwordless sudo **and** no tty to
  prompt on — the installer **stops at step 1 and installs nothing**: there is
  no unprivileged variant of the daemon binary. With a tty it prompts for your
  password. To uninstall, run the inner installer with `uninstall` (`--purge`
  also removes `~/.clawee/data`).

  **There is no setuid tier.** claweed used to ship a third, mode-4755
  root-gaining binary (`clawee-spawn`) that an unprivileged daemon execed to fork
  a tenant's session. The daemon runs as root itself now and forks its own
  per-user children, so the helper is retired: nothing builds it, nothing ships
  it, and the installer removes a leftover copy from hosts installed before the
  split.

## Verify by hand

The signing public key lives in this repo and is mirrored at
`https://release.clawee.org/clawee-release.pub`. To verify a download yourself:

```sh
minisign -V -P "$(cat clawee-release.pub | tail -n1)" \
  -m SHA256SUMS.txt -x SHA256SUMS.txt.minisig
f=<file>                                      # the file you downloaded
want=$(awk -v f="$f" '{ n = $2; sub(/^\*/, "", n); if (n == f) { print $1; exit } }' SHA256SUMS.txt)
got=$(shasum -a 256 "$f" | awk '{print $1}')  # sha256sum "$f" on Linux
if   [ -z "$want" ];        then echo "NO ENTRY for $f in SHA256SUMS.txt — do not install"
elif [ "$want" = "$got" ];  then echo "OK $f"
else                             echo "MISMATCH for $f — do not install"; fi
```

A failed signature check means the bytes are untrusted — do not install them.

The checksum block compares one digest by hand on purpose. `shasum -c
--ignore-missing` is what the installers used to run, and the stock `shasum` on
a pre-2016 macOS rejects that option outright — which read as "tampered". Its
obvious replacement is worse: `sha256sum -c` exits **0** on an empty or
malformed checklist, so a mistyped filename would report success having verified
nothing. Selecting the entry by exact name and shouting when there is none is
what the installer's own gate does.

## Pin a version

Each component reads a version-pin env var. The value is the release tag
(`<comp>/<stamp>`):

| Component | Env var |
|---|---|
| `clawee` | `CLAWEE_VERSION` |
| `claweed` | `CLAWEE_CLAWEED_VERSION` |

```sh
CLAWEE_VERSION=clawee/v0.1.15.2026.06.14.86f2a984 \
  curl -fsSL https://release.clawee.org/clawee/install.sh | sh
```

Unset → the installer resolves the newest release for that component.

## Supported platforms

| OS | arm64 | amd64 |
|---|---|---|
| macOS (darwin) | ✓ | ✓ |
| Linux | ✓ | ✓ |

Windows is not supported.

## How this repo is built

This is the public face of the channel. Built binaries for the private
component repos (`clawee-git/cli`, `clawee-git/daemon`) are published as
**GitHub Release assets on this repo** (the component sources are private and
can't be `curl`'d anonymously). `release.clawee.org` (nginx + Cloudflare) is the
manage service's public face; the bootstraps, the signing pubkey and the version
JSONP stay static files beside it, put there by `clawee-release-manage
publish-static` when the **kit** changes — not on every cut, because they embed
no version.

### A cut does not publish

Cutting a release and making it downloadable are **two separate acts**, and
only the first is automated:

```
tools/release.sh <comp>
  stamp → build → SHA256SUMS.txt → minisign
    → upload to the PRIVATE staging bucket at <comp>/<channel>/<stamp>/
    → register a `staged` catalog row with the manage service
    → regenerate the bootstraps + a "[RELEASED: <comp>] … (staged)" marker commit
```

That is the whole chain. A cut creates **no** release tag, **no** GitHub
Release, writes nothing to the public bucket, updates no `latest.json` and
copies nothing to the static host — so nothing anyone can download changes
when a release is cut.

**Promote** is the second act, taken by an operator in the manage service once
they have tested the staged build. It copies the same verified bytes to the
public surface, creates the GitHub Release, writes the channel manifest and
flips the row `public`. Promote is a copy, not a rebuild: the bytes an operator
tested are the bytes that go live, which is why a cut signs and notarizes in
full even though nothing is published yet.

`--channel stable|beta` files the row. It defaults to `beta` when the component
source is on a beta branch and `stable` otherwise; asking for `--channel
stable` from a beta branch is refused rather than filing beta bytes as stable.

A cut refuses **before** uploading when the manage URL is unconfigured, and
fails **after** uploading when the service refuses the registration — staged
bytes with no catalog row are a stranded artifact nobody can find or promote.
The two configuration keys it needs (`<staging bucket>`, `<manage URL>`) live
in the sealed config; see `AGENTS.md`.

```
clawee/    claweed/        ← per-component outer bootstraps, generated: install.sh
                              + upgrade.sh (stable) and their beta.* twins, plus
                              version.js + beta.version.js (the site's badge JSONP)
inner/clawee/install.sh     ← clawee's inner installer, repo-committed (ships
                              verbatim inside each verified clawee zip). claweed
                              has no committed copy: its inner installer is
                              rendered at build time from the daemon repo's
                              install/install.sh.in
versions/<comp>             ← per-component SemVer source of truth
internal/manage/web/templates/public/
                            ← release.clawee.org's pages, rendered from the
                              promoted catalog (there is no static index.html
                              any more)
tools/                      ← version.sh, build.sh, gen-bootstraps.sh, release.sh,
                              prune-releases.sh, test-e2e.sh, verify-no-env.sh,
                              test-stage-fail-closed.sh, test-cut-no-publish.sh,
                              test-upgrade-bootstrap.sh
tools/r2-mirror/             ← uploads a dist/<stamp>/ to a bucket. --prefix puts
                              the channel in the key, --no-manifest withholds
                              latest.json: the cut uses both, promote uses
                              neither
cmd/clawee-release-register/ ← registers the `staged` catalog row (nonce → sign
                              with the release key → POST). internal/register/
                              holds the payload, the key reader and the client
tools/test-cut-no-publish.sh ← proves the cut publishes nothing: a static scan
                              for publish verbs plus a run of the stage half
                              under a stubbed PATH that logs every command
tools/modules/               ← shared bootstrap trust-chain modules (Task 10,
                              2026-08-25): SHARED WITH BURROWEE, spliced into
                              tools/bootstrap.template.sh at generation time by
                              gen-bootstraps.sh's expand_includes (never at
                              runtime). Lock-gated: tools/modules/MODULES.lock
                              pins each module's version + sha256, enforced by
                              tools/test-modules.sh; see
                              docs/adoption-2026-08-25-bootstrap-modules.md for
                              which modules Clawee adopted vs. forked.
tools/lock-modules.sh        ← rewrites tools/modules/MODULES.lock from the
                              modules on disk
tools/sync-modules.sh        ← compares tools/modules/ against another
                              product's (e.g. Burrowee's) and copies in newer
                              ones; tools/sync-modules.test.sh covers its four
                              verdicts
tools/test-modules.sh        ← the module gates: lock integrity, `# needs:`
                              ordering, and that committed bootstraps match
                              what gen-bootstraps.sh would (re)generate
tools/test-checksum-verify.sh ← drives the shipped verify-checksum block
                              against a stub pre-2016 shasum (no
                              --ignore-missing) for both clawee and claweed
clawee-release.pub          ← minisign signing public key (added at activation)
```

The `claweed` inner installer is the **canonical** sudo-minimal installer that
lives in `clawee-git/daemon` at `install/install.sh.in`; `release.sh` renders it
per-build (substituting the version stamp) so the served installer can never
drift from source. This repo keeps **no** copy of it: one existed under
`inner/claweed/`, described as "kept current for shellcheck + reference", but
nothing here could enforce that — the canonical file lives in a private repo —
and it had silently fallen 600+ lines behind, still documenting a setuid tier
that no longer exists. Read the canonical file, or a built zip.

- `clawee-git/release` (PUBLIC). Trunk: `main`.

## Status

Preview release. Expect rough edges; report issues on this repo.
