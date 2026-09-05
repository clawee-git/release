# Ops runbook — the release host

Bringing up `release.clawee.org`: the buckets, the manage service, its edge,
and the first promote.

**Every step here is OPERATOR-ACTIVATION.** None of it runs from CI, from
`tools/release.sh`, or from an agent session. The agent's chain ends at the cut
— build, stage privately, register the row, report the stamp and the row's
manage URL (`~/.agents/guidelines/release-management.md` §8,
`~/.agents/guidelines/release.md` §11). Everything below is the other side of
that line, and the order is the order it is done in.

Placeholders only in this file. The real account, buckets, credential paths and
token live in the sealed `release.dp` config (`~/.agents/guidelines/secrets.md`)
— load it before you start, and if a value is missing, stop and ask rather than
guessing one.

| Placeholder | What it is |
|---|---|
| `<RELEASE_HOST>` | the origin serving this site |
| `<PORT>` | the loopback port the service binds (`--listen`, default `8787`) |
| `<DATA_DIR>` | the service's data root: the catalog and the secret key |
| `<STATIC_DIR>` | the nginx static root |
| `<STAGING_BUCKET>` / `<PUBLIC_BUCKET>` | the private staging and public buckets |
| `<R2_ACCOUNT>` / `<R2_CREDS>` | the Cloudflare account, and the file holding `access_key_id` / `secret_access_key` |
| `<GH_TOKEN_FILE>` | the file holding the GitHub release token |
| `<ORG>/<REPO>` | the repo the GitHub releases are published to |
| `<MANAGE_URL>` | the service's base URL — what a cut registers against |
| `<KIT_CHECKOUT>` | a checkout of this repo, for `publish-static` |

Host facts that are not secrets, and that predate this runbook: the origin is
`nsm.renative.com`, which also fronts the console / umbree / burree /
`release.burrowee.com`; the static root is
`/ebs_storage/apps/release.clawee.org/static`; the edge is Cloudflare in **Full
(strict)** mode.

---

## 1. Buckets

Two, and the split between them is the entire publication control in this
product (`release-management.md` §3). A cut writes only to staging; only
**promote** copies into public.

**Staging — PRIVATE, and it must stay that way.**

- No public access policy. No `r2.dev` public URL. **No custom domain.**
- Nothing else is needed: the service reads it with credentials, and hands a
  human a *presigned* URL when an invite is minted.
- It holds every build that has been cut and not promoted. A staging bucket
  that answers an unauthenticated GET has been publishing unapproved builds
  since the day it was created, and nothing downstream can notice — promote
  works, the pages render, the catalog is right. Step 6 checks it.

**Public — unchanged from what the installers already read.** Public access on,
served at `<PUBLIC_BASE_URL>`, the same flat channel layout as today.

Both use one credential pair (`<R2_CREDS>`), a file readable only by the
service account. `--public-bucket` equal to `--staging-bucket` is refused: a
private store enforced by prefix is not a private store.

> **Staging retention is not implemented** (`AGENTS.md`, "Known gaps"). Prune
> the staging bucket by hand or with a bucket lifecycle rule until it is.

## 2. Sealed config keys

In `release.dp`, and nowhere else. The kit reads its half through
`CLAWEE_R2_CONFIG`; the service takes its half as flags, which the rendered
unit carries (step 4).

| Key | Value |
|---|---|
| `staging_bucket` | `<staging bucket>` — PRIVATE |
| `public_bucket` | `<public bucket>` |
| `r2_account_id` | `<R2 account>` |
| *creds file* | `<R2 creds path>` — `access_key_id` / `secret_access_key`, mode 0600 |
| *token file* | `<GitHub release token>` — mode 0600, and see step 6 for its scope |
| `manage_url` | `<manage base URL>` — what a cut registers against |

## 3. The service user, the data root, the binary

```sh
# OPERATOR, on <RELEASE_HOST>:
sudo useradd --system --home-dir <DATA_DIR> --shell /usr/sbin/nologin clawee-release
sudo install -d -o clawee-release -g clawee-release -m 0700 <DATA_DIR>
sudo install -o root -g root -m 0755 clawee-release-manage /usr/local/bin/
```

