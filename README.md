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

Each installer detects your OS/arch, resolves the latest published release for
that component, downloads the zip + `SHA256SUMS.txt` + `SHA256SUMS.txt.minisig`,
**verifies the minisign signature against the baked public key**, checks the
SHA-256, then unzips and runs the inner installer.

- **clawee** lands in `$HOME/.local/bin` (override with `PREFIX`), then ensures
  `burrowee-cli` is present (installed from burrowee's public channel if missing).
- **claweed** is the canonical sudo-minimal daemon installer. Run it **as your
  user**, never under `sudo`: the data-dir, the `burrowee-gateway` dependency and
  the closing doctor run take **no** privilege. It escalates with `sudo` for
  three steps only — the root-owned `claweed` + `claweed-updater` binaries in
  `/usr/local/bin`, the root-owned spawn *policy* files, and the system boot
  unit. Without sudo the last two degrade to a printed block you can run by
  hand. To uninstall, run the inner installer with `uninstall` (`--purge` also
  removes `~/.clawee/data`).

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
shasum -a 256 -c --ignore-missing SHA256SUMS.txt   # or sha256sum on Linux
```

A failed signature check means the bytes are untrusted — do not install them.

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
can't be `curl`'d anonymously). The static bootstrap scripts are mirrored to
`release.clawee.org` (nginx + Cloudflare).

```
clawee/    claweed/        ← per-component outer bootstrap (install.sh, generated)
inner/clawee/install.sh     ← clawee's inner installer, repo-committed (ships
                              verbatim inside each verified clawee zip). claweed
                              has no committed copy: its inner installer is
                              rendered at build time from the daemon repo's
                              install/install.sh.in
versions/<comp>             ← per-component SemVer source of truth
site/index.html             ← release.clawee.org landing page
tools/                      ← version.sh, build.sh, gen-bootstraps.sh, release.sh,
                              prune-releases.sh, test-e2e.sh, verify-no-env.sh
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

- `clawee-git/release` (PUBLIC). Trunk: `main`. gh.account: `clawee-git`.
- Call gh via `~/bin/ghp`, never bare `gh`.

## Status

Preview release. Expect rough edges; report issues on this repo.
