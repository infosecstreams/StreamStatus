# Site hosting without GitHub Pages, cost of the box, and "newer than Caddy?"

The owner's questions (2026-11-19):
- "We can drop GitHub pages if we don't need it ... now it costs like $22 a month."
- "Are there newer and better stuff than Caddy?"

Everything here is measured or checked against official pricing pages and
docs. Prices are as of 2026-11-19, and anything unverified is flagged.

## 1. Where the site should live once GitHub Pages is dropped

| | (a) EC2 box, behind Caddy | (b) EC2 origin + Cloudflare Free in front | **(c) Cloudflare Workers static assets** |
|---|---|---|---|
| Extra cost | $0 | $0 | **$0** (static requests free and unlimited) |
| Latency, far users | One region (us-west-2). A cold load from Europe or Asia needs ~2 round trips of 150–200 ms each, so **~300–450 ms to first byte** | Edge-cached: one RTT to the nearest PoP (~20–50 ms) after the first miss per PoP | **Same as (b)**, with no origin and no misses |
| HTTP/3, Brotli | Yes. Caddy serves brotli-11/zstd-19 files made at build time (tested: home page 9.4 KB br) | Yes (Cloudflare Free compresses with **zstd by default**, br as fallback) | Yes |
| 103 Early Hints | **No.** Caddy has no Early Hints directive (nginx ≥1.29 has one) | Yes, from origin `Link` headers ([docs](https://developers.cloudflare.com/cache/advanced-configuration/early-hints/)) | Not documented for static assets: test it |
| Headers / CSP / immutable `/_astro/*` | `caddy/site.caddy` (tested) | Origin sets them; Cache Rules for edge TTL | `_headers` (tested with `wrangler dev`) |
| Deploy | New pipeline to the box: push a signed artifact, then the host does an atomic symlink swap. SSH from GitHub runners is blocked by the security group today | (a)'s pipeline **plus** a cache-purge API call | `wrangler deploy` job, atomic, no purge |
| Availability | **The site goes down with the box** (Plausible OOM, host migration, deploys) | Edge serves stale while the box is down (if `stale-if-error` and Cache Rules are set) | Independent of the box |
| DNS | Stays at DigitalOcean (apex A record → Elastic IP) | Must move to Cloudflare | Must move to Cloudflare |
| Ops burden | Highest: you own TLS, deploys, uptime and abuse for the site too | Medium | **Lowest** |

**Recommendation: (c).** McMaster's speed comes from Akamai's edge. A
single Oregon box can't match that for anyone outside the US west coast,
however fast the proxy is. (c) costs nothing, has the fewest moving parts,
and keeps the site up when the box isn't. Dropping GitHub Pages is fine:
nothing else needs it. The only real cost is moving nameservers to Cloudflare
(`site/MIGRATION-cloudflare.md`). Finish with `SITE_HOST=cloudflare` and
Pages switched off.

- **(a) is the fallback** if the owner won't move DNS. `caddy/site.caddy`
  plus `site/precompress.sh` are ready and tested locally. The missing piece is
  a deploy path:
  - Recommended: build in Actions, run `precompress.sh`, publish the dist as
    a signed OCI artifact on GHCR, and have a forced-command
    `deploy-site sha256:<digest>` on the host (the same model as
    `deploy-streamstatus`) unpack it into `site/releases/<sha>` and swap the
    `current` symlink.
  - Like the bot deploy, it needs SSH from GitHub runners, which the security
    group doesn't allow today.
- **(b)** only makes sense if the site had to stay on the box for some other
  reason. It has (a)'s ops plus (c)'s DNS move.
- **Bunny** (~$1/month minimum) would need Bunny DNS for the apex.
  **CloudFront** (always-free 1 TB) has no zstd and no documented Early
  Hints. Neither beats free Cloudflare for this.

## 2. Cost of the box

**Measured on the host (read-only):** t3.small (x86, 2 vCPU, 1.9 GiB) in
us-west-2d, up 90 days, one 30 GiB root volume.

| Container | Memory now |
|---|---|
| plausible | 433 MiB (limit 700 MiB) |
| clickhouse | 303 MiB (limit 512 MiB) |
| postgres | 98 MiB |
| streamstatus | 77 MiB |
| nginx + certbot | 10 MiB |

Host: 1026 MiB used, **285 MiB in swap**, 634 MiB available. The 2 GiB box is
tight but working. Plausible CE recommends "at least 2 GB of RAM"
([README](https://github.com/plausible/community-edition)). ClickHouse says
2 GB "works with tuning", so our 512 MiB cap is already below its own floor;
its low-memory tips are in [ClickHouse usage tips](https://clickhouse.com/docs/operations/tips).

**Today's bill, rebuilt** (us-west-2,
[EC2](https://aws.amazon.com/ec2/pricing/on-demand/),
[EBS](https://aws.amazon.com/ebs/pricing/),
[VPC IPv4](https://aws.amazon.com/vpc/pricing/)):

| Line | Rate | Monthly |
|---|---|---|
| t3.small on-demand | $0.0208/h | $15.18 |
| gp3 root 30 GiB | $0.08/GB-mo | $2.40 |
| Second 30 GiB gp3 volume (`aws_ebs_volume.iss-prod` in terraform) | $0.08/GB-mo | $2.40 **if it exists**. It is **not attached** to the instance (lsblk shows only the root disk), so likely an orphan. Its snapshot (`aws_ebs_snapshot.iss-prod`) costs ~$1.50 more. |
| Public IPv4 (Elastic IP) | $0.005/h | $3.65 |
| **Total** | | **≈ $21.2–$25.1** (matches the owner's ~$22) |

**Options, all-in monthly:**

| Option | Monthly | RAM | Effort | Notes |
|---|---|---|---|---|
| Today | ~$22–25 | 2 GiB | — | |
| **Quick win: delete the orphan volume and snapshot** (after checking in the console that it is unattached and unneeded) | −$2.40 to −$3.90 | — | 5 min | No other change |
| 1-yr EC2 Instance Savings Plan on the **same t3.small** | ~$15.5 | 2 GiB | 10 min, no migration | Commits for a year; rules out moving within the year |
| t4g.small (Graviton) + 1-yr EC2 Instance SP ($0.0105/h) | ~$13.7 | 2 GiB | Migration plus an **arm64 build of the bot** (the only amd64-only image; Plausible, ClickHouse, Postgres and Caddy are all multi-arch) | Postgres must move with pg_dump (the data dir isn't portable across architectures) |
| t4g.medium + 1-yr SP | ~$21.4 | **4 GiB** | Same as above | Today's money, double the RAM |
| **Lightsail 2 GB** ([pricing](https://aws.amazon.com/lightsail/pricing/)) | **$12** flat (2 vCPU, 60 GB SSD, 3 TB transfer, IPv4 included) | 2 GiB | Migration only. Same x86 arch, so docker volumes can be copied as-is | **No commitment.** Same AWS account. Firewall supports UDP 443 |
| Lightsail 4 GB | $24 | 4 GiB | Same | If memory pressure bites |
| DigitalOcean 2 GB/2 vCPU ([pricing](https://www.digitalocean.com/pricing/droplets)) | $18 | 2 GiB | Migration | DNS is already there, but that doesn't matter after (c) |
| Hetzner CAX11 (ARM, 4 GB, EU only) | €6.49 + VAT | 4 GiB | Migration + arm64 bot | **Shown "currently unavailable"**; US locations only offer CPX at €17.99 for 2 GB after the 2026 price rises. Not recommended right now |

**Recommendation:**
1. **Now:** delete the orphan EBS volume and snapshot, if the console
   confirms them.
2. **Then:** move to **Lightsail 2 GB for $12/month**, about **half today's
   bill**.
   - Same architecture, so docker volumes copy across with no dump and restore.
   - No 1-year lock-in, IPv4 included, 60 GB of disk.
   - Do it together with the Caddy cutover: the new box starts on Caddy.
     Rollback is then just the old instance, kept stopped for two weeks. A
     stopped EC2 instance costs only its disk and IP.
3. Apply ClickHouse's low-memory settings. If the box still swaps hard after
   the move, step up to the 4 GB plan ($24).
4. Don't buy a Savings Plan if you're moving. Buy one (on the same t3.small)
   only if staying put is the decision.

**Migration outline** (a runbook to write once the owner picks; not written yet):
1. Create the Lightsail instance with the same OS family and Docker, and open
   80/tcp, 443/tcp and 443/udp.
2. Lower the `p` record's TTL to 60 s a day ahead.
3. Stop the stack on the old box, then rsync `~/hosting` and the docker volumes.
4. Start on the new box with Caddy. Caddy issues a new certificate once DNS
   points there; certbot's certificate can be copied in as a stopgap.
5. Switch the `p` A record.

Downtime is ~15–30 min of Plausible ingestion. Twitch retries webhooks, and
the bot's 5-minute reconcile catches up.

## 3. Is there anything newer and better than Caddy?

Every version below was checked against official releases on 2026-11-19
(sources at the end).

| Server | Latest | HTTP/3 | Compression (on the fly / precompressed files) | 103 Early Hints | Built-in ACME | Verdict for this box |
|---|---|---|---|---|---|---|
| **Caddy** | 2.11.9 (2026-11-05) | Stable, default | gzip+zstd / br, zstd, gz files | No | Yes (HTTP-01, TLS-ALPN, ARI) | **Pick** |
| nginx mainline | 1.31.9 (2026-11-18) | **"Experimental"** in its own docs; HTTP/3 CVE fixes in 1.31.2 and 1.31.9 | gzip / gzip_static (brotli/zstd third-party) | Yes (≥1.29.0) | Separate `nginx-acme` module 0.4.x (pre-1.0) | OK, more pieces |
| Angie (nginx fork) | 1.12.4 | Yes | nginx-like | Likely (nginx base) | Built in | Best "nginx syntax" option; small community |
| Traefik | 3.7.19 | Yes | gzip/br/zstd | Unverified | Yes | Proxy only, no static files |
| HAProxy | 3.4.8 LTS | Production since 2.8 | gzip | Unverified | Yes (3.2+) | Proxy only, no static files |
| H2O | **no releases** ("use master") | Yes | br/gzip | Yes | No | Nothing to pin |
| Ferron | 2.8.1; 3.0 at RC | Unverified | Unverified | Unverified | Yes | Too young |
| River (ISRG Prossimo, Pingora) | 0.5.0 (2024-08) | No | No | No | — | **Abandoned**: no commits since 2024-09 |
| Pingora | 0.9.1 | — | — | — | — | A library, not a server |
| Pingap (Pingora-based) | 0.16.2 | Not listed | Plugins | Unverified | Yes | Pre-1.0, one main maintainer |
| Envoy | 1.40.2 | Yes | gzip/br/zstd filters | Unverified | No | Overkill |

**Is newer better here? No.** Caddy *is* the modern pick for one hostname
on one small box:
- One container replaces nginx and certbot.
- Stable HTTP/3, ARI renewals, ECH and post-quantum key exchange by default
  since 2.10.
- It serves precompressed files.
- The shortest config of the lot (~180 lines with comments here, tested).

The newer Rust and Pingora projects add no feature this box needs, and they
are less mature (River is abandoned). Caddy's one gap that matters is 103
Early Hints, and with the site on Cloudflare (recommendation 1) the edge
provides that. Measured memory: 27–40 MiB for Caddy vs 7 MiB for nginx today.
That's irrelevant next to ClickHouse.

Sources:
- [Caddy releases](https://github.com/caddyserver/caddy/releases)
- [nginx CHANGES](https://nginx.org/en/CHANGES)
- [nginx HTTP/3 module](https://nginx.org/en/docs/http/ngx_http_v3_module.html)
- [nginx-acme releases](https://github.com/nginx/nginx-acme/releases)
- [Angie releases](https://github.com/webserver-llc/angie/releases)
- [Traefik releases](https://github.com/traefik/traefik/releases)
- [HAProxy](https://www.haproxy.org/)
- [H2O "no more releases"](https://github.com/h2o/h2o/releases/tag/tag-no-more-releases)
- [Ferron releases](https://github.com/ferronweb/ferron/releases)
- [River](https://github.com/memorysafety/river)
- [Pingora releases](https://github.com/cloudflare/pingora/releases)
- [Pingap](https://github.com/vicanso/pingap)
- [Envoy releases](https://github.com/envoyproxy/envoy/releases)
- [Cloudflare compression](https://developers.cloudflare.com/speed/optimization/content/compression/)
- [Workers static assets billing](https://developers.cloudflare.com/workers/static-assets/billing-and-limitations/)
- [Hetzner cost-optimized](https://www.hetzner.com/cloud/cost-optimized/)