`<DATA_DIR>` is mode **0700** and owned by the service user. It holds the
catalog — which carries every session and every sealed second factor — and the
secret key beside it. The service refuses to start with a secret key at any
mode another account can read.

`--data-dir` has **no default and is never read from the environment**: a
guessed root is either a second empty catalog or a write into another
deployment's.

## 4. The unit and the timer

Rendered, never hand-typed. The three lines most easily left out of a
hand-written unit — `User=`, `ProtectSystem=strict`, `ReadWritePaths=` — are
exactly the three whose absence looks like a working service, and an unset
`User=` in a system unit is a publishing service running as root.

```sh
clawee-release-manage ops render --out ./rendered \
    --host <RELEASE_HOST> --static-dir <STATIC_DIR> \
    --data-dir <DATA_DIR> --base-url <MANAGE_URL> --listen 127.0.0.1:<PORT> \
    --r2-account <R2_ACCOUNT> --r2-creds <R2_CREDS> \
    --staging-bucket <STAGING_BUCKET> --public-bucket <PUBLIC_BUCKET> \
    --github-repo <ORG>/<REPO> --github-token-file <GH_TOKEN_FILE> \
    --public-base-url <PUBLIC_BASE_URL>
```

It writes and stops — it installs nothing, reloads nothing, enables nothing.

```
rendered/systemd/clawee-release-manage.service
rendered/systemd/clawee-release-manage-retain.service
rendered/systemd/clawee-release-manage-retain.timer
rendered/nginx/<RELEASE_HOST>.conf
```

The rendered units carry the bucket names and the credential paths, so they are
**not** committed to this repo — render them on the host, or render and copy
them there. The vhost carries no secret, so it *is* committed
(`ops/nginx/release.clawee.org.conf`) and is byte-gated against the renderer: a
hand edit there fails the build.

```sh
# OPERATOR, on <RELEASE_HOST>:
sudo cp rendered/systemd/*.service rendered/systemd/*.timer /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now clawee-release-manage.service
```

Leave the timer until step 7 — there is nothing to retain yet.

Once it is running, check that the unit can actually resolve a name before the
first promote — the hardening restricts the address families the service may
use, and a lookup failure reads as a bucket outage rather than as a unit
setting:

```sh
# OPERATOR, on <RELEASE_HOST>:
systemctl status clawee-release-manage.service     # active, no restart loop
journalctl -u clawee-release-manage -n 50          # the startup line names the live seams
```

The startup line prints which seams are configured and which will refuse, so a
missing bucket or token is visible here rather than at the first promote.

## 5. The edge

```sh
# OPERATOR, on <RELEASE_HOST>:
sudo mkdir -p <STATIC_DIR>
sudo cp rendered/nginx/<RELEASE_HOST>.conf /etc/nginx/sites-enabled/
sudo nginx -t && sudo systemctl reload nginx
```

Three traps on this origin, each of which fails in a way that does not look
like itself:

- **`sites-enabled/`, not `conf.d/`.** This host's `nginx.conf` includes only
  `/etc/nginx/sites-enabled/*`. A file dropped in `conf.d/` is silently dead:
  `nginx -t` passes, the reload succeeds, and none of the directives run.
- **TLSv1.2 only.** This host's nginx build rejects `TLSv1.3` and fails
  `nginx -t`, aborting the reload. The rendered vhost pins TLSv1.2; leave it.
- **No `default_server`.** Another `sites-enabled` file already owns
  `default_server` on `:443`, and a duplicate fails `nginx -t`.

DNS is an **A record** for `<RELEASE_HOST>` at the origin IP, proxied (orange
cloud). Full (strict) means the edge validates the origin certificate, so issue
a real cert first (mirror whatever the console / `release.burrowee.com` vhost on
this host uses) and point `ssl_certificate` / `ssl_certificate_key` at it.

The vhost's split: everything is the service's — `/`, `/downloads`, `/verify`,
`/platforms`, `/docs`, `/manage`, `/api` — **except** `*.sh`, `*.js`,
`/clawee-release.pub` and `*.txt` / `*.minisig`, which stay files. The
bootstraps are the trust anchor delivered as `curl … | sh`, they embed no
version, and a static file cannot be affected by whether the service is up.

## 6. Accounts, then `doctor`

```sh
# OPERATOR, as the service user:
clawee-release-manage admin add <name> --data-dir <DATA_DIR>
clawee-release-manage admin list --data-dir <DATA_DIR>
```

