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

**One Go module.** `tools/r2-mirror` had its own until the manage service
needed the same S3/SigV4 client and the same `latest.json` schema: two modules
meant either a `replace` directive or a second copy of the signing code, and a
second copy of a signer is a signer that drifts. The shared pieces are now
`internal/r2` (client, presigning, creds) and `internal/manifest` (the channel
manifest). `go vet ./... && go test ./...` at the repo root covers everything.

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
clawee-release-manage publish-static --root <kit> --dest <[user@host:]dir> [--dry-run]
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
| `internal/manage/web` | routing split, read API, operator pages, and the PUBLIC pages (`internal/manage/web/public.go`) |
| `internal/staticsurface` | the one list of files the host still serves as static bytes — read by `publish-static` and by the site's link check |

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

### Operator commands

```sh
# provision an account; the second factor enrols itself at first login
clawee-release-manage admin add <name> --data-dir <DATA_DIR>
clawee-release-manage admin list --data-dir <DATA_DIR>

# remove an account: its sessions and CSRF tokens cascade away with it.
# There is no re-enrolment path — resetting someone's second factor means
# removing the account and adding it again.
clawee-release-manage admin remove <name> --data-dir <DATA_DIR>

# run the service
clawee-release-manage serve --data-dir <DATA_DIR> --base-url https://<MANAGE_HOST> \
    --r2-account <R2_ACCOUNT> --r2-creds <R2_CREDS> \
    --staging-bucket <STAGING_BUCKET> --public-bucket <PUBLIC_BUCKET> \
    --github-repo <ORG>/<REPO> --github-token-file <GH_TOKEN_FILE>

# the nightly retention net (a timer runs this)
clawee-release-manage retain --data-dir <DATA_DIR> <the same store flags>

# what a real pass WOULD expire. Touches neither the buckets nor the catalog,
# so it is safe — and useful — before the stores are wired.
clawee-release-manage retain --data-dir <DATA_DIR> --dry-run
```

**The real `retain` pass REFUSES without a public store and a GitHub
publisher.** Expiring a row is one-way — a later pass only ever sees rows it
expires itself — so marking rows it cannot prune would orphan their bytes
permanently. Use `--dry-run` until the stores are wired.

Placeholders only, here and everywhere else in this repo's markdown: the real
account, buckets, host and token path live in the sealed `release.dp` config
(`~/.agents/guidelines/secrets.md`).

| Flag | What it names |
|---|---|
| `--data-dir` | the catalog (`catalog.db`) and, by default, the service secret key. **No default, never read from the environment** |
| `--secret-key` | the key that seals enrolled TOTP secrets; defaults to `secret.key` inside `--data-dir`. Created 0600 on first run, and refused at any mode another user can read or through a symlink. It is **not** in the catalog on purpose: a copy of the database alone is inert. Replacing it invalidates every enrolled second factor |
| `--base-url` | the public URL; also decides whether cookies are marked `Secure` |
| `--listen` | default `127.0.0.1:8787` — loopback, because the service belongs behind the host's TLS proxy (`ops/nginx/`) |
| `--r2-account`, `--r2-creds` | the Cloudflare account and the file holding `access_key_id` / `secret_access_key` |
| `--staging-bucket` | the PRIVATE bucket a cut uploads to. Read, presign, and one write (the invite script) |
| `--public-bucket` | what installers read. Refused if it equals the staging bucket |
| `--github-repo`, `--github-token-file` | the release listing; **promote fails closed without them**. The repo half also gives the download page its GitHub release links |
| `--public-base-url` | the URL the public bucket is served at. The download page links into its channel layout; without it the page renders with no download links rather than with guessed ones |

**Every seam is optional and a missing one refuses with a 503 naming the gap**,
so the service can be brought up in stages — catalog first, then invites, then
promote. A *half*-configured store is an error, not a silently disabled one.
The startup log prints which seams are live.

### The public surface

`release.clawee.org` is this service. `/` (install), `/downloads`, `/verify`,
`/platforms` and `/docs` are server-rendered from the promoted catalog and the
channel manifests — there is no static `index.html` any more, and no cut copies
one. The page a visitor reads and the version an installer resolves therefore
cannot disagree.

