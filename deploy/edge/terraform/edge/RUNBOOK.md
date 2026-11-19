# Runbook: EC2 → Lightsail (with Caddy) and DigitalOcean DNS → Cloudflare

Order matters. Each phase is independently reversible, and only phase 3 is
user-visible. Phases 1 and 2 can happen days before it.

| Phase | What | User-visible | Rollback |
|---|---|---|---|
| 0 | State bucket, tokens, tfvars | no | n/a |
| 1 | Cloudflare zone mirrors DigitalOcean; nameservers move | no | nameservers back to DO |
| 2 | Lightsail box built, Caddy holds a real cert, rehearsal | no | `terraform destroy` the box |
| 3 | **Cutover**: data copied, `p` → Lightsail | ~10–15 min of Plausible/webhook downtime | `p` back to EC2, restart old stack |
| 4 | Site → Cloudflare Workers | no (DNS swap) | `site_on_workers=false` |
| 5 | Decommission EC2, orphan EBS, DO zone, legacy terraform | no | — (only after a quiet week) |

Shorthand below: **old** = `ssh iss` (EC2, 52.13.20.236).
**new** = the Lightsail box (`ssh iss-ls` over Tailscale after phase 2).
Run Terraform from `deploy/edge/terraform/edge`.

## DNS inventory (2026-11-19, via DNS-over-HTTPS)

Plain UDP `dig` from the owner's network drops TXT/MX answers for this zone.
Use `dig +tcp` or `https://dns.google/resolve?name=…&type=…`.

| Name | Type | Value | TTL |
|---|---|---|---|
| infosecstreams.com | A | 185.199.108–111.153 (GitHub Pages) | 300 |
| infosecstreams.com | MX | 1 aspmx / 5 alt1, alt2 / 10 alt3, alt4 .aspmx.l.google.com | 14400 |
| infosecstreams.com | TXT | google-site-verification=wo2Eozl2S_…, google-site-verification=L7zkDNvc… | 3600 |
| www | CNAME | infosecstreams.github.io | 300 |
| p | A | 52.13.20.236 | 1800 |
| _github-challenge-infosecstreams-organization | TXT | 182225866e | 3600 |

All of these are in `cloudflare.tf`. Not published, and not added:
- **CAA**: the legacy terraform state lists one; it has drifted.
- **SPF, DMARC, DKIM**: absent today, so mail from @infosecstreams.com can be
  spoofed. A good follow-up is `v=spf1 include:_spf.google.com ~all`, a
  `_dmarc` `p=none` record to start, and Google's DKIM key. It's a mail
  change, so it's left to the owner.

Before phase 1, re-run the inventory in case anything was added since.

## Phase 0: state backend, credentials, variables (owner)

1. **State bucket** (once, cents a month):
   ```bash
   export AWS_PROFILE=<admin profile> AWS_REGION=us-west-2
   ACCOUNT=$(aws sts get-caller-identity --query Account --output text)
   B=infosecstreams-tfstate-$ACCOUNT
   aws s3api create-bucket --bucket "$B" --create-bucket-configuration LocationConstraint=us-west-2
   aws s3api put-bucket-versioning --bucket "$B" --versioning-configuration Status=Enabled
   aws s3api put-public-access-block --bucket "$B" --public-access-block-configuration \
     BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true
   # New buckets are SSE-S3 encrypted by default.
   ```
   Copy `backend.hcl.example` to `backend.hcl`, set the bucket name, then run
   `terraform init -backend-config=backend.hcl`.
2. **Cloudflare API token for Terraform** (dashboard → My Profile → API
   Tokens → Create Custom Token). Scope it to the account:
   - Zone: Zone Edit
   - Zone: Zone Settings Edit
   - Zone: DNS Edit
   - Zone: Dynamic Redirect Edit (shown as Single Redirect in some UIs)
   - Account: Workers Scripts Edit (for the custom domain)

   Export it as `CLOUDFLARE_API_TOKEN` only in the shell that runs Terraform.
   It is separate from the narrower GitHub Actions token in
   `deploy/edge/site/`. *Permission names drift: if `plan` gets a 403,
   the error names the missing one.*
