variable "region" {
  description = "AWS region for Lightsail. us-west-2 keeps the box next to the current one."
  type        = string
  default     = "us-west-2"
}

variable "availability_zone" {
  type    = string
  default = "us-west-2a"
}

variable "instance_name" {
  type    = string
  default = "iss-prod-ls"
}

variable "bundle_id" {
  description = "Lightsail plan. small_3_0 = 2 GB RAM, 2 vCPU, 60 GB SSD, 3 TB transfer, IPv4 included ($12/month). medium_3_0 = 4 GB ($24). Check with `aws lightsail get-bundles --region us-west-2`."
  type        = string
  default     = "small_3_0"
}

variable "blueprint_id" {
  description = "Amazon Linux 2023, the same OS as the current EC2 host, so the ec2-user paths in compose and deploy scripts stay valid."
  type        = string
  default     = "amazon_linux_2023"
}

variable "ssh_public_key" {
  description = "OpenSSH public key for ec2-user (public key only; the private key never touches Terraform)."
  type        = string
}

variable "admin_ssh_cidrs" {
  description = "Extra IPv4 CIDRs allowed to reach port 22 (break-glass). Day-to-day SSH goes over Tailscale. Lightsail's browser SSH (lightsail-connect) is always allowed."
  type        = list(string)
  default     = []
}

variable "auto_snapshot_time" {
  description = "UTC HH:00 for Lightsail's daily automatic snapshot (kept 7 days, billed per GB-month). Empty string disables it."
  type        = string
  default     = "10:00"
}

variable "compose_version" {
  type    = string
  default = "v5.8.0"
}

variable "compose_sha256" {
  description = "sha256 of docker-compose-linux-x86_64 for compose_version (from the release's .sha256 file)."
  type        = string
  default     = "b8840f5008c447f41fce0a57ae3adeab42c89b1eb6e6012524bcabbfb4ffbaf6"
}

variable "cloudflare_account_id" {
  description = "Cloudflare account ID (the account that already holds highvibes.cloud and the tunnel). Not secret."
  type        = string
}

variable "zone_name" {
  type    = string
  default = "infosecstreams.com"
}

variable "p_record_target" {
  description = "Where p.infosecstreams.com points. \"lightsail\" (the new static IP) or \"ec2\" (the old Elastic IP, for the import/rollback phase)."
  type        = string
  default     = "ec2"
  validation {
    condition     = contains(["lightsail", "ec2"], var.p_record_target)
    error_message = "p_record_target must be \"lightsail\" or \"ec2\"."
  }
}

variable "ec2_elastic_ip" {
  description = "The current EC2 Elastic IP (rollback target for p)."
  type        = string
  default     = "52.13.20.236"
}

variable "p_proxied" {
  description = "Proxy p.infosecstreams.com through Cloudflare. Recommended false: see README.md."
  type        = bool
  default     = false
}

variable "p_ttl" {
  description = "TTL for p. Keep 60 around the cutover; raise to 1 (auto = 300) afterwards."
  type        = number
  default     = 60
}

variable "site_worker_name" {
  description = "Name of the Workers static-assets script deployed by GitHub Actions (site/wrangler.jsonc `name`)."
  type        = string
  default     = "infosecstreams-site"
}

variable "site_on_workers" {
  description = "false while GitHub Pages still serves the site (apex A records to Pages). true once the Worker exists (first `wrangler deploy` done): the apex becomes a Workers custom domain."
  type        = bool
  default     = false
}
