# The new box: Lightsail 2 GB, Amazon Linux 2023, a static IP, and the
# firewall. The bootstrap (cloud-init) installs Docker, compose, cloudflared
# and tailscale but authenticates nothing: tunnel and tailnet credentials are
# entered on the box once (RUNBOOK.md), so they never land in Terraform state
# or in Lightsail's stored user data.

resource "aws_lightsail_key_pair" "admin" {
  name       = "${var.instance_name}-admin"
  public_key = var.ssh_public_key
}

resource "aws_lightsail_instance" "iss" {
  name              = var.instance_name
  availability_zone = var.availability_zone
  blueprint_id      = var.blueprint_id
  bundle_id         = var.bundle_id
  key_pair_name     = aws_lightsail_key_pair.admin.name
  ip_address_type   = "dualstack"

  user_data = templatefile("${path.module}/cloud-init.sh.tftpl", {
    compose_version = var.compose_version
    compose_sha256  = var.compose_sha256
  })

  dynamic "add_on" {
    for_each = var.auto_snapshot_time == "" ? [] : [1]
    content {
      type          = "AutoSnapshot"
      snapshot_time = var.auto_snapshot_time
      status        = "Enabled"
    }
  }

  lifecycle {
    # user_data only runs on first boot; a later edit must not replace the
    # box (and its data). Re-run the bootstrap by hand instead.
    ignore_changes = [user_data]
  }
}

resource "aws_lightsail_static_ip" "iss" {
  name = "${var.instance_name}-ip"
}

resource "aws_lightsail_static_ip_attachment" "iss" {
  static_ip_name = aws_lightsail_static_ip.iss.name
  instance_name  = aws_lightsail_instance.iss.name
}

# Replaces Lightsail's default firewall (22 and 80 open to the world).
resource "aws_lightsail_instance_public_ports" "iss" {
  instance_name = aws_lightsail_instance.iss.name

  # SSH: Lightsail browser console plus optional break-glass CIDRs. Normal
  # admin access and CI deploys go over Tailscale (tailscale0 bypasses this
  # firewall: it is outbound-initiated WireGuard on UDP 41641).
  port_info {
    protocol          = "tcp"
    from_port         = 22
    to_port           = 22
    cidrs             = var.admin_ssh_cidrs
    cidr_list_aliases = ["lightsail-connect"]
  }

  # HTTP (ACME HTTP-01 + redirect to HTTPS), HTTPS, and HTTP/3 (QUIC).
  port_info {
    protocol   = "tcp"
    from_port  = 80
    to_port    = 80
    cidrs      = ["0.0.0.0/0"]
    ipv6_cidrs = ["::/0"]
  }
  port_info {
    protocol   = "tcp"
    from_port  = 443
    to_port    = 443
    cidrs      = ["0.0.0.0/0"]
    ipv6_cidrs = ["::/0"]
  }
  port_info {
    protocol   = "udp"
    from_port  = 443
    to_port    = 443
    cidrs      = ["0.0.0.0/0"]
    ipv6_cidrs = ["::/0"]
  }
  # No 8080: backspin is reached through the Cloudflare Tunnel on loopback.
}
