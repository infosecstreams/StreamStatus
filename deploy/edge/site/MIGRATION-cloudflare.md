# Site migration: GitHub Pages → Cloudflare Workers static assets

The zone, DNS records, redirect and apex binding are **Terraform**
(`terraform-iss/edge` (branch `infra-edge`, worktree `~/repos/worktrees/terraform-iss-edge`)). The order of phases, the nameserver move and rollback
are in its `RUNBOOK.md` (phases 1 and 4). This file covers the
site repository's side and the checks. Cost: $0. Static-asset requests are
free and unlimited on Workers Free.

Correction to the first version of this file: the live zone also has
Google Workspace **MX** records and two google-site-verification **TXT**
records. Plain UDP `dig` from the owner's network hid them. They are all
carried over in `cloudflare.tf` (inventory in the RUNBOOK).

## Site repository (coordinator, one PR in infosecstreams.github.io)

1. Copy `_headers` to `site/public/_headers`. Astro copies `public/` into
   `dist/`.
2. Copy `wrangler.jsonc` to `site/wrangler.jsonc`. It has **no routes**:
   Terraform binds the apex.
3. Add the `deploy-cloudflare` job from `site-astro-cloudflare-deploy.yaml.snippet`
   to `.github/workflows/site-astro.yaml`, and add
   `&& vars.SITE_HOST != 'cloudflare'` to the Pages deploy job's `if:`.
4. Optional (site-astro): a `404.astro` page; `not_found_handling` serves it.

## GitHub (owner)

- Environment `cloudflare` (deployment branches: `main`) with these secrets:
  - `CLOUDFLARE_API_TOKEN`: a custom token with Account → *Workers Scripts:
    Edit* and nothing else. The custom domain is Terraform's job, so CI needs
    no zone rights.
  - `CLOUDFLARE_ACCOUNT_ID`
- Repository variable `SITE_HOST`. Use `pages` until RUNBOOK phase 4, `both`
  during it, and `cloudflare` after.

## Checks after RUNBOOK phase 4

```bash
deploy/edge/measure.sh https://infosecstreams.com/ https://infosecstreams.com/faq
curl -sI https://infosecstreams.com/_astro/<any>.js | grep -i cache-control   # max-age=31536000, immutable
curl -sI https://www.infosecstreams.com/ | grep -i location                   # 301 → https://infosecstreams.com/
```

Expect:
- `server: cloudflare`, `alt-svc: h3=":443"`, HTTP/3 `3 200`
- `content-encoding: zstd` or `br` (Cloudflare Free defaults to zstd)
- the `_headers` headers
- ideally a `103` carrying the preconnect/preload `Link`

Early Hints for Workers static assets is **not verified**. Cloudflare
documents it for proxied responses and for Pages. If no 103 appears, nothing
breaks; the `<link rel=preload>` in the HTML still works. Then load the site
in a browser and check the console for CSP errors.

## Local test already done

`_headers` and `wrangler.jsonc` were checked with `wrangler dev`:
- 9 header rules parsed
- `/faq` serves `faq.html`; `/faq.html` returns 307 to `/faq`
- `/_astro/*` is immutable; `/sw.js` is `no-cache` with its own CSP
- the `Link` header appears on HTML only

The workflow job passes actionlint 1.7.7.

## What does not change

- `p.infosecstreams.com` stays **DNS only** and points straight at the
  Lightsail box (Caddy).
- The bot stays on its own box (the owner's decision).
- The build job, the Pages artifact and the daily regen triggers.
