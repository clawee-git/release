# Clawee release channel

Public, signed, self-service install channel for the Clawee terminal client and
its PTY daemon. Every download is verified end-to-end (minisign signature →
SHA-256 → unzip → exec a verified inner installer).

Two components are published here:

| Component | Binaries | What it is | Cross-channel dependency |
|---|---|---|---|
| `claweev2` | `claweev2` | the Clawee terminal client | `burrowee-cli` (from `release.burrowee.com/cli`) when missing |
| `claweed` | `claweed`, `clawee-spawn` | the PTY daemon + setuid spawn helper | `burrowee-gateway` (from `release.burrowee.com/gateway`) when missing/older |

There is **no universal dispatcher** — clawee's binaries are invoked directly.

## Install

```sh
# Client
curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/claweev2/install.sh | sh
# Daemon (run AS YOUR USER — it escalates with sudo only for the setuid spawn helper)
curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/claweed/install.sh | sh
```

Each installer detects your OS/arch, resolves the latest published release for
that component, downloads the zip + `SHA256SUMS.txt` + `SHA256SUMS.txt.minisig`,
**verifies the minisign signature against the baked public key**, checks the
SHA-256, then unzips and runs the inner installer.

- **claweev2** lands in `$HOME/.local/bin` (override with `PREFIX`), then ensures
  `burrowee-cli` is present (installed from burrowee's public channel if missing).
- **claweed** is the canonical sudo-minimal daemon installer: it installs
  `claweed` to a user-writable prefix + boot unit in your own user domain with
  **no** privilege, escalates with `sudo` for exactly one tier (the setuid-root
  `clawee-spawn` + its root-owned allowlist), and cross-installs
  `burrowee-gateway`. With no passwordless sudo it prints the exact Tier-S block
  and exits non-zero so you know the spawn helper isn't installed yet. To
  uninstall, run the inner installer with `uninstall` (`--purge` also removes
  `~/.clawee/data`).

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
| `claweev2` | `CLAWEE_CLAWEEV2_VERSION` |
| `claweed` | `CLAWEE_CLAWEED_VERSION` |

```sh
CLAWEE_CLAWEEV2_VERSION=claweev2/v0.1.1.2026.06.13.86f2a984 \
  curl -fsSL https://release.clawee.org/claweev2/install.sh | sh
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
claweev2/  claweed/        ← per-component outer bootstrap (install.sh, generated)
inner/<comp>/install.sh     ← inner installer (ships inside each verified zip)
                              claweev2: repo-committed; claweed: rendered at build
                              time from the daemon repo's install/install.sh.in
versions/<comp>             ← per-component SemVer source of truth
site/index.html             ← release.clawee.org landing page
tools/                      ← version.sh, build.sh, gen-bootstraps.sh, release.sh,
                              prune-releases.sh, test-e2e.sh, verify-no-env.sh
clawee-release.pub          ← minisign signing public key (added at activation)
```

The `claweed` inner installer is the **canonical** sudo-minimal installer that
lives in `clawee-git/daemon` at `install/install.sh.in`; `release.sh` renders it
per-build (substituting the version stamp) so the served installer can never
drift from source. `inner/claweed/install.sh` here is a committed render kept
current for shellcheck + reference only.

- `clawee-git/release` (PUBLIC). Trunk: `main`. gh.account: `clawee-git`.
- Call gh via `~/.claude/bin/ghp`, never bare `gh`.

## Status

Preview release. Expect rough edges; report issues on this repo.