3. **The Cloudflare account** that already holds `highvibes.cloud` and the
   tunnel is the one to use. Its ID is on the dashboard's Overview sidebar
   (`cloudflare_account_id`).
4. Copy `terraform.tfvars.example` to `terraform.tfvars`. Set
   `ssh_public_key`, `cloudflare_account_id`, and your current IP as a /32 in
   `admin_ssh_cidrs` for first login.

## Phase 1: Cloudflare zone (no user-visible change)

1. `terraform apply -target=cloudflare_zone.iss` creates the zone. Then
   `terraform apply` creates every record **exactly as on DigitalOcean**, plus
   settings and the redirect rule (inert while `www` is DNS only). The
   defaults keep `p_record_target = "ec2"` and `site_on_workers = false`.
   - `p` gets TTL 60 here, so phase 3's switch propagates within a minute.
   - The `-target` step isn't strictly needed, but it surfaces token problems
     early.
2. Compare the zones. Both lists must match the inventory:
   ```bash
   for t in A MX TXT CNAME; do dig +tcp +short @<cf-ns-1> infosecstreams.com $t; done
   dig +tcp +short @<cf-ns-1> p.infosecstreams.com A
   ```
3. **Namecheap** → Domain List → infosecstreams.com → Nameservers → Custom
   DNS: enter the two `cloudflare_name_servers` from `terraform output`.
   - The `.com` delegation TTL is 48 h. Wait until the Cloudflare dashboard
     shows the zone **Active**.
   - Check mail still delivers: send a message to an @infosecstreams.com
     address.
4. Rollback: put `ns1/ns2/ns3.digitalocean.com` back at Namecheap. The DO
   zone is untouched until phase 5.

## Phase 2: build the Lightsail box (no user-visible change)

1. `terraform apply`. It creates the instance, static IP and firewall. `p`
   still points at EC2.
2. First login (port 22 is open only to `admin_ssh_cidrs` and Lightsail's
   browser console):
   ```bash
   IP=$(terraform output -raw lightsail_static_ip)
   ssh ec2-user@$IP 'cat /var/lib/iss-bootstrap.done && docker-compose version && free -m'
   ```
