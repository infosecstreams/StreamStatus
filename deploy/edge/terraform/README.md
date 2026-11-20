# Terraform

The Terraform for the new stack (Lightsail 2 GB box + the Cloudflare zone)
and its migration runbook live with the existing infrastructure code. That's
`~/repos/iss/terraform-iss`, directory `edge/`, branch `infra-edge`
(worktree `~/repos/worktrees/terraform-iss-edge`):

- `edge/README.md`: design, what is in state, the S3 backend
- `edge/RUNBOOK.md`: phases 0–5 (EC2 → Lightsail, DigitalOcean → Cloudflare,
  site → Workers, decommission of the old server with its disk and Elastic IP),
  each with rollback

`iss-sg-quic.tf.snippet` here is only for running Caddy on the old EC2 box
(UDP 443 in its security group). It isn't needed once on Lightsail.