`admin add` sets a password and nothing else. **The second factor enrols itself
at first login**: the account's first sign-in at `<MANAGE_URL>/manage/login`
shows the TOTP secret once, and every later login requires a code. There is no
re-enrolment path — resetting someone's second factor is `admin remove`
followed by `admin add`, and their sessions cascade away with the account.

Then check the deployment:

```sh
clawee-release-manage doctor --data-dir <DATA_DIR> --user clawee-release \
    --r2-account <R2_ACCOUNT> --r2-creds <R2_CREDS> \
    --staging-bucket <STAGING_BUCKET> --public-bucket <PUBLIC_BUCKET> \
    --github-repo <ORG>/<REPO> --github-token-file <GH_TOKEN_FILE> \
    --kit-root <KIT_CHECKOUT> --check-write
```

`--user` is the account the service runs as, and it is what the ownership
checks compare against — not whoever typed the command. Run `doctor` under
`sudo` without it and a correctly owned data root would be reported wrong; run
it as root against a root-owned tree and the check would pass while proving
nothing. If the user does not resolve on this host, every line says which
account was compared instead.

`doctor` opens the catalog **read-only** and never migrates it, so it must be
run after the catalog exists — `admin add` or the first service start creates
it. A data dir with no catalog is a failure naming the path, which is what a
mistyped `--data-dir` looks like; nothing is created for it.

Run it with the **same flags the unit carries** — a doctor pointed at different
buckets is a doctor checking a different deployment. Exit 0 means every check
passed, 1 that at least one failed, 2 that the invocation was wrong. `--json`
prints the same report for a monitor.

| Check | Fails when |
|---|---|
| `catalog` | there is no catalog at `<DATA_DIR>`, it will not open, or its migration ledger and the binary disagree in either direction. Read-only: a missing catalog is never created, and a pending migration is reported, never applied |
| `data-dir` | the data root is readable by group or other, or is not owned by `--user` |
| `secret-key` | the key is missing, is not 0600, or is not owned by `--user` |
| `release-key` | the baked signing key, `<KIT_CHECKOUT>/clawee-release.pub` and the generated bootstraps do not all agree. Skipped with no `--kit-root` |
| `staging-bucket` | the private bucket is not reachable with these credentials |
| `staging-private` | **the staging bucket served an unauthenticated GET.** Fix it before promoting anything: remove the public access policy and any custom domain |
| `public-bucket` | the public bucket is not reachable |
| `github` | the token cannot read `<ORG>/<REPO>` — or, with `--check-write`, its permissions would not allow publishing a release |

`doctor` **writes nothing**: no probe object, no draft release, no touched
file. `--check-write` reads the repo's permissions rather than creating
anything, which is why the whole verb is safe to run against production —
looking is the one sanctioned thing to do on a production host.

A store you have not wired yet reports **skipped**, not failed. The service is
designed to come up in stages: catalog first, then invites once staging is
wired, then promote once the public bucket and the token are.

## 7. The retention timer

```sh
# OPERATOR, on <RELEASE_HOST>:
sudo systemctl enable --now clawee-release-manage-retain.timer
systemctl list-timers clawee-release-manage-retain.timer
```

Promote already runs retention for the pair it just published, so this is the
**net**: it covers a component nobody has promoted for a month. It keeps 10
stable / 1 beta per component, never the current row, on both the public bucket
and the GitHub listing. `Persistent=true`, so a host that was down at the
scheduled hour catches up at the next boot.

The pass **refuses** without a public store and a GitHub publisher — expiring a
row is one-way, so marking rows it cannot prune would orphan their bytes
permanently. Before the stores are wired, preview instead:

```sh
clawee-release-manage retain --data-dir <DATA_DIR> --dry-run
```

## 8. The static surface

```sh
clawee-release-manage publish-static --root <KIT_CHECKOUT> --dest <RELEASE_HOST>:<STATIC_DIR> --dry-run
clawee-release-manage publish-static --root <KIT_CHECKOUT> --dest <RELEASE_HOST>:<STATIC_DIR>
```

