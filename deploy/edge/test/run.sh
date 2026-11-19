#!/usr/bin/env bash
# Local functional test of deploy/edge/caddy/Caddyfile against fake upstreams.
# Needs docker. Uses Caddy's internal CA instead of Let's Encrypt.
#   deploy/edge/test/run.sh
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
img=${CADDY_IMAGE:-caddy:2.11.9-alpine}
net=edge-test-$$
work=$(mktemp -d)
cleanup() { docker rm -f "$net-edge" "$net-up" >/dev/null 2>&1 || true; docker network rm "$net" >/dev/null 2>&1 || true; rm -rf "$work"; }
trap cleanup EXIT

# Test copy of the Caddyfile: internal CA instead of ACME.
sed -e '/^\tcert_issuer acme {/,/^\t}/d' -e 's/^\temail .*/\tlocal_certs/' "$here/../caddy/Caddyfile" > "$work/Caddyfile"

mkdir -p "$work/bot/streamstatus/stats" "$work/backspin"
echo "backspin home" > "$work/backspin/index.html"
python3 -I -c '
import json,sys
s=[{"id":str(i),"login":"user%d"%i,"display_name":"user%d"%i,"status":"offline","hours_30d":i%50} for i in range(400)]
open(sys.argv[1],"w").write(json.dumps({"generated_at":"2026-11-19T21:00:00Z","live_count":0,"streamers":s}))' "$work/bot/streamstatus/status.json"
gzip -k -9 "$work/bot/streamstatus/status.json"
echo '{"hours":{"30d":1}}' > "$work/bot/streamstatus/stats/alpha.json"
printf 'BEGIN:VCALENDAR\r\nEND:VCALENDAR\r\n' > "$work/bot/streamstatus/schedule.ics"

docker network create "$net" >/dev/null
docker run -d --name "$net-up" --network "$net" --network-alias streamstatus --network-alias plausible \
  -v "$here/upstreams.Caddyfile:/etc/caddy/Caddyfile:ro" -v "$work/bot:/srv/bot:ro" "$img" >/dev/null
docker run -d --name "$net-edge" --network "$net" -v "$work/Caddyfile:/etc/caddy/Caddyfile:ro" -v "$work/backspin:/srv/backspin:ro" "$img" >/dev/null
sleep 3

# curl inside the network, against the edge container's IP with the real hostname.
ip=$(docker inspect -f "{{(index .NetworkSettings.Networks \"$net\").IPAddress}}" "$net-edge")
c() { docker run --rm --network "$net" ymuski/curl-http3 curl -sk --resolve "p.infosecstreams.com:443:$ip" "$@"; }

fail=0
check() { # name, expected, actual
  if [[ "$3" == *"$2"* ]]; then echo "ok   $1"; else echo "FAIL $1: want '$2' got '$3'"; fail=1; fi
}
U=https://p.infosecstreams.com
long=$(printf 'x%.0s' {1..600})   # a path long enough that the echoed body is >512 B and gets encoded

check "plausible root" "plausible GET /" "$(c $U/)"
check "plausible /api/event goes to plausible" "plausible POST /api/event" "$(c -X POST -A Mozilla/5.0 $U/api/event)"
check "plausible dashboard api" "plausible GET /api/stats/infosecstreams.com/top-stats" "$(c -A Mozilla/5.0 $U/api/stats/infosecstreams.com/top-stats)"
check "plausible /api/status.json is NOT the bot" "plausible GET /api/status.json" "$(c -A Mozilla/5.0 $U/api/status.json)"
check "liveview websocket goes to plausible" "plausible GET /live/websocket" "$(c -A Mozilla/5.0 $U/live/websocket)"
check "liveview longpoll goes to plausible" "plausible POST /live/longpoll" "$(c -X POST -A Mozilla/5.0 $U/live/longpoll)"
check "plausible schema.json stays with plausible" "plausible GET /api/docs/query/schema.json" "$(c -A Mozilla/5.0 $U/api/docs/query/schema.json)"
check "webhook POST" "bot webhook POST" "$(c -X POST $U/webhook/callbacks)"
check "webhook GET refused" "405" "$(c -o /dev/null -w '%{http_code}' $U/webhook/callbacks)"
check "api POST refused" "405" "$(c -o /dev/null -w '%{http_code}' -X POST $U/streamstatus/status.json)"
check "stats by login" '"30d":1' "$(c $U/streamstatus/stats/alpha.json)"
check "ics" "BEGIN:VCALENDAR" "$(c $U/streamstatus/schedule.ics)"