3. **Join Tailscale.** The login runs interactively, so no key is stored
   anywhere:
   ```bash
   ssh -t ec2-user@$IP 'sudo tailscale up --ssh --hostname iss-ls'
   ```
   Open the printed URL and approve. In the Tailscale admin console, disable
   key expiry for `iss-ls`. From now on, use `ssh ec2-user@iss-ls`. Then set
   `admin_ssh_cidrs = []` and apply again (break-glass stays available
   through Lightsail's browser SSH).
4. **Bring the stack's files over** (config only, not data yet), relayed
   through your laptop:
   ```bash
   ssh iss 'tar czf - -C /home/ec2-user --exclude=hosting/nginx_access.log hosting backspin' \
     | ssh iss-ls 'tar xzf - -C /home/ec2-user'
   ```
   This includes the env files (secrets). They go laptop-to-laptop over SSH
   and are never written locally.
5. **Switch the compose file to Caddy** on new (`~/hosting/docker-compose.yml`):
   - Add the `caddy` service and volumes from
     `deploy/edge/docker-compose.caddy.yml`, and copy
     `deploy/edge/caddy/Caddyfile` to `~/hosting/caddy/Caddyfile`.
   - Delete the `nginx` and `letsencrypt` services and the `acme-challenge`
     volume. On this box there is nothing to roll back to.
   - Change `depends_on: nginx` to `caddy` in `plausible` and `streamstatus`.
   - **Port 8080 belongs to backspin on loopback** (the tunnel's target is
     `http://127.0.0.1:8080`). Caddy publishes `127.0.0.1:8080:8080`. If the
     `streamstatus` service publishes `127.0.0.1:8080:8080` (ci-deploy
     snippet), change it to `127.0.0.1:18080:8080`, and set
     `HEALTH_URL=http://127.0.0.1:18080/streamstatus/status.json` in
     `deploy-streamstatus`.
   - Then run `docker-compose config -q`, and
     `docker-compose pull && docker-compose create` (this creates the named
     volumes with compose labels; nothing starts).
6. **Get Caddy its real certificate before the cutover.** On **old**, route
   ACME challenges for p to the new box, in the running nginx container only
   (the same trick as `../../CUTOVER-caddy.md` step 3):
   ```bash
   # in the `listen 80` server block for p.infosecstreams.com:
   #   location ^~ /.well-known/acme-challenge/ { proxy_set_header Host $host; proxy_pass http://<lightsail static ip>; }
   docker cp hosting_nginx_1:/etc/nginx/conf.d/nginx.default.conf /tmp/n.conf   # edit as above
   docker cp /tmp/n.conf hosting_nginx_1:/etc/nginx/conf.d/nginx.default.conf
   docker exec hosting_nginx_1 nginx -t && docker exec hosting_nginx_1 nginx -s reload
   ```
   On **new**, run `docker-compose up -d caddy` and wait for
   `docker-compose logs -f caddy | grep -m1 'certificate obtained successfully'`.
   (certbot renewals on old are not due before 2026-12-20, so its challenge
   path isn't needed meanwhile.)
7. **Tunnel connector on new** (the tunnel keeps serving from old too; two
   connectors are normal replicas). In Zero Trust → Networks → Tunnels →
   (the tunnel with the `backspin.highvibes.cloud` route) → Configure →
   install connector → copy the token, then on new:
   `sudo cloudflared service install <token>`. Paste it interactively; it
   ends up only in the systemd unit on the box. The dashboard should then
   show 2 connectors.
   - Note: the tunnel's only route is `backspin.highvibes.cloud →
     http://127.0.0.1:8080`, but public DNS for backspin.highvibes.cloud
     currently resolves to 51.77.81.230, not a Cloudflare tunnel address. The
     route may be unused. Check the highvibes.cloud DNS in the dashboard
     before relying on it.
8. **Rehearsal** (optional, recommended): do steps 2–5 of phase 3 against a
   `pg_dumpall` taken from the live old box, start the stack, and test with
   `curl --resolve p.infosecstreams.com:443:<static ip> …`. Then
   `docker-compose down` and delete the volumes again. **Do not start
   `streamstatus` during a rehearsal** (see phase 3, "Never two bots").
9. Rollback: `terraform destroy -target=aws_lightsail_instance_public_ports.iss -target=aws_lightsail_static_ip_attachment.iss -target=aws_lightsail_static_ip.iss -target=aws_lightsail_instance.iss -target=aws_lightsail_key_pair.admin`
   removes the box, and the zone stays.

## Phase 3: cutover (~10–15 min; pick a quiet hour)

The data is small: Postgres 71 MB, ClickHouse 13 MB, Plausible data 30 MB
(measured 2026-11-19). Same images on both sides (Postgres 18.7, ClickHouse
25.11.9.34, Plausible CE v3.2.1) and the same CPU architecture (x86_64), so
**stopped-volume copies are exact and safe**. A logical dump of Postgres is
taken as well, as insurance.

**Never two bots.** Two running `streamstatus` instances would both commit
status to the site repo and both answer EventSub. Stop old's bot before new's
starts.

1. **Freeze old** (Plausible ingestion and webhooks pause here; Twitch
   retries webhook deliveries, and the bot's 5-minute reconcile catches up
   afterwards):
   ```bash
   ssh iss 'cd ~/hosting && docker-compose stop streamstatus plausible && \
     docker-compose exec -T plausible_db pg_dumpall -U postgres | gzip > ~/backups/pg-$(date -u +%Y%m%dT%H%MZ).sql.gz && \
     docker-compose stop plausible_events_db plausible_db'
   ```
2. **Copy the volumes** old → laptop → new. Volume names are
   `<project>_<name>`, and the project is `hosting` on both. Check with
   `docker volume ls`. `streamstatus-data` exists only once bot-live-data has
   shipped; skip it if absent.
   ```bash
   for v in db-data-pg18 event-data plausible-data streamstatus-data; do
     ssh iss "docker volume inspect hosting_$v >/dev/null 2>&1 && docker run --rm -v hosting_$v:/v:ro alpine tar -C /v -czf - ." \
       | ssh iss-ls "docker run --rm -i -v hosting_$v:/v alpine sh -c 'tar -C /v -xzf -'" || echo "skip $v"
   done
   ```
   The bot's site checkout lives in the container and is re-cloned on start.
3. **Start new** (Caddy is already up with its certificate):
   ```bash
   ssh iss-ls 'cd ~/hosting && docker-compose up -d && docker-compose ps'
   ```
   Test before switching DNS:
   ```bash
   for u in / /js/script.js /api/health /streamstatus/status.json; do
     curl -s -o /dev/null -w "%{http_code} $u\n" -A Mozilla/5.0 --resolve p.infosecstreams.com:443:<static ip> https://p.infosecstreams.com$u
   done
   ```
   Log in to the Plausible dashboard through `--resolve` (or a hosts-file
   entry). Yesterday's numbers must be there.
4. **Switch DNS:** `terraform apply -var p_record_target=lightsail` (or set it
   in tfvars). TTL is 60 s. Watch with
   `watch -n5 dig +short p.infosecstreams.com @1.1.1.1`.
5. **Verify:**
   - `deploy/edge/measure.sh https://p.infosecstreams.com/js/script.js` shows
     h3, gzip, an ECDSA cert, and `server` gone.
   - The Plausible realtime view shows visitors.
   - The bot logs EventSub notifications. A test is the next
     `stream.online`, or check Twitch's subscription status:
     `GET /helix/eventsub/subscriptions?status=webhook_callback_verification_failed`
     should be empty.
   - `curl https://backspin.highvibes.cloud/` through the tunnel, if that
     route is in use.
6. **Old box:**
   - Stop the remaining containers: `docker-compose stop`. Keep the
     instance **running** for 48 h; nginx stays up but now gets no traffic.
   - Then stop (not terminate) it: `aws ec2 stop-instances --instance-ids i-02c4d00dffd268f8e`.
   - Also stop cloudflared and tailscaled on old before stopping
     (`sudo systemctl stop cloudflared`), so the tunnel shows only the new
     connector.
   - Keep the stopped instance for a week. Stopped, it costs EBS ($2.40) and
     the Elastic IP ($3.65).

**Rollback (within the week):**
1. Copy back any Plausible data you want to keep. Same loop, reversed
   direction, after `docker-compose stop` on new.
2. On old: `aws ec2 start-instances …`, then `cd ~/hosting && docker-compose up -d`.
3. `terraform apply -var p_record_target=ec2`.
4. On new: `docker-compose stop streamstatus` (never two bots).

## Phase 4: site → Cloudflare Workers static assets

1. Merge the site PR (`deploy/edge/site/`: `_headers`, `wrangler.jsonc`
   **without routes**, the `deploy-cloudflare` job), and set the repo
   variable `SITE_HOST=both`.
2. The first `deploy-cloudflare` run creates the Worker
   `infosecstreams-site`, which isn't public yet.
3. Bind the apex. This is two applies, because Cloudflare refuses a custom
   domain while other apex A records exist:
   ```bash
   terraform apply -var site_on_workers=true -target='cloudflare_dns_record.apex_pages'   # removes the 4 Pages A records
   terraform apply -var site_on_workers=true                                              # custom domain + proxied www
   ```
   The gap between the two is seconds. Resolvers keep the cached Pages answer
   for its 300 s TTL, so nobody notices. Set `site_on_workers = true` in
   tfvars.
4. Verify with `deploy/edge/measure.sh https://infosecstreams.com/` and
   `curl -sI https://www.infosecstreams.com/` (expect a 301 to the apex). See
   `deploy/edge/site/MIGRATION-cloudflare.md` for the full checks.
5. After two quiet weeks: `SITE_HOST=cloudflare`, switch off GitHub Pages,
   and delete `site/public/CNAME`.
6. Rollback: `terraform apply -var site_on_workers=false`. The Pages A records
   return and the custom domain goes. Pages kept deploying the whole time
   (`both`).

## Phase 5: decommission (after the week)

### 5a. Confirm and delete the orphan EBS volume and snapshot

The legacy code makes the case:
- `aws_ebs_volume.iss-prod` was created blank (no `snapshot_id`) and is never
  attached: there is no `aws_volume_attachment`, and `lsblk` on the instance
  shows only the root disk.
- `aws_ebs_snapshot.iss-prod` is a snapshot **of that volume**, not of the
  root disk.

Confirm in AWS before deleting anything. The host role can't describe
volumes, so use your admin profile:

```bash
export AWS_PROFILE=<admin profile> AWS_REGION=us-west-2
# 1. The volume: State must be "available" and Attachments [].
aws ec2 describe-volumes --volume-ids vol-00239ae1d5c7a8bd6 \
  --query 'Volumes[].{id:VolumeId,state:State,size:Size,az:AvailabilityZone,created:CreateTime,fromSnapshot:SnapshotId,attachments:Attachments,tags:Tags}'
# 2. The snapshot: VolumeId must be vol-00239ae1d5c7a8bd6 (the orphan), not the root volume.
aws ec2 describe-snapshots --snapshot-ids snap-09d07a4c38743d318 \
  --query 'Snapshots[].{id:SnapshotId,volume:VolumeId,size:VolumeSize,started:StartTime,description:Description,state:State}'
# 3. For comparison, the instance's real root volume:
aws ec2 describe-instances --instance-ids i-02c4d00dffd268f8e \
  --query 'Reservations[].Instances[].BlockDeviceMappings[].{dev:DeviceName,vol:Ebs.VolumeId}'
# 4. Anything else unattached or forgotten in the account:
aws ec2 describe-volumes --filters Name=status,Values=available --query 'Volumes[].[VolumeId,Size,CreateTime]' --output table
aws ec2 describe-snapshots --owner-ids self --query 'Snapshots[].[SnapshotId,VolumeId,VolumeSize,StartTime,Description]' --output table
# 5. Optional: was it ever attached in the last 90 days?
aws cloudtrail lookup-events --lookup-attributes AttributeKey=ResourceName,AttributeValue=vol-00239ae1d5c7a8bd6 \
  --query 'Events[].[EventTime,EventName,Username]' --output table
```

Console equivalent:
1. EC2 → Elastic Block Store → **Volumes** → search `vol-00239ae1d5c7a8bd6`.
   Volume state must be **Available** and "Attached resources" empty.
2. **Snapshots** → `snap-09d07a4c38743d318`: its "Volume" must be
   `vol-00239ae1d5c7a8bd6`.
3. Instances → `i-02c4d00dffd268f8e` → Storage: the root device must be a
   *different* volume.

If all checks hold, delete the pair through the legacy Terraform, so its
state stays true. Back up the state first; Terraform 1.16 will upgrade the
1.5.6 state format, and that can't be undone:

```bash
cd ~/repos/iss/terraform-iss && cp terraform.tfstate terraform.tfstate.pre-cleanup
# In ec2.tf: set final_snapshot = false on aws_ebs_volume.iss-prod (otherwise
# destroying it creates a new snapshot), then delete both resource blocks.
terraform plan -var-file=vars.tfvars   # expect exactly: 2 to destroy (volume, snapshot)
terraform apply -var-file=vars.tfvars
```

CLI alternative:
```bash
aws ec2 delete-snapshot --snapshot-id snap-09d07a4c38743d318
aws ec2 delete-volume --volume-id vol-00239ae1d5c7a8bd6
terraform state rm aws_ebs_snapshot.iss-prod aws_ebs_volume.iss-prod   # then delete the blocks
```

The saving is $2.40/month for the volume. The snapshot of a never-written
volume stores almost nothing.

### 5b. Retire the EC2 stack and DigitalOcean

1. Optional archive: take a final snapshot of the root volume
   (~$0.05/GB-month, until you delete it).
2. In `terraform-iss`, destroy:
   - the instance, EIP and association
   - the security group, subnet, route table, IGW and VPC
   - the CloudWatch alarms (they watch the old instance ID)
   - all `digitalocean_*` resources (deletes the DO zone; only after phase 1's
     nameserver move)

   Note `delete_on_termination = false` on the root disk: after the instance
   is gone, delete that volume too (EC2 → Volumes, state Available).
3. Remove the old `iss` node in the Tailscale admin console.
4. Archive `terraform-iss`. Move this module into the `iss` repo, which
   becomes the single source of truth.

Monthly after phase 5: **Lightsail $12** (plus a few cents for snapshots
and the state bucket), down from ~$21–24.
