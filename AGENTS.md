# AGENTS.md — clawee-git/release

The project manual for the Clawee release kit. Global policy lives in the
shared playbook (`~/.agents/`); this file carries only what is true of **this**
repo. Where the two disagree, an override is named here with its reason.

Load `~/.agents/guidelines/release.md` and
`~/.agents/guidelines/release-management.md` before touching the cut.

---

## What this repo is

The public face of the Clawee install channel, and the kit that produces it.
The component sources are private (`clawee-git/cli`, `clawee-git/daemon`); this
repo builds them, signs the result, and owns everything downstream of that.

It is **not** on the `dev` spine. `main` is the trunk — this is a release repo,
the record of cuts, so branch-and-PR is for reviewable changes to the kit
itself, not for the cuts it records.

## Stack

| | |
|---|---|
| Cut orchestration | `bash` — `tools/release.sh` is the entry point |
| Build / assemble | Go — `cmd/rkit` (module `github.com/clawee-git/release`) |
| Bucket uploads | Go — `tools/r2-mirror` (its own module, `clawee-release-r2-mirror`) |
| Catalog registration | Go — `cmd/clawee-release-register` + `internal/register` |
| Signing | `minisign` (release key), `modernech-sign` + `rcodesign` (Apple) |
| Secrets | age-sealed, in the sibling `release.dp` repo |

Two Go modules, so `go vet ./... && go test ./...` runs **twice**: once at the
repo root and once in `tools/r2-mirror/`.

## The cut chain

A cut does **not** publish. It stages privately and registers a row; going live
is **promote**, an operator act in the manage service.

```
tools/release.sh <clawee|claweed|all> [--channel stable|beta] [--public] [--dry-run]

  1. stamp        versions/<comp> + the source HEAD sha  (tools/version.sh)
  2. build        darwin/linux × arm64/amd64, assembled and zipped
  3. sums         a sorted SHA256SUMS.txt over the four zips
  4. sign         minisign over the sums (real key, or the TEST key on --dry-run)
  5. stage        → PRIVATE staging bucket, <comp>/<channel>/<stamp>/, NO manifest
  6. register     nonce → sign the row with the release key → POST; row is `staged`
  7. record       regenerate the bootstraps; commit "[RELEASED: <comp>] <date> <stamp> (staged)"
```

`tools/release.sh --distribute-only <comp> <stamp>` runs steps 5–7 over an
existing `dist/<stamp>/`. It is both the `rkit build` follow-on and the **re-run
path** after a failed upload or registration.

**Nothing in this script publishes.** No release tag, no GitHub Release, no
public-bucket write, no `latest.json`, no `scp`, no retention pass. Those belong
to promote. `tools/test-cut-no-publish.sh` enforces it.

### The two refusals, and why they sit where they do

- **The manage URL is checked BEFORE the upload.** Staged bytes with no catalog
  row are a stranded artifact: nothing lists them, nothing can promote them. The
  cheap moment to say so is before they exist.
- **A registration failure fails the cut AFTER the upload.** The bytes are
  already there and are inert — the thing to surface is the missing row, not the
  objects.

### `--channel` and the cut origin

The channel and the branch are one decision, not two:

| Channel | Cut origin |
|---|---|
| `stable` | `main` |
| `beta` | `beta` — the permanent cycle branch — or a `beta-<slug>` experiment beside it |

The branch decides when `--channel` is absent. A flag that disagrees with the
branch is **refused** (retention and every installer read the channel, not the
branch, so a mislabelled row is a beta build treated as stable), and a branch
that maps to neither is no cut origin at all — a release is published from a
decided branch, never from wherever the tree happens to be.

**Both the component source worktree and this repo answer to the rule.** They
are inputs to one artifact; a stable kit cutting a beta component is the same
mislabelling from the other side.

**This repo carries a permanent `beta` branch of its own** — it is one of the
participating repos, and `beta` is never deleted at release
(`~/.agents/guidelines/beta.md`). So a cut is a two-sided checkout:

| Cutting | This repo on | Component source on |
|---|---|---|
| stable | `main` | `main` |
| beta | `beta` (or a `beta-<slug>` experiment beside it) | its `beta` |

A beta cut is therefore a real, reachable operation: check this repo out on
`beta`, the component source on its `beta`, and cut. It is not a flag layered
over a stable checkout.

**A project worktree can never cut.** Its branch (`<date>-<slug>`) maps to no
channel, so the release-repo guard refuses before anything is built — which is
correct, and is why kit changes are developed here and cut from `main` or
`beta`.

Dry-runs are lenient about all of it, so they still run off a prep worktree.

