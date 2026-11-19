# terraform/edge: Lightsail box + Cloudflare zone for infosecstreams

The new stack, as code: a Lightsail 2 GB instance (Amazon Linux 2023,
static IP, firewall, first-boot bootstrap) and the `infosecstreams.com` zone
on Cloudflare (records, settings, www→apex redirect, apex → site Worker).
`RUNBOOK.md` walks the migration from the EC2 box in phases, with rollback.

Checked locally: `terraform validate`, and `terraform plan` with fake
credentials and a local backend for both phases (29 and 26 resources to add,
no errors). The rendered user data passes `bash -n` and shellcheck. Its
package and repo steps were run in an `amazonlinux:2023` container (docker,
dnf-automatic, cloudflared 2026.11.1, tailscale 1.104.7, compose v5.8.0 with
its sha256 checked). **Nothing has been applied.**

## Where the existing Terraform lives

`~/repos/iss/terraform-iss` is a local git repo with no remote, using
Terraform 1.5.6 with **local state** (`terraform.tfstate`, serial 136, last
applied 2023-08). It holds:

| File | Resources |
|---|---|
| `ec2.tf` | `aws_instance.iss-prod` (`i-02c4d00dffd268f8e`, t3.small, us-west-2d), `aws_eip.iss-eip` (52.13.20.236), **`aws_ebs_volume.iss-prod` (`vol-00239ae1d5c7a8bd6`, 30 GiB, never attached: there is no `aws_volume_attachment`)**, **`aws_ebs_snapshot.iss-prod` (`snap-09d07a4c38743d318`, a snapshot of that empty volume, not of the root disk)**, two CloudWatch alarms |
| `vpc.tf` | VPC, subnet, IGW, route table, `aws_security_group.iss-sg` |
| `digitalocean_dns.tf` | the DigitalOcean zone and records |
| `providers.tf` | aws (us-west-2) + digitalocean |

`~/repos/iss/ansible-iss` clones `plausible-docker-compose` to `~/hosting`.

This module is a **separate root module with its own state**, built on the
same conventions (us-west-2, `Environment = Production` tags). The legacy
stack can then be destroyed on its own afterwards (RUNBOOK phase 5) without
touching the new one. Suggested home: copy this directory to
`~/repos/iss/terraform-iss/edge/` (or a new `iss` remote) once reviewed.

## Files

| File | What |
|---|---|
| `versions.tf` | Terraform ≥1.10, aws ~> 6.73, cloudflare ~> 5.29, S3 backend (partial config) |
| `variables.tf` | All knobs; defaults are the recommendation |
| `lightsail.tf` | Key pair (public key only), instance, static IP, firewall |
| `cloud-init.sh.tftpl` | First-boot bootstrap: swap, QUIC sysctls, Docker, compose, cloudflared and tailscale (installed, **not** authenticated), dnf-automatic security updates |
| `cloudflare.tf` | Zone, MX/TXT carried over, `p`, apex/www for both site phases, redirect rule, zone settings |
| `outputs.tf` | Static IP, Cloudflare name servers |
| `backend.hcl.example`, `terraform.tfvars.example` | Copy, fill in, keep out of git (`.gitignore` covers both) |

## Decisions

**Credentials never enter Terraform.**
- AWS and Cloudflare credentials come from the environment (`AWS_PROFILE`,
  `CLOUDFLARE_API_TOKEN`).
- The Cloudflare Tunnel token and the Tailscale login are entered on the box
  once (RUNBOOK phase 2). They'd otherwise sit in state *and* in Lightsail's
  stored user data, readable by anyone with `lightsail:GetInstance`.
- The key pair takes a public key only. Letting Terraform generate it would
  put the private key in state.

**What is in state** (none of it secret, but treat it as internal):
- the instance, its user data (the bootstrap script, no secrets) and its IPs
- the static IP
- the zone ID and name servers, and every DNS record (MX and verification
  TXT values are public in DNS anyway)
- zone settings and the redirect ruleset
- the Cloudflare account ID

**State backend: S3 in the same AWS account, versioned, with native locking.**
- `use_lockfile = true` needs Terraform ≥1.10, so no DynamoDB table.
- Cost is cents per month.
- Bootstrap commands are in RUNBOOK phase 0.
- HCP Terraform's free tier would also work, but it is one more vendor.
- Don't keep state local like `terraform-iss` does. That state is on one
  laptop and has drifted: it still lists a CAA record that isn't published.

**`p.infosecstreams.com` is DNS only (`p_proxied = false`).**
- Caddy then terminates TLS itself and serves HTTP/3, with its own certificate.
- Plausible sees real client IPs. Proxied, it would see Cloudflare's IPs unless
  Caddy trusted Cloudflare's ranges and read `CF-Connecting-IP`; Plausible's
  unique-visitor counting and geo depend on that.
- Twitch webhooks and the 15-second live API skip an extra hop and cache layer.
- What proxying would buy: origin-IP hiding, DDoS absorption, and edge caching
  of `/streamstatus/*`. Revisit if the box is ever attacked. The flip is one
  variable, plus Caddy `trusted_proxies` config.

**Site deploys: wrangler from GitHub Actions; Terraform for the binding.**
- Terraform owns the slow-changing parts: the zone, `www`, the redirect and
  the `cloudflare_workers_custom_domain` that binds the apex to the Worker.
- wrangler (`deploy/edge/site/`) owns the Worker's *content*. The site
  rebuilds on every push and on the daily regen:
  - wrangler uploads only changed files, applies `_headers`, and deploys
    atomically.
  - With Terraform's `cloudflare_workers_script` instead, every content change
    would be a `terraform apply` in CI. CI would need the state backend and a
    broad token, and state would churn with asset hashes on every deploy.
- So `site/wrangler.jsonc` has **no `routes`**. The custom domain lives in
  Terraform only, so the two tools never fight over it.

**Lightsail details.**
- `small_3_0` is the $12, 2 GB bundle. Confirm with
  `aws lightsail get-bundles --region us-west-2 --query "bundles[?bundleId=='small_3_0']"`
  before applying.
- Amazon Linux 2023 is the same OS as today, so `/home/ec2-user/hosting`
  paths in compose and the deploy script carry over.
- Dual-stack.
- Daily automatic snapshots kept 7 days (incremental, billed per GB-month;
  the data is ~120 MB plus the OS). Set `auto_snapshot_time = ""` to turn them
  off.
- Firewall:
  - 80/tcp, 443/tcp and 443/udp open to all.
  - 22 only from Lightsail's browser console plus optional break-glass CIDRs.
  - No 8080: backspin is reached through the tunnel, on loopback.

**Not managed here** (and why):
- The Cloudflare Tunnel's routes. They are remotely managed in the dashboard
  (Zero Trust → Networks → Tunnels), and the tunnel lives in the account's
  other zone, highvibes.cloud. Importing them is possible later with
  `cloudflare_zero_trust_tunnel_cloudflared_config`.
- The site Worker's content (wrangler, above).
- DNSSEC. It's easy to add (`cloudflare_zone_dnssec` plus a DS record at
  Namecheap), but it's a registrar step, so it's a follow-up.