Run it **when the kit changes** — a new bootstrap template, a regenerated
badge, a rotated signing key — **not per release**. Those files embed no
version; the bootstraps resolve one at install time. It checks the whole set
before copying anything, because a partial publish leaves some bootstraps new
and some old and each still verifies its own download, so nothing downstream
notices.

The site's pages are not files. `/`, `/downloads`, `/verify`, `/platforms` and
`/docs` are rendered from the promoted catalog, so there is no `index.html` to
copy and no cut copies one.

## 9. The first promote

A cut has already run and reported a stamp and the row's manage URL. **Make the
first one a beta row.** A stable promote is what every installer resolves next;
a beta promote is reachable only by someone who asked for the beta channel, so
a mistake costs a yank rather than a bad install for everyone.

1. Sign in at `<MANAGE_URL>/manage`, find the `staged` row.
2. Press **Promote**. The page streams progress as it happens.
3. Watch the stream. The order is fixed and the reason is the failure mode:

       verify → copy every file → GitHub release → manifest LAST → flip → retention

   Everything before the flip is reversible by doing nothing: a failure leaves
   the row `staged`, the manifest untouched, and at most some orphaned public
   objects the next attempt overwrites. **The manifest is the go-live**, which
   is why it is written last.

   A promote is several hundred megabytes of copying and the stream stays open
   for all of it. A stream that stops moving is worth investigating; a stream
   that is slow is a promote.

### After a promote — what to check

- The row reads `current` on its channel, and the previous current does not.
- `curl -fsS <PUBLIC_BASE_URL>/<comp>/latest.json` (or the beta manifest) names
  the new stamp — this is the file that decides what an installer fetches.
- The GitHub release exists at `<ORG>/<REPO>`, carries all four zips plus
  `SHA256SUMS.txt` and its `.minisig`, and is marked **pre-release** for a beta.
- `<MANAGE_URL>/downloads` lists it — the pages read the same catalog, so a
  disagreement here means the flip did not complete.
- An install from a clean machine (a container, never this host):

  ```sh
  curl -fsSL --proto '=https' --tlsv1.2 https://<RELEASE_HOST>/clawee/install.sh | sh
  ```

- The retention pass at the end of the promote pruned to 10 stable / 1 beta and
  did not touch the current row.

### Rollback is a yank

There is no "unpromote". **Yank** the row: it inverts promote's order — the
manifest first, then the row flips — so a failure between the two leaves the
withdrawn build *unserved* rather than still being handed to every installer.

A yanked row keeps its place in the download history and loses its links. Its
bytes usually remain in the bucket and on the GitHub release; yank withdraws
the release, it does not delete the objects, which is exactly why the pages must
not hand them out. To go back to the previous version, promote that row again.

## 10. Invite links

Minted from the manage surface (`/manage/invites`), never from a cut. An invite
is a presigned, time-limited URL to a rendered `install.sh` in the *staging*
bucket: it puts a build on somebody's host, which is why it is an operator act
and why the staging bucket must not be readable without one.

## 11. Smoke test

```sh
curl -fsSI https://<RELEASE_HOST>/                            # 200 text/html (proxied)
curl -fsSI https://<RELEASE_HOST>/downloads                   # 200 text/html (proxied)
curl -fsSI https://<RELEASE_HOST>/manage/login                # 200 text/html (proxied)
curl -fsSI https://<RELEASE_HOST>/clawee/install.sh           # 200 text/x-shellscript (static)
curl -fsSI https://<RELEASE_HOST>/clawee/beta.install.sh      # 200 text/x-shellscript (static)
curl -fsSI https://<RELEASE_HOST>/claweed/install.sh          # 200 text/x-shellscript (static)
curl -fsSI https://<RELEASE_HOST>/clawee-release.pub          # 200 text/plain (static)
```

## 12. A note on the trusted proxy

Login attempts are rate-limited per (stage, client IP, account), and `ClientIP`
reads `RemoteAddr` **only** — never `X-Forwarded-For`. Behind this vhost every
request arrives from the loopback address, so the limit is effectively
per-account, which is the conservative behaviour: it cannot be widened by a
forged header, and a forged header is exactly how a per-IP limit is defeated.

If this deployment ever needs genuinely per-client limits, that is a
**trusted-proxy** decision — which hop's address to believe, and why this
service may believe it — taken explicitly and implemented then. Do not add a
`--trusted-proxy` flag on the strength of this note.
