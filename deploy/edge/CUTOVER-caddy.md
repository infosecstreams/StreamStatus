# Cutover runbook: nginx + certbot → Caddy on the EC2 host

Audience: the coordinator, on the host (`ssh iss`, `cd ~/hosting`). Expected
interruption: **2–4 seconds** of refused connections on p.infosecstreams.com,
during the port hand-off in step 6. Twitch retries webhook deliveries, the
site retries `/streamstatus/status.json` every 15 s, and Plausible's script loads after
idle, so nobody should notice. Caddy has its own certificate **before** the
hand-off, so there is no window without TLS.

Container names below assume the compose project `hosting` (compose v1:
`hosting_nginx_1`). Check them with `docker ps` first.

## 0. Preconditions (owner, AWS console or terraform)

1. **Security group `iss-sg`: allow UDP 443** from 0.0.0.0/0. The snippet is
   `deploy/edge/terraform/iss-sg-quic.tf.snippet`. It's harmless before
   cutover, since nothing listens on UDP 443 yet.
2. **UDP buffers for QUIC.** quic-go wants ~7.5 MB socket buffers, or it
   logs a warning and HTTP/3 throughput suffers. These are host-wide
   (not per-namespace):
   ```bash
   printf 'net.core.rmem_max=7500000\nnet.core.wmem_max=7500000\n' | sudo tee /etc/sysctl.d/99-quic.conf
   sudo sysctl --system
   ```
3. **DNS:** `p` stays a plain A record to the Elastic IP (DigitalOcean
   today; **DNS only / grey cloud** if the zone moves to Cloudflare).
4. **CAA:** if a CAA record is published for infosecstreams.com, it must
   allow `letsencrypt.org` (terraform declares one; it is not live today).

## 1. Back up

```bash
cd ~/hosting
ts=$(date +%Y%m%d-%H%M%S); mkdir -p backup-$ts
cp -a docker-compose.yml nginx/ backup-$ts/
sudo tar czf backup-$ts/letsencrypt.tgz /etc/letsencrypt
docker inspect hosting_nginx_1 > backup-$ts/nginx-inspect.json
```

## 2. Install the files (no running change yet)

```bash
mkdir -p caddy
cp <infra-edge>/deploy/edge/caddy/Caddyfile caddy/Caddyfile
```

Edit `docker-compose.yml`, following the header of
`deploy/edge/docker-compose.caddy.yml`:
- Add the `caddy` service and the `caddy-data` and `caddy-config` volumes.
- In `plausible` and `streamstatus`, change `depends_on` from `nginx` to `caddy`.
- Add `profiles: ["legacy-nginx"]` to `nginx` and `letsencrypt`.
  Compose 1.28+ supports profiles, and the host runs 1.29.

```bash
docker-compose config -q && echo compose-ok
docker-compose pull caddy
docker run --rm -v "$PWD/caddy:/etc/caddy:ro" caddy:2.11.9-alpine caddy validate --config /etc/caddy/Caddyfile
```

## 3. Get Caddy its own certificate while nginx still serves traffic

Run Caddy as a one-off container on the compose network, with **no published
ports**. It shares the `caddy-data` volume, so the certificate it gets is the
one the real service uses.

```bash
docker-compose run -d --no-deps --name caddy_stage caddy
```