`main` was previously the only accepted branch, full stop, which made the beta
path unreachable: the channel could be derived and the flag accepted, but the
only branch either could have come from was rejected. A cycle changes what feeds
the cut, not whether there is one (`~/.agents/guidelines/beta.md` §2/§4).

## Seams

Every remote store is behind a flag, so no test ever reaches a real bucket,
GitHub, or host.

| Seam | Where | What it isolates |
|---|---|---|
| `--bucket` | `tools/r2-mirror` | which bucket — the cut names the private one, promote the public one |
| `--prefix` | `tools/r2-mirror` | the channel segment in the key; empty = the flat public layout |
| `--no-manifest` | `tools/r2-mirror` | **the go-live.** A manifest write is what makes a release reachable; the cut withholds it |
| `--dry-run` | both Go tools and `release.sh` | prints the staging keys and the register payload, touches nothing |
| `Client.HTTP` | `internal/register` | the manage service — the tests run the real handshake against a fake server |
| `GO_BIN` | `release.sh` | the toolchain, for harness sessions whose PATH lacks `go` |

## The manage service

`cmd/clawee-release-manage` is the other half of the cut: the kit stages
privately and registers a row, and this service is what an operator promotes it
from (`~/.agents/guidelines/release-management.md`). It is a single Go binary
with a SQLite catalog and server-rendered pages — no SPA build, no second
runtime.

```
clawee-release-manage serve --data-dir <dir> --base-url https://<host> [--listen 127.0.0.1:8787]
clawee-release-manage admin add|list|remove <name> --data-dir <dir>
clawee-release-manage version [--data-dir <dir>]
clawee-release-manage docs > docs/cli-help.md
```

`docs/cli-help.md` is generated, and a suite test byte-diffs it — regenerate it
in the same change as any surface move (`~/.agents/guidelines/cli-help.md`).

| Package | Owns |
|---|---|
| `internal/manage/catalog` | the closed vocabulary: components, channels, states, stamp shapes. The router derives its per-channel paths from `Channels` |
| `internal/manage/store` | the SQLite catalog: rows, admins, sessions, CSRF, nonces, invites. Clock-free — every method takes `now` from the caller |
| `internal/manage/totp` | RFC 6238, pinned to the official vectors. Ported from the console's `internal/console/totp` |
| `internal/manage/auth` | password (argon2id), sealed TOTP secrets, sessions, CSRF, login rate limiting |
| `internal/manage/intake` | the nonce and register endpoints; verifies against the baked `clawee-release.pub` |
| `internal/manage/web` | routing split, read API, pages |

**Ported from, not imported:** the surface follows
`Burrowee/console/code/main/internal/console/` — `release/channel.go`,
`release/download_guard.go`, `release/r2_mirror.go`, `release/github_publish.go`,
`store/release_version.go`, `totp/`, `grant/`, and that repo's release-versions
retention and promote/R2-sync design specs. Read those before extending this.

### Roots and secrets

`--data-dir` has **no default and is never read from the environment**: it holds
the catalog and the service's root secret, and a guessed root is either a second
empty catalog or a write into another deployment's
(`~/.agents/guidelines/privilege.md`). The secret key (`secret.key`, mode 0600)
is validated at its own writer — absolute, clean, `O_NOFOLLOW`, refused at any
mode another user can read. It seals enrolled TOTP secrets, so a copy of the
catalog file alone is inert.

Login attempts are rate-limited per **(stage, client IP, account)** and the
counters live in the catalog (`login_failures`), not in process memory — an
unpersisted limit is one a restart erases, so a guessing run resumes across a
deploy. The two stages are counted **separately** and only a correct code
clears the code counter: a password must never vouch for the factor it is gated
by. `ClientIP` reads `RemoteAddr` and never `X-Forwarded-For`, so behind the
host's proxy the limit is effectively per-account; a deployment that needs
per-client limits must answer the trusted-proxy question explicitly.

Cookies are marked `Secure` iff `--base-url` is https; `--listen` defaults to
loopback because the service belongs behind the host's TLS proxy
(`ops/nginx/`).

### Seams batch B must implement, by name

Promote, yank, mint and the invite listing are routed today and answer **501**,
already session- and CSRF-gated, so the pages are final and batch B fills in
bodies rather than adding surface:

