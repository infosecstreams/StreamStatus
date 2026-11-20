# Edge: proxy on EC2 and hosting for the site

The goal is for every byte of infosecstreams.com and its live API to arrive
as fast as McMaster-Carr's (playbook rows 4 and 12). This folder holds the
measurements, the recommendation, and ready-to-apply configs. Nothing here
is applied yet.

| File | What |
|---|---|
| `measure.sh` | Read-only before/after probe: TLS, ALPN, compression, cache headers, 103, HTTP/3, cold TTFB, 304s |
| `measurements/` | The 2026-11-19 baseline (site, p. host, mcmaster.com and CDN references) |
| `caddy/Caddyfile` | Caddy for p.infosecstreams.com: Plausible (incl. its `/api/*` and `/live/*`), `/webhook/callbacks`, the bot's `/streamstatus/*` |
| `caddy/site.caddy` | Option (a) only: the Astro site served from the box (precompressed br/zstd), tested |
| `HOSTING-AND-COST.md` | Site hosting with GitHub Pages dropped, the box's cost and cheaper options, and the "newer than Caddy?" survey |
| `docker-compose.caddy.yml` | The `caddy` service for the host's compose file |
| `CUTOVER-caddy.md` | Cutover runbook: certificate obtained before switching, 2–4 s port hand-off, rollback |
| `terraform/README.md` | Pointer: the Terraform for the new stack (Lightsail 2 GB + Cloudflare zone) and its migration runbook live in `terraform-iss/edge` (branch `infra-edge`, worktree `~/repos/worktrees/terraform-iss-edge`) |
| `terraform/iss-sg-quic.tf.snippet` | Security group: UDP 443 for HTTP/3 (only if Caddy goes on the old EC2 box) |
| `nginx-streamstatus-api.conf` | **Only if Caddy is not adopted:** `location /streamstatus/` for the deployed nginx config, replacing ci-deploy's `location /api/` |
| `test/run.sh` | Local test of the Caddyfile against fake upstreams (32 checks, docker) |
| `site/` | Cloudflare migration for the Astro site: `_headers`, `wrangler.jsonc`, a workflow job, `MIGRATION-cloudflare.md`; `precompress.sh` for option (a) |

## Baseline (2026-11-19, from Rocklin CA)

| | infosecstreams.com (GitHub Pages) | p.infosecstreams.com (nginx on EC2) | www.mcmaster.com (Akamai) |
|---|---|---|---|
| HTTP | h2, **no h3** | h2, **no h3** (UDP 443 closed, nginx built without QUIC) | h2, no h3 |
| TLS | 1.3, RSA, no 0-RTT | 1.3, **RSA 4096**, session tickets off | 1.3 |
| Compression | **gzip only** (6405 B) | **none at all**: script.js 3005 B, dashboard 15.7 KB | br (14.6 KB of 80 KB) |
| HTML caching | `max-age=600`, fixed | n/a | `no-cache` at the browser, Akamai-cached |
| 103 Early Hints | no | no | no |
| Custom headers / CSP | impossible | yes | yes |
| Cold TTFB, median of 7 | 209 ms | 271 ms (script.js) | 265 ms |

Other findings:
- The Chainguard nginx image on the host is built without
  `http_v3_module`, brotli or zstd. Getting HTTP/3 on nginx means changing
  image or compiling modules.
- No OCSP stapling anywhere, and none is needed: Let's Encrypt ended OCSP in
  2025, so there is nothing to staple.
- McMaster doesn't serve HTTP/3 either. Its speed comes from the site
  techniques (prefetch, service worker, tiny HTML) plus Brotli and a warm CDN.
  We can beat its transport.

## Recommendation 1: Caddy replaces nginx and certbot on EC2

| | Caddy 2.11 (stock image) | nginx + QUIC + brotli |
|---|---|---|
| HTTP/3 | built in (quic-go); 0-RTT on resumption | nginx ≥1.25 `listen quic`, but **not in the Chainguard image**; switch to `nginx:mainline` |
| TLS certs | automatic: ACME, ARI renewal, ECDSA P-256 by default; the certbot container goes away | certbot container + webroot volume (today) |
| Compression | gzip + zstd built in; brotli needs a custom xcaddy build | gzip built in; brotli/zstd need third-party modules compiled into a custom image |
| Re-resolving upstream containers after a deploy | automatic (per connection) | `resolver 127.0.0.11` + variables (the ci-deploy workaround) |
| Config | ~180 lines, half of them comments; tested locally (`test/run.sh`) | ~200 lines across 3 files + module build |
| Memory | ~27–30 MiB RSS measured | ~5–10 MiB |

**Caddy wins.** It has fewer moving parts (no certbot, no module builds),
and HTTP/3 and ECDSA come with the stock image. 20 MiB more on the 2 GiB
t3.small is fine: Plausible's limits add up to ~1.2 GiB.

**API compression decision.** The bot already serves gzip from a
per-snapshot cache with strong ETags. Caddy passes `Accept-Encoding` through,
so gzip-capable clients get the bot's bytes (and its 304s), and Caddy only
encodes for zstd-only clients. On-the-fly zstd or brotli at the proxy would
add CPU per request for little gain: zstd-3 is 0–8 % smaller than gzip on our
JSON, and *larger* on small files. The real win is **brotli q11 precomputed by
the bot once per snapshot**: status.json is 20–28 % smaller (6239 → 4957 B
at 349 streamers; 15115 → 10925 B at 700). At 700 streamers that keeps it
inside the ~14.6 KB first congestion window, so a cold fetch saves a round
trip. This is a small bot change (pure-Go `andybalholm/brotli`, `Vary`
already set), proposed as a follow-up for bot-live-data. Caddy then passes
`br` through with no config change.