h=$(c -D - -o /dev/null -H 'Accept-Encoding: zstd, br, gzip' $U/streamstatus/status.json)
check "api gzip passthrough" "content-encoding: gzip" "$h"
check "api swr added" "stale-while-revalidate=15, stale-if-error=300" "$h"
check "api cors kept" "access-control-allow-origin: *" "$h"
check "api corp" "cross-origin-resource-policy: cross-origin" "$h"
check "hsts" "strict-transport-security: max-age=63072000" "$h"
check "alt-svc h3" 'alt-svc: h3=":443"' "$h"
et=$(sed -n 's/^etag: *//Ip' <<<"$h" | tr -d '\r')
check "api 304 on gzip etag" "304" "$(c -o /dev/null -w '%{http_code}' -H 'Accept-Encoding: zstd, br, gzip' -H "If-None-Match: $et" $U/streamstatus/status.json)"

hz=$(c -D - -o /dev/null -H 'Accept-Encoding: zstd' $U/streamstatus/status.json)
check "api zstd when gzip not accepted" "content-encoding: zstd" "$hz"
etz=$(sed -n 's/^etag: *//Ip' <<<"$hz" | tr -d '\r')
check "zstd etag suffixed" '-zstd"' "$etz"
check "api 304 on zstd etag" "304" "$(c -o /dev/null -w '%{http_code}' -H 'Accept-Encoding: zstd' -H "If-None-Match: $etz" $U/streamstatus/status.json)"
hp=$(c -D - -o /dev/null -A Mozilla/5.0 -H 'Accept-Encoding: zstd' "$U/js/$long")
check "plausible zstd for zstd-only clients" "content-encoding: zstd" "$hp"

check "plausible gets gzip" "content-encoding: gzip" "$(c -D - -o /dev/null -A Mozilla/5.0 -H 'Accept-Encoding: zstd, gzip' "$U/js/$long")"
check "blocked UA aborted" "000" "$(c -o /dev/null -w '%{http_code}' -A 'Mozilla/5.0 zgrab/0.x' $U/ || true)"
check "blocked path aborted" "000" "$(c -o /dev/null -w '%{http_code}' -A Mozilla/5.0 $U/wp-login.php || true)"
check "null UA aborted" "000" "$(c -o /dev/null -w '%{http_code}' -A '' $U/ || true)"
check "security.txt" "Contact: mailto:security.txt@infosecstreams.com" "$(c $U/.well-known/security.txt)"
check "wrong SNI rejected" "000" "$(docker run --rm --network "$net" ymuski/curl-http3 curl -sk -o /dev/null -w '%{http_code}' --resolve "x.example:443:$ip" https://x.example/ || true)"
check "http redirects to https" "308" "$(docker run --rm --network "$net" ymuski/curl-http3 curl -s -o /dev/null -w '%{http_code}' --resolve "p.infosecstreams.com:80:$ip" http://p.infosecstreams.com/)"
check "backspin on :8080" "backspin home" "$(docker run --rm --network "$net" ymuski/curl-http3 curl -s --resolve "backspin.highvibes.cloud:8080:$ip" http://backspin.highvibes.cloud:8080/some/route)"
check "http/3" "3 200" "$(c --http3-only -o /dev/null -w '%{http_version} %{http_code}' -A Mozilla/5.0 $U/)"

docker stats --no-stream --format 'edge caddy memory: {{.MemUsage}}' "$net-edge"
exit $fail
