output "lightsail_static_ip" {
  value = aws_lightsail_static_ip.iss.ip_address
}

output "lightsail_ipv6" {
  value = aws_lightsail_instance.iss.ipv6_addresses
}

output "cloudflare_name_servers" {
  description = "Set these at Namecheap (Domain List → infosecstreams.com → Nameservers → Custom DNS)."
  value       = cloudflare_zone.iss.name_servers
}

output "p_points_to" {
  value = "${local.p_ip} (${var.p_record_target}, proxied=${var.p_proxied})"
}
