# PORTING.md — burrowee release channel → clawee parameterization map

**DELETE THIS FILE AT ACTIVATION (B6).** It documents how this repo was cloned
from `burrowee-git/release` and parameterized for clawee, so every divergence is
auditable. Source of the clone: `/Volumes/MacintoshED/Workstation/Coding/Burrowee/release/code/release/`.

The clone is **structural** — same two-tier signed-installer model (outer
bootstrap = trust anchor with the minisign pubkey baked in; inner installer
ships inside the verified zip), same `version.sh`/`build.sh`/`gen-bootstraps.sh`/
`release.sh`/`prune-releases.sh`/`test-e2e.sh`/`verify-no-env.sh` toolchain, same
`v<X.Y.Z>.<YYYY>.<MM>.<DD>.<sha8>` stamp, same `shasum`/`sha256sum` portability,
same minisign-over-SHA256SUMS chain.

## 1. Literal substitutions (every burrowee token dispositioned)

| burrowee literal | clawee value | Where |
|---|---|---|
| `release.burrowee.com` (the channel host) | `release.clawee.org` | bootstrap.template.sh, release.sh, ops/*, site, README |
| `burrowee-git/release` (repo slug) | `clawee-git/release` | release.sh, prune-releases.sh, bootstrap.template.sh, README |
| `burrowee-git` (gh account) | `clawee-git` | release.sh, README, ops |
| `burrowee-release.pub` | `clawee-release.pub` | release.sh, gen-bootstraps.sh, README, site, ops |
| `burrowee-release.key.age` (sealed secret) | `clawee-release.key.age` | release.sh, release.dp |
| `~/.age/burrowee-release.txt` (age identity) | `~/.age/clawee-release.txt` | release.sh |
| `burrowee-release-key.XXXXXX` (mktemp) | `clawee-release-key.XXXXXX` | release.sh |
| env prefix `BURROWEE_*` | `CLAWEE_*` | all (DL_BASE, RELEASE_REPO, RELEASE_YES, PUBKEY_FILE, UNINSTALL, `<COMP>_VERSION`, SRC_*) |
| `STATIC_DIR=/ebs_storage/apps/release.burrowee.com/static` | `/ebs_storage/apps/release.clawee.org/static` | release.sh, ops |
| `ops/nginx/release.burrowee.com.conf` | `ops/nginx/release.clawee.org.conf` | filename + body |
| TEMP-pubkey placeholder note (`PHASE5A_OR_A2`) | `B5_OR_ACTIVATION` | gen-bootstraps.sh |
| user-facing "burrowee" prose | "clawee" | README, site, ops, script headers |

## 2. Components — TWO, not burrowee's three

| burrowee | clawee | binaries | source repo / package |
|---|---|---|---|
| `cli` | `claweev2` | `claweev2` | `clawee-git/cli` → `./cmd/clawee` (binary renamed claweev2) |
| `gateway` | `claweed` | `claweed`, `clawee-spawn` | `clawee-git/daemon` → `./cmd/claweed` + `./cmd/clawee-spawn` |
| `edge` | — (dropped) | — | clawee has no self-hosted relay component |

- Component name `claweev2` (binary, URL path, release tag, version-pin env), NOT
  `clawee` — the legacy v5 `clawee` binary keeps that name and coexists. URL is
  `release.clawee.org/claweev2/install.sh`. (Spec component table updated
  2026-06-12; the plan predates the rename and says `clawee` — superseded.)
- `versions/{claweev2,claweed}` start at `0.1.1` (matching the cli + daemon repo
  `VERSION` files), not burrowee's per-component numbers.

## 3. Dropped burrowee-isms

| Dropped | Why |
|---|---|
| **dispatcher** (`burrowee` binary built once + bundled into every zip; `versions/burrowee`, `SRC_DISPATCHER`, `build_dispatcher`, the `MAP="burrowee:."` build case, the `bundle burrowee into each zip` assembly step) | clawee zips contain ONLY the component binaries; no universal entry dispatcher. |
| **`config/console-pub.hex`** + all edge console-signing logic (`console_pub_hex`, `CONSOLE_PUB_HEX`, `consolePubHexProd`, `BURROWEE_CONSOLE_PUB`/`BURROWEE_CLOUD_PUB`, the placeholder reject) | edge-specific; clawee has no edge/console-signing dependency. `config/` dir removed. |
| **edge skills sync** (`EDGE_SKILLS_SRC`, the `skills/` mirror + scp) | skills are out of Phase B scope (component-repo-owned; not in the spec's component table). |
| **`skills/` directory** (`burrowee-*-install`/`-setup` SKILL.md) | out of scope (see above). Not cloned. |
| **`verify-no-env` forbidden list** `BURROWEE_RELAY_WS\|mustEnv\|BURROWEE_GW_` | replaced with clawee's config-env names `CLAWEE_DATA_DIR\|CLAWEE_SOCKET\|CLAWEE_SPAWN_HELPER\|mustEnv` (matches the daemon's own build-local.sh gate). |

## 4. Preserved burrowee literals (the cross-channel dependency — DO NOT rename)

clawee depends on burrowee's binaries, installed from **burrowee's own public
channel**. These stay as-is, intentionally:

| Preserved literal | Where | Why |
|---|---|---|
| `https://release.burrowee.com/cli/install.sh` | inner/claweev2 dependency step | claweev2 needs `burrowee-cli` as its transport; installed from burrowee's channel. |
| `https://release.burrowee.com/gateway/install.sh` | inner/claweed (daemon installer) | claweed needs `burrowee-gateway`; installed from burrowee's channel. |
| `burrowee-cli`, `burrowee-gateway` (binary names) | dependency `--version` checks | the actual burrowee binaries clawee dials/registers against. |

The dependency steps deliberately do NOT minisign-verify the fetched burrowee
bootstrap: the burrowee bootstrap is its OWN minisign trust-anchor. Transport is
pinned (`--proto '=https' --tlsv1.2`); never downgrades an existing install.

## 5. The claweed inner installer is the daemon's canonical template

burrowee authored each inner installer inside the release repo. For claweed we do
**not** fork: the canonical sudo-minimal installer lives in `clawee-git/daemon`
at `install/install.sh.in` (with `install/build-local.sh` as its local-source
generator). `release.sh` renders it per-build (`sed __CLAWEED_VERSION__ →
<stamp>`) so the served installer can never drift from source.

- `inner/claweed/install.sh` here is a committed render (placeholder version
  banner) kept current for shellcheck + reference only. The release zip always
  carries the freshly-rendered daemon template.
- **Cosmetic note (intentional, not a bug):** the daemon template's header still
  reads "LOCAL-SOURCE variant … sourced from this staged directory instead of a
  signed GitHub release." In the release-channel path the binaries DO come from
  the verified release zip — but post-unzip they sit "in this staged directory"
  exactly as the header describes, so the body is correct verbatim. The daemon
  repo owns that wording; we render, never edit.
- `claweev2` keeps a fresh, repo-committed inner installer (simple bin-placer +
  the burrowee-cli dependency step) — there is no daemon-style template for it.

## 6. Outer-bootstrap inner-exec contract (component-aware)

burrowee's outer execs every inner identically (`PREFIX=… BURROWEE_UNINSTALL=…
sh ./install.sh`). The two clawee inners have different contracts, so the outer
branches on `$COMP`:

- `claweev2` — `PREFIX="$PREFIX" CLAWEE_UNINSTALL="…" sh ./install.sh` (simple).
- `claweed` — `CLAWEE_PREFIX="$PREFIX/bin" sh ./install.sh` (the daemon installer
  reads `CLAWEE_PREFIX`, runs interactively, takes `uninstall`/`--yes` as args;
  uninstall is done by running the inner directly, not via the bootstrap).

## 7. Test signing key

Matches burrowee's pattern: `tools/testkeys/test.pub` committed, `test.key`
gitignored. Regenerated fresh for clawee (`minisign -G -W`, no-password) — NOT
copied from burrowee. `test-e2e.sh` signs dry-run builds with it offline; the
real release key (B5) is operator-generated and lives age-sealed in
`clawee-git/release.dp`.