| Seam | Where the stub is | What batch B owns |
|---|---|---|
| object store interface | not yet declared | copy staged → public, presign, prune; every remote store behind a flag, as `tools/r2-mirror` already is |
| GitHub publisher interface | not yet declared | create the release/pre-release and upload assets; promote **fails closed** without it |
| `PATCH /api/v1/manage/releases/{id}` | `web.handleNotImplementedAPI` | promote and yank, in the guideline's order: verify → copy → GitHub → manifest → flip (`store.Promote`) → retention |
| `POST /manage/releases/{id}/promote\|yank\|mint` | `web.handleNotImplementedPage` | the page-form twins of the above |
| `POST /api/v1/manage/releases/{channel}/install-url` | `web.handleNotImplementedAPI` | the 48 h invite: presigned URLs, rendered `install.sh`, `store.CreateInvite` |
| `GET /api/v1/manage/releases/{channel}/invites` | `web.handleNotImplementedAPI` | the JSON twin of the invites page |
| retention | `store.ExpireOldVersions` (done) | calling it at the end of promote, 10 stable / 1 beta, plus the byte pruning on both surfaces |
| download guard | not yet declared | https-only, no userinfo or IP literal, per-hop, dial the classified address — the service's only outbound fetch |

`store.Yank` already returns the successor row, which is what the caller
re-points the manifest at (or removes it when there is none).

## Sealed config

Read through `CLAWEE_R2_CONFIG` (default `~/.clawee/release/config.toml`), which
is **outside any repo**. Overrides in parentheses.

| Key | Value | Override |
|---|---|---|
| `r2_account_id` | the Cloudflare account | — |
| `staging_bucket` | `<staging bucket>` — PRIVATE, never public | `CLAWEE_R2_STAGING_BUCKET` |
| `manage_url` | `<manage URL>` — the manage service base URL | `CLAWEE_MANAGE_URL` |

Credentials (`access_key_id`, `secret_access_key`) live in a separate file,
`CLAWEE_R2_CREDS` (default `~/.clawee/release/r2.key`). The signing key is
age-sealed in `release.dp` and decrypted to a mode-600 tmpfile that is
overwritten and unlinked on exit.

**There is deliberately no default staging bucket.** A guessed bucket name is
either a failed upload or a write to the public one, and the second publishes a
build nobody approved.

**No live bucket names, hosts, URLs or tokens in markdown** — placeholders only,
here and in `README.md`. The real values are in the sealed config
(`~/.agents/guidelines/secrets.md`).

## Registration is machine-authentication

No admin credential lives in a release kit. The service issues a single-use
nonce; the kit signs the **whole row, nonce included**, with the same Ed25519
key that signs `SHA256SUMS.txt`; the service verifies against the public half it
already bakes in. Because the nonce is a field of the signed body rather than a
header, one signature buys exactly one row.

The Ed25519 key is read straight out of the minisign secret-key file, which must
be **password-less** (`minisign -G -W`) — the cut signs non-interactively, so a
password-protected key is a configuration error, named as such.

## Tests

```sh
# Go, both modules
go vet ./...  && go test ./...
( cd tools/r2-mirror && go vet ./... && go test ./... )

# shell suites — run them all
for t in tools/test-*.sh tools/*.test.sh; do bash "$t"; done
```

| Suite | What it pins |
|---|---|
| `tools/test-cut-no-publish.sh` | the cut publishes nothing — a static scan of `release.sh` **and** a run of the stage half under a stubbed, logging PATH |
| `tools/test-stage-fail-closed.sh` | every unresolved staging config is a refusal, never a skip |
| `tools/test-e2e.sh` | the `--dry-run` cut end to end |
| `tools/test-modules.sh` | the bootstrap trust chain: lock integrity, `# needs:` ordering, committed bootstraps match the generator |
| `tools/test-checksum-verify.sh` | the shipped verify block against a stub pre-2016 `shasum` |
| `tools/module_gate.test.sh` | the gate `release.sh` runs before the first build |

`test-e2e.sh` and `verify-no-env.test.sh` need the component source worktrees on
disk; they fail with a "source worktree missing" message on a machine that has
none, which is a missing checkout, not a regression.

## Conventions

- **`go run`, not an installed binary.** `release.sh` invokes both Go tools
  through `${GO_BIN} run`, so a cut can never use a stale build of its own kit.
- **Shell functions are extracted by tests, not copied into them.** The suites
  `awk` a function verbatim out of `release.sh`, which breaks the moment its
  shape changes instead of testing a stale fork. That relies on each function's
  closing `}` being alone in column 0 — keep it that way.
- **Comments say why.** This kit's failure modes are all "it looked like it
  worked": a skipped upload, a manifest written from the wrong path, a signature
  over reordered fields. Where a rule exists because something went wrong, the
  incident is in the comment.
- `Payload`'s field order in `internal/register` is the **wire contract** — the
  signature covers the canonical JSON in declaration order. Reordering silently
  invalidates every signature.
