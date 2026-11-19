# Site migration: GitHub Pages → Cloudflare (DNS + Workers static assets)

Who does what: **Owner** = a dashboard step only the account owner can do.
**Coordinator** = a repo/PR step. Each phase can be rolled back on its own.
Cost: $0. Cloudflare Free plan, plus Workers Free, where static asset
requests are free and unlimited.

## Phase 0: prepare (no user-visible change)

1. **Coordinator:** in `infosecstreams.github.io`, as one PR on the Astro site:
   - Copy `deploy/edge/site/_headers` to `site/public/_headers`.
   - Copy `deploy/edge/site/wrangler.jsonc` to `site/wrangler.jsonc`.
   - Add the `deploy-cloudflare` job from
     `deploy/edge/site/site-astro-cloudflare-deploy.yaml.snippet` to
     `.github/workflows/site-astro.yaml`, and add `&& vars.SITE_HOST != 'cloudflare'`
     to the Pages deploy job's `if:`.
   - Optional (site-astro): add a `404.astro` page; `not_found_handling` serves it.
2. **Owner, Cloudflare dashboard:** create a free account if there isn't one.
   **Add a domain** → `infosecstreams.com` → Free plan. Cloudflare scans the
   current DNS records.
3. **Owner, Cloudflare → DNS → Records:** make the imported zone match the
   live one exactly. The live DigitalOcean zone (checked 2026-11-19) has only:

   | Type | Name | Content | Proxy |
   |---|---|---|---|
   | A ×4 | `@` | 185.199.108.153, .109, .110, .111 (GitHub Pages) | **DNS only** for now |
   | CNAME | `www` | `infosecstreams.github.io` | **DNS only** for now |
   | A | `p` | 52.13.20.236 (EC2 Elastic IP) | **DNS only, always** |

   Note: `~/repos/iss/terraform-iss/digitalocean_dns.tf` also declares two
   TXT records (google-site-verification and
   `_github-challenge-infosecstreams-organization`) and a CAA record. None of
   them resolves today, so the terraform state and the live zone have drifted.
   If GitHub's verified-domain badge for the org matters, re-add that TXT
   record (GitHub → org Settings → Verified domains shows the value).
4. **Owner, Cloudflare → SSL/TLS:**
   - Overview: **Full (strict)**.
   - Edge Certificates: Always Use HTTPS **on**, Minimum TLS **1.2**,
     TLS 1.3 **on**, Automatic HTTPS Rewrites **on**.
   - Leave HSTS off here. The site sends its own header via `_headers`.
5. **Owner, Cloudflare → Speed / Rules / Network:**
   - Speed → Settings → **Early Hints: on**.
   - Network: **HTTP/3 (with QUIC): on**, **0-RTT Connection Resumption: on**.
     0-RTT only matters for idempotent GETs here; the site is static.
   - Compression (Rules → Compression Rules, or the default): keep the
     default, which serves Brotli to browsers that ask for it, and add Zstandard
     where the dashboard offers it.
6. **Owner, GitHub → infosecstreams.github.io → Settings → Environments:**
   create `cloudflare` (deployment branches: `main`). Add the secrets
   `CLOUDFLARE_API_TOKEN` (custom token: Account → *Workers Scripts: Edit*;
   Zone `infosecstreams.com` → *Workers Routes: Edit*) and `CLOUDFLARE_ACCOUNT_ID`.
   Set the repository variable `SITE_HOST=pages` for now.

## Phase 1: move DNS only (nameservers), nothing proxied yet

7. **Owner, Namecheap → Domain List → infosecstreams.com → Nameservers →
   Custom DNS:** replace `ns1/ns2/ns3.digitalocean.com` with the two
   `*.ns.cloudflare.com` names Cloudflare shows. Delegation from `.com`
   currently has a 2-day TTL (172800 s), so wait up to 48 h. Every record is
   still DNS only, so nothing changes for users. Check it:
   ```bash
   dig @a.gtld-servers.net NS infosecstreams.com +noall +authority
   dig @1.1.1.1 p.infosecstreams.com +short   # still 52.13.20.236
   ```
   Cloudflare's dashboard marks the zone **Active**.
   Rollback: put the DigitalOcean nameservers back. Keep the DO zone, unchanged,
   until phase 3 has run for two weeks.