**Routing.** Plausible CE owns `/api/*` (`POST /api/event` on every
pageview, `/api/stats/<domain>/...`, `/api/v1|v2`) and `/live/*` (LiveView
`/live/websocket`, `/live/longpoll`) on the same host. The bot API therefore
lives under `/streamstatus/` (CONTRACT.md). The Caddyfile routes
`/streamstatus/*` and `/webhook/callbacks` to the bot and everything else to
Plausible; websocket upgrades need no extra config. The ci-deploy-security
snippet's `location /api/` must not be deployed: it would break analytics.

Headers added on top of the bot's: `stale-while-revalidate=<max-age>,
stale-if-error=300` on the API, `Cross-Origin-Resource-Policy: cross-origin`,
HSTS, nosniff, Referrer-Policy, and no `Server` header.

## Recommendation 2: the site moves to Cloudflare (Free): DNS plus Workers static assets

GitHub Pages can be dropped entirely. `HOSTING-AND-COST.md` compares serving
the site from the box itself (option a, configs ready as the fallback), the
box behind a CDN (b), and a CDN-native host (c, this recommendation).

| | GitHub Pages (today) | Cloudflare proxy in front of Pages | **Cloudflare Workers static assets** | Netlify (free credits) | Bunny CDN pull zone |
|---|---|---|---|---|---|
| Cost | free | free | **free** (static requests unlimited) | free tier, credit-metered | ~$1/month minimum |
| HTTP/3 | no | yes | **yes** (measured 183 ms h3 vs 244 ms h2 on a CF-hosted site) | no `alt-svc` seen | no `alt-svc` seen |
| Brotli | no | yes | **yes** | yes | yes |
| Edge HTML + purge on deploy | fixed 600 s | Cache Rules + purge API call per deploy; two CDNs chained | **atomic deploys, no purge**; HTML revalidates at the edge (`max-age=0` + ETag) | atomic deploys | purge API |
| Custom headers / CSP | no | Transform Rules (10 on Free) | **`_headers` file in the repo** (tested locally with wrangler) | `_headers` | edge rules |
| 103 Early Hints | no | yes (from origin Link headers, which Pages doesn't send) | zone toggle; for Workers assets **unverified** | no | no |
| Apex on current DNS | yes | needs CF nameservers | needs CF nameservers | apex A to a load balancer, not anycast, unless Netlify DNS | needs Bunny DNS (no apex CNAME at DO) |
| Deploy change | none | add a purge step | a `wrangler deploy` job (snippet), API token | new deploy | new deploy |

**Pick: Cloudflare Workers static assets, with the zone moved to Cloudflare
DNS** (nameservers changed at Namecheap; DigitalOcean DNS retired). It is the
only free option with HTTP/3, Brotli, repo-controlled headers (the CSP and the
service-worker headers) and atomic deploys without a purge step. The build
job and its artifact stay; only the deploy job is added. During the
migration both hosts deploy (`SITE_HOST=both`), so rolling back is a DNS
change. p.infosecstreams.com stays **DNS only**, straight to Caddy on EC2,
which keeps the owner's "bot stays on EC2" decision and real client IPs for
Plausible.

Cloudflare Pages would have served too. Cloudflare now points new projects
to Workers static assets, which has the same `_headers` and redirects
support, so we start there. The proxy-in-front-of-Pages variant is the
fallback if the owner wants zero workflow changes. It still needs the same
nameserver move, plus a purge call per deploy.

## Expected after

| | Today | After |
|---|---|---|
| Site, first visit, cold | h2, 209 ms TTFB, 6.4 KB gzip HTML | h3 ~180 ms (one RTT fewer for the handshake; more from Europe/Asia, where Pages' Fastly PoPs are fewer); Brotli HTML ~5.1–5.9 KB (−8 to −20 %) |
| Site, repeat visit | 304 or a cached copy for 10 min | service worker: ~0 ms from cache; background revalidation is a ~200 B 304 from the nearest PoP |
| Hashed JS | `max-age=600` | `max-age=31536000, immutable` (no revalidation at all) |
| p. TLS + first byte | h2, RSA-4096 cert, 271 ms | h3 (−1 RTT ≈ 50 ms from California, ~150 ms from Europe), ECDSA cert, 0-RTT on resumption |
| Plausible script.js | 3005 B (uncompressed) | 1324 B gzip |
| Plausible dashboard HTML | 15.7 KB | 3.1 KB |
| `/streamstatus/status.json` (349 streamers) | n/a yet | 6.2 KB gzip (bot); 5.0 KB once the bot precomputes brotli |

These are estimates from the measured components (reference CDN timings,
compression of our real payloads). Re-run `measure.sh` after each phase to
replace them with real numbers.

## Site CSP (for site-astro)

`_headers` sets only the directives a `<meta>` CSP can't carry
(`frame-ancestors`, plus `base-uri`, `object-src`, `form-action`). The pages
have inline scripts (the `html.js` class, boot/prefetch fallback,
`speculationrules`), so `script-src` must use their hashes. Astro can emit a
hashed `<meta http-equiv="Content-Security-Policy">` (`experimental.csp` /
`security.csp`, depending on the Astro version). Add to it:
`connect-src 'self' https://p.infosecstreams.com; script-src 'self' <hashes> https://p.infosecstreams.com 'inline-speculation-rules'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; worker-src 'self'`.
`/sw.js` gets its own strict CSP header (`default-src 'self'`). That is the
policy that governs the service worker's own fetches.
