# infosecstreams.com on Cloudflare (Free plan). Terraform owns the zone, its
# records and settings, the www→apex redirect, and the apex's binding to the
# site Worker. GitHub Actions (wrangler) owns the Worker's content: every
# deploy uploads the Astro build. See README.md "Why wrangler for the site".

resource "cloudflare_zone" "iss" {
  account = { id = var.cloudflare_account_id }
  name    = var.zone_name
  type    = "full"
}

locals {
  zone_id = cloudflare_zone.iss.id

  # Carried over from DigitalOcean (inventory 2026-11-19 via DNS-over-HTTPS,
  # see RUNBOOK.md "DNS inventory"). Google Workspace mail.
  mx = {
    "aspmx.l.google.com"      = 1
    "alt1.aspmx.l.google.com" = 5
    "alt2.aspmx.l.google.com" = 5
    "alt3.aspmx.l.google.com" = 10
    "alt4.aspmx.l.google.com" = 10
  }
  apex_txt = [
    "google-site-verification=wo2Eozl2S_TMsOSMZBxmpF6lTgghkUyKGhcjNp4TETM",
    "google-site-verification=L7zkDNvcgcYM5ZJmQa8-TF2rgyOI_1FcEOQ91NPd37g",
  ]
  github_pages_ips = ["185.199.108.153", "185.199.109.153", "185.199.110.153", "185.199.111.153"]

  p_ip = var.p_record_target == "lightsail" ? aws_lightsail_static_ip.iss.ip_address : var.ec2_elastic_ip
}

# --- Mail and verification (unchanged from DigitalOcean) ---

resource "cloudflare_dns_record" "mx" {
  for_each = local.mx
  zone_id  = local.zone_id
  name     = var.zone_name
  type     = "MX"
  content  = each.key
  priority = each.value
  ttl      = 3600
}

resource "cloudflare_dns_record" "apex_txt" {
  for_each = toset(local.apex_txt)
  zone_id  = local.zone_id
  name     = var.zone_name
  type     = "TXT"
  content  = "\"${each.value}\""
  ttl      = 3600
}

resource "cloudflare_dns_record" "github_org_challenge" {
  zone_id = local.zone_id
  name    = "_github-challenge-infosecstreams-organization.${var.zone_name}"
  type    = "TXT"
  content = "\"182225866e\""
  ttl     = 3600
}

# CAA: none is published today. If added, it must allow every issuer in use:
# letsencrypt.org (Caddy on p), and Cloudflare's edge CAs (pki.goog, ssl.com,
# letsencrypt.org). Cloudflare adds its own CAA entries automatically when
# a zone has CAA records and Universal SSL is on.

# --- p.infosecstreams.com: the Lightsail box (Caddy: Plausible, bot API, webhook) ---

resource "cloudflare_dns_record" "p" {
  zone_id = local.zone_id
  name    = "p.${var.zone_name}"
  type    = "A"
  content = local.p_ip
  proxied = var.p_proxied
  ttl     = var.p_proxied ? 1 : var.p_ttl
  comment = "Lightsail ${var.instance_name} (Caddy). DNS only: Caddy terminates TLS and HTTP/3."
}

# --- The site ---

# Phase 1 (site_on_workers = false): the apex still points at GitHub Pages,
# DNS only, exactly as on DigitalOcean.
resource "cloudflare_dns_record" "apex_pages" {
  for_each = var.site_on_workers ? toset([]) : toset(local.github_pages_ips)
  zone_id  = local.zone_id
  name     = var.zone_name
  type     = "A"
  content  = each.value
  proxied  = false
  ttl      = 300
}

# Phase 2 (site_on_workers = true): the apex is a Workers custom domain.
# Cloudflare creates and manages the apex DNS record and certificate itself.
# The Worker must exist first (one `wrangler deploy` from the site workflow).
resource "cloudflare_workers_custom_domain" "apex" {
  count      = var.site_on_workers ? 1 : 0
  account_id = var.cloudflare_account_id
  zone_id    = local.zone_id
  hostname   = var.zone_name
  service    = var.site_worker_name
}

# www: phase 1 keeps the GitHub Pages CNAME (DNS only); phase 2 makes it a
# proxied CNAME to the apex so the redirect rule below can answer.
resource "cloudflare_dns_record" "www" {
  zone_id = local.zone_id
  name    = "www.${var.zone_name}"
  type    = "CNAME"
  content = var.site_on_workers ? var.zone_name : "infosecstreams.github.io"
  proxied = var.site_on_workers
  ttl     = var.site_on_workers ? 1 : 300
}

resource "cloudflare_ruleset" "redirects" {
  zone_id     = local.zone_id
  name        = "redirects"
  description = "www to apex"
  kind        = "zone"
  phase       = "http_request_dynamic_redirect"
  rules = [{
    ref         = "www_to_apex"
    description = "Redirect www.infosecstreams.com to the apex"
    expression  = "(http.host eq \"www.${var.zone_name}\")"
    action      = "redirect"
    action_parameters = {
      from_value = {
        status_code           = 301
        preserve_query_string = true
        target_url = {
          expression = "concat(\"https://${var.zone_name}\", http.request.uri.path)"
        }
      }
    }
  }]
}

# --- Zone settings (Free plan) ---

locals {
  zone_settings = {
    ssl                      = "strict" # Full (strict): only matters for proxied records
    always_use_https         = "on"
    automatic_https_rewrites = "on"
    min_tls_version          = "1.2"
    tls_1_3                  = "on"
    http3                    = "on"
    "0rtt"                   = "on"
    early_hints              = "on"
  }
}

resource "cloudflare_zone_setting" "iss" {
  for_each   = local.zone_settings
  zone_id    = local.zone_id
  setting_id = each.key
  value      = each.value
}