## Phase 2: serve the site from Cloudflare

8. **Coordinator:** set `SITE_HOST=both`. Push or run the workflow. The
   `deploy-cloudflare` job creates the Worker `infosecstreams-site`.
   - The custom-domain route in `wrangler.jsonc` makes Cloudflare replace the
     apex A records with its own record and issue the certificate.
   - If the dashboard refuses because the apex A records exist, delete the
     four GitHub A records first, then re-run the job. That's a gap of a few
     seconds for new visitors only, since resolvers keep the cached answer.
   - **Owner** may need to confirm the custom domain under Workers & Pages →
     `infosecstreams-site` → Settings → Domains & Routes.
9. **Owner, Cloudflare → DNS:** change `www` to a **proxied** `CNAME www →
   infosecstreams.com`. Then **Rules → Redirect Rules → Create** with
   "Redirect from WWW to root" (a template): `https://www.infosecstreams.com/*` →
   `https://infosecstreams.com/${1}`, 301, preserve the query string.
10. **Owner, GitHub → infosecstreams.github.io → Settings → Pages:** leave
    the custom domain set for now, so phase 3 can roll back. GitHub shows a
    DNS warning once the apex no longer points at it; that's expected.
11. **Verify** from any machine:
    ```bash
    deploy/edge/measure.sh https://infosecstreams.com/ https://infosecstreams.com/faq
    curl -sI https://infosecstreams.com/_astro/<any>.js | grep -i cache-control   # max-age=31536000, immutable
    curl -sI https://www.infosecstreams.com/ | grep -i location                   # 301 → apex
    ```
    Expect `server: cloudflare`, `alt-svc: h3=":443"`, HTTP/3 `3 200`,
    `content-encoding: br`, the `_headers` headers, and a `103` with the
    preconnect/preload `Link`.
    Early Hints for Workers static assets is **not verified**: Cloudflare
    documents it for proxied/cached responses and for Pages. If no 103
    appears, nothing breaks; the `<link rel=preload>` in the HTML still works.
    Then load the site in a browser and check the console for CSP errors.

Rollback for phase 2: in Cloudflare DNS, remove the Worker custom domain
(Workers → Domains & Routes) and re-add the four GitHub A records, DNS only.
Set `SITE_HOST=pages`. GitHub Pages was deployed the whole time (`both`).

## Phase 3: finish (after 2 weeks without a rollback)

12. **Coordinator:** set `SITE_HOST=cloudflare`, so the Pages job stops.
13. **Owner, GitHub → Pages:** remove the custom domain, and switch Pages
    off (Settings → Pages → Source: none, or unpublish), so
    `infosecstreams.github.io` doesn't serve a stale copy. Delete
    `site/public/CNAME` (coordinator).
14. **Owner:** delete the DigitalOcean DNS zone. Move the terraform
    `digitalocean_record` resources to the `cloudflare` provider
    (`cloudflare_dns_record`), or drop them from terraform, so the
    `iss` repo matches reality again.
15. **Optional, CAA:** if you publish one, include every CA in use:
    ```
    infosecstreams.com. CAA 0 issue "letsencrypt.org"   ; Caddy on p.
    infosecstreams.com. CAA 0 issue "pki.goog"          ; Cloudflare edge certs
    infosecstreams.com. CAA 0 issue "ssl.com"           ; Cloudflare edge certs
    ```

## What does not change

- `p.infosecstreams.com` stays **DNS only** and goes straight to the EC2 host
  (Caddy, see `../CUTOVER-caddy.md`). Plausible keeps seeing real client IPs,
  Twitch webhooks don't pass through Cloudflare, and the API's 15 s freshness
  stays under our control.
- The bot stays on EC2 (owner decision).
- The build job, the Pages artifact and the daily regen triggers.