**Every public handler reads the catalog through `CurrentPublic` or
`PublicHistory`, and neither can return a `staged` row.** That is the property
the split exists for, enforced by the store's method set rather than by a filter
each handler remembers: `ListByComponent` returns every state and backs the
OPERATOR's history page, so a public handler that reached for it and forgot the
filter would look exactly like working code. A test seeds staged rows on both
channels and asserts their stamps, versions and artifact names appear nowhere.

The beta install line and the beta download tab carry content only while that
component has a beta `is_current`. A beta command that outlives its cycle
installs the last beta forever after it graduated.

What is still static, and why: the bootstraps (`<comp>/install.sh`,
`<comp>/upgrade.sh` and their `beta.*` twins), the signing pubkey, and the
per-channel badge JSONP. They embed no version — the bootstraps resolve one at
install time — and a static file cannot be affected by whether the service is
up. The badge is generated from the channel manifest and **only** from it: an
unreachable manifest writes an empty badge rather than falling back to
`versions/<comp>`, which is the number the NEXT cut will carry. `internal/staticsurface` is the one list of them; `publish-static` copies
exactly that list and the site's link check will not let a page link outside it.

### `publish-static`

```sh
clawee-release-manage publish-static --root <KIT_CHECKOUT> --dest <RELEASE_HOST>:<STATIC_DIR> --dry-run
clawee-release-manage publish-static --root <KIT_CHECKOUT> --dest <RELEASE_HOST>:<STATIC_DIR>
```

This was five `scp` lines inside `tools/release.sh`, running on every cut. Both
facts were wrong: serving a file is a publication and a cut publishes nothing,
and the files it copied embed no version, so it was a round trip to write bytes
that had not changed on the one host where an accidental write is visible to
everyone.

**Run it when the KIT changes** — a new bootstrap template, a regenerated badge,
a rotated signing key — not per release. It checks the whole set before copying
anything (a partial publish leaves some bootstraps new and some old, and each
still verifies its own download, so nothing downstream notices), refuses a kit
missing a generated file while naming the generator that produces it, and
`--dry-run` prints the plan. The host and the static dir come from the sealed
`release.dp` config; nothing about them is in this binary.

### What promote does, in order

    verify → copy every file → GitHub release → manifest LAST → flip → retention

Every step before the flip is reversible by doing nothing: a failure leaves the
row `staged`, the manifest untouched, and at most some orphaned public objects
the next attempt overwrites. Writing the manifest last is what makes that true —
the manifest is the go-live. Progress streams as NDJSON (`PATCH
/api/v1/manage/releases/{id}`) and as plain text from the page buttons, both
rendered from the same event stream.

The service runs with **`WriteTimeout: 0`**. It bounds the whole response, and
a promote's response is a progress stream open for the entire publish; at 60
seconds it truncated the operator's log mid-publish while the promote carried
on invisibly. What can wedge is bounded instead at the phases
(`ReadHeaderTimeout`, `IdleTimeout`) and inside the outbound clients, which
carry no blanket timeout for the same reason (`internal/r2/r2.go`).

**Yank inverts the order** — manifest first, then the row flips — because a
failure between the two must leave the withdrawn build *unserved* rather than
still being handed to every installer.

**Retention** keeps 10 stable / 1 beta per component on both surfaces, never the
current row, and runs at the end of every promote plus from `retain`. Pruning is
best effort: the catalog is the source of truth and bytes are reconciled to it.

### Known gaps

**Staging-store retention is not implemented.** The guideline
(`~/.agents/guidelines/release-management.md` §7) asks for the staging store to
be kept to the same counts plus every `staged` row; this service prunes the
public surface and GitHub only. The divergence is deliberate and visible in the
seam: `backend.Staging` exposes no `Delete`, so promote cannot damage what a
cut staged no matter how it is wired. Adding staging retention means adding
that capability, and it should arrive as its own narrower seam with its own
tests rather than by widening this one. Until then the staging bucket grows
without bound and is pruned by hand or by a bucket lifecycle rule.

`RecordLoginFailure` **warns and continues** when the catalog write fails, so a
database that has become unwritable would stop counting login attempts while
still authenticating them. Failing the login closed instead would turn a
transient disk problem into a total lockout of a surface whose whole job is
publishing. It is a deliberate trade, not an oversight; if the catalog is
unwritable, promote and invites are already refusing and the operator has a
bigger problem than the counter.

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
