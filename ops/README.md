# Ops — release.clawee.org

Operator activation notes for the public install channel. **Every step below is
OPERATOR-ACTIVATION** — none runs as part of CI or the release script; do them
once by hand on the host. After that the vhost proxies the site's pages to
`clawee-release-manage`, and `clawee-release-manage publish-static` refreshes
the static half when the kit changes — no release step writes here.

Host: `nsm.renative.com` (the same box that fronts the console / umbree /
burree / `release.burrowee.com`). Static surface:
`/ebs_storage/apps/release.clawee.org/static` (the `--dest` `publish-static` is
run with). Edge: Cloudflare, **Full (strict)** mode.

The nginx vhost is `ops/nginx/release.clawee.org.conf`.

---

## 1. DNS — OPERATOR

Create an **A record** for `release.clawee.org` → the nsm origin IP, and set it
**Cloudflare-proxied** (orange cloud). Full (strict) means CF validates the
origin cert, so a real cert must be in place on the origin (step 3) before the
SSL mode will succeed.

## 2. Install the vhost — OPERATOR

> **nsm-specific:** this host's `/etc/nginx/nginx.conf` includes only
> `/etc/nginx/sites-enabled/*` — it does **NOT** include
> `/etc/nginx/conf.d/*.conf`. A file dropped under `conf.d/` is silently dead
> (nginx -t passes, reload succeeds, directives never run). It **must** go into
> `sites-enabled/`.

```sh
# OPERATOR, on nsm:
sudo cp ops/nginx/release.clawee.org.conf \
        /etc/nginx/sites-enabled/release.clawee.org.conf
sudo mkdir -p /ebs_storage/apps/release.clawee.org/static
```

Do **not** add `default_server` to this vhost — another sites-enabled file
already owns `default_server` on `:443`; a duplicate fails `nginx -t`.

## 3. Issue the origin cert — OPERATOR

Issue a cert for `release.clawee.org` (mirror whatever the console /
`release.burrowee.com` vhost on this host uses — e.g. certbot / the host's
existing LE setup), then point the `ssl_certificate` / `ssl_certificate_key`
placeholders in the vhost at the real paths.

> **nsm-specific:** this host's nginx build **rejects `TLSv1.3`** — the vhost
> pins `ssl_protocols TLSv1.2;`. Leave it; raising it to TLSv1.3 fails
> `nginx -t` and aborts the reload.

## 4. Validate + reload — OPERATOR

```sh
# OPERATOR, on nsm:
sudo nginx -t && sudo systemctl reload nginx
```

## 5. First publish — OPERATOR

Two separate acts, and only the second touches this host's disk.

**The static surface** — the bootstraps, their beta twins, the signing pubkey
and the per-channel badge JSONP — is published from a kit checkout with

```sh
clawee-release-manage publish-static --root <KIT_CHECKOUT> --dest <RELEASE_HOST>:<STATIC_DIR> --dry-run
clawee-release-manage publish-static --root <KIT_CHECKOUT> --dest <RELEASE_HOST>:<STATIC_DIR>
```

Run it **when the kit changes**, not per release: those files embed no version.

**Releases** are cut with `tools/release.sh`, which stages privately and
registers a `staged` catalog row — it copies nothing here and publishes
nothing. Going live is `promote`, in the manage service.

The site's pages are no longer files: `/`, `/downloads`, `/verify`,
`/platforms` and `/docs` are rendered by `clawee-release-manage` from the
promoted catalog, so this vhost proxies them (see the nginx conf) and there is
no `index.html` to copy.

## 6. Smoke test

```sh
curl -fsSI https://release.clawee.org/                              # 200, text/html (proxied)
curl -fsSI https://release.clawee.org/downloads                     # 200, text/html (proxied)
curl -fsSI https://release.clawee.org/clawee/beta.install.sh        # 200, text/x-shellscript
curl -fsSI https://release.clawee.org/clawee/install.sh             # 200, text/x-shellscript
curl -fsSI https://release.clawee.org/claweed/install.sh            # 200, text/x-shellscript
curl -fsSI https://release.clawee.org/clawee-release.pub            # 200, text/plain
```

A green install path end-to-end:

```sh
curl -fsSL --proto '=https' --tlsv1.2 https://release.clawee.org/clawee/install.sh | sh
```
