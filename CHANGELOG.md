# Changelog — clawee-git/release

Notable changes to the release kit and its services. Cuts themselves are
recorded by their `[RELEASED: <comp>]` commits, not here.

## Unreleased

### Added — `clawee-release-manage`, the publish-management service

A cut stages privately and registers a row; going live is now a separate,
operator-only act. This is the service that act happens in
(`~/.agents/guidelines/release-management.md`).

- **Catalog.** SQLite, pure-Go driver, one file under `--data-dir`, WAL,
  forward-only ledgered migrations. Release rows, admins, sessions, CSRF
  tokens, register nonces, invites, login-failure counters.
- **Registration.** `POST /api/v1/releases/nonce` and `…/register`:
  machine-authenticated by the same Ed25519 key that signs `SHA256SUMS.txt`,
  verified against the baked `clawee-release.pub`. No admin credential lives in
  a release kit.
- **Auth.** Accounts provisioned on the host with `admin add`; password
  (argon2id) plus TOTP enrolled at first login, its secret sealed with a key
  that does not live in the catalog. Session and CSRF cookies in their own
  namespace; every write CSRF-gated.
- **Read surfaces and pages.** Per-channel summary and history APIs, and
  server-rendered `/manage` pages — two tabs, per-component cards, history,
  invites, login. The public `/` reads promoted rows only, by construction.
- **Invites.** `POST /api/v1/manage/releases/{channel}/install-url` mints a
  48-hour `curl … | sh` link for a `staged` or `public` row: presigned
  artifacts wrapped in a generated `install.sh` that runs the same verification
  chain as the public bootstrap. Every mint is audited; the copy command is
  served only while the link is live.
- **Promote and yank.** `PATCH /api/v1/manage/releases/{id}`, streaming NDJSON
  progress. Verify → copy → GitHub release → manifest last → flip → retention;
  fails closed without a GitHub publisher. Yank re-points the manifest at the
  newest remaining public row or removes it, and never deletes bytes.
- **Retention.** 10 stable / 1 beta per component on the public surface and on
  GitHub, never the current row, at the end of every promote and from the
  `retain` verb.
- **CLI.** `serve`, `admin add|list|remove`, `retain`, `version`, `docs` on the
  shared help model; `docs/cli-help.md` is generated and gated by a test.

### Changed

- **One Go module.** `tools/r2-mirror` folded into the root module so the
  S3/SigV4 client (`internal/r2`), the credentials parser and the `latest.json`
  schema (`internal/manifest`) have one home each. `go test ./...` at the repo
  root now covers what needed two runs before.
- The R2 client gained `Head`, `Get`, server-side `Copy` and query presigning,
  plus an injectable clock so a presigned URL is reproducible under test.
- The GitHub release body carries the verify-by-hand recipe again — the surface
  `tools/test-checksum-verify.sh` pinned before publishing left the kit. Its
  test executes the emitted block against a stub pre-2016 `shasum`.