Temporarily route ACME HTTP-01 challenges from nginx to it. This edits only the
running nginx container (a recreate restores the image's config), and reloads
nginx without dropping connections:

```bash
docker cp hosting_nginx_1:/etc/nginx/conf.d/nginx.default.conf /tmp/nginx.default.conf
# In the `listen 80; server_name p.infosecstreams.com;` block, replace the
# acme-challenge location with:
#     location ^~ /.well-known/acme-challenge/ {
#         resolver 127.0.0.11 valid=10s;
#         set $caddy_stage http://caddy_stage:80;
#         proxy_set_header Host $host;
#         proxy_pass $caddy_stage;
#     }
docker cp /tmp/nginx.default.conf hosting_nginx_1:/etc/nginx/conf.d/nginx.default.conf
docker exec hosting_nginx_1 nginx -t && docker exec hosting_nginx_1 nginx -s reload
docker logs -f caddy_stage 2>&1 | grep -m1 -E 'certificate obtained successfully|could not get certificate'
```

You want `certificate obtained successfully` for `p.infosecstreams.com`. If
it fails, read the error. Usually it's the proxy location or CAA. Nothing
user-facing has changed yet.

## 4. Test the staged Caddy end to end (inside the network)

```bash
ip=$(docker inspect -f '{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}' caddy_stage)
net=$(docker inspect -f '{{range $k,$v := .NetworkSettings.Networks}}{{$k}}{{end}}' caddy_stage)
t() { docker run --rm --network "$net" curlimages/curl -s --resolve "p.infosecstreams.com:443:$ip" "$@"; }
t -o /dev/null -w '%{http_code} plausible /\n' -A Mozilla/5.0 https://p.infosecstreams.com/
t -o /dev/null -w '%{http_code} script.js %header{content-encoding}\n' -A Mozilla/5.0 -H 'Accept-Encoding: gzip' https://p.infosecstreams.com/js/script.js
t -o /dev/null -w '%{http_code} webhook GET (want 405)\n' https://p.infosecstreams.com/webhook/callbacks
t -o /dev/null -w '%{http_code} plausible /api/health\n' -A Mozilla/5.0 https://p.infosecstreams.com/api/health
t -o /dev/null -w '%{http_code} plausible LiveView longpoll (not 404 from the bot)\n' -A Mozilla/5.0 https://p.infosecstreams.com/live/longpoll
t -o /dev/null -w '%{http_code} backspin\n' --resolve backspin.highvibes.cloud:8080:$ip http://backspin.highvibes.cloud:8080/
# once the bot serves the API:
t -D - -o /dev/null -H 'Accept-Encoding: gzip' https://p.infosecstreams.com/streamstatus/status.json
```

The certificate it presents must be issued for `p.infosecstreams.com`
(`t -v ... 2>&1 | grep subject`).

## 5. Announce nothing, pick a quiet minute

Ideally do this when no roster streamer is going live (Twitch retries anyway).

## 6. Hand-off (the 2–4 s)

```bash
docker rm -f caddy_stage
docker-compose stop -t 3 nginx letsencrypt && docker-compose up -d --no-deps caddy
```

## 7. Verify from outside

From any machine:

```bash
deploy/edge/measure.sh https://p.infosecstreams.com/ https://p.infosecstreams.com/js/script.js https://p.infosecstreams.com/streamstatus/status.json
```

Expect: `server` header gone; `alt-svc: h3=":443"`; HTTP/3 `3 200`;
`content-encoding: gzip` on script.js (1.3 KB instead of 3.0 KB); an ECDSA
certificate (`openssl s_client ... | openssl x509 -noout -text | grep 'Public Key Algorithm'`
→ id-ecPublicKey).

Then:
- In Twitch, check the webhook still verifies: the bot logs no signature
  errors, and the next `stream.online` arrives.
- In Plausible, check the realtime view shows visitors. If every visitor
  shows the same IP, X-Forwarded-For is wrong (it shouldn't be: Caddy sets
  it to the client address by default).
- Check `docker logs hosting_caddy_1` for the quic-go UDP buffer warning. If
  it's there, redo precondition 2.

## 8. Rollback (any time in the first two weeks)

```bash
docker-compose stop -t 3 caddy
docker-compose up -d --no-deps --force-recreate nginx letsencrypt   # recreate = original nginx config
```

`--force-recreate` matters. Without it, the old nginx container comes back
with the step-3 ACME proxy, and certbot renewals then fail. The certbot
certificate stays valid until 2027-01-19, and certbot renews it from about
2026-12-20, so rollback is safe until well past then. Restore
`backup-*/docker-compose.yml` afterwards, so a plain `up -d` doesn't start Caddy.

Rolling back HTTP/3 alone: remove the `443:443/udp` port, or close UDP 443 in
the security group. Browsers fall back to HTTP/2 within a request.

## 9. Clean up (after Caddy has run for 2 weeks)

- Delete the `nginx` and `letsencrypt` services and the `acme-challenge`
  volume from `docker-compose.yml`. Keep `backup-*/letsencrypt.tgz` and
  remove `/etc/letsencrypt` later.
- Commit the same change to `goproslowyo/plausible-docker-compose` (the host's
  `~/hosting` is a clone of it), so the repo matches the host: `caddy/Caddyfile`
  in place of `nginx/`, and the README's container list updated.
- In `StreamStatus`, `deploy/nginx-streamstatus.conf` (ci-deploy-security)
  is superseded by `deploy/edge/caddy/Caddyfile`; delete it, and point
  `deploy/README.md` step "nginx: replace the webhook location" at this runbook.
- Caddy renews about 30 days before expiry (it also follows Let's Encrypt
  ARI). Nothing to schedule. Cert files live in the `caddy-data` volume.
