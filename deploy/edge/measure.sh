#!/usr/bin/env bash
# Read-only edge measurement for infosecstreams.com and p.infosecstreams.com.
# Run before and after a change and diff the output.
#
#   deploy/edge/measure.sh                 # default URLs, 5 timing runs
#   RUNS=10 deploy/edge/measure.sh https://infosecstreams.com/ https://p.infosecstreams.com/streamstatus/status.json
#
# Needs curl and openssl. HTTP/3 checks use the local curl if it has HTTP3,
# else the ymuski/curl-http3 container when docker is available.
set -euo pipefail

RUNS=${RUNS:-5}
UA='infosecstreams-edge-measure/1'
if [ $# -gt 0 ]; then URLS=("$@"); else
  URLS=(https://infosecstreams.com/ https://p.infosecstreams.com/ https://p.infosecstreams.com/js/script.js https://p.infosecstreams.com/streamstatus/status.json)
fi

h3curl() {
  if curl --version | grep -q HTTP3; then curl "$@"
  elif command -v docker >/dev/null; then docker run --rm ymuski/curl-http3 curl "$@"
  else echo "no HTTP/3 curl" >&2; return 1; fi
}

for url in "${URLS[@]}"; do
  host=${url#https://}; host=${host%%/*}
  echo "================ $url"
  echo "--- TLS"
  for v in tls1_3 tls1_2; do
    out=$(echo | timeout 10 openssl s_client -connect "$host:443" -servername "$host" -"$v" -status 2>/dev/null || true)
    proto=$(sed -n 's/^ *Protocol *: *//p' <<<"$out" | head -1)
    cipher=$(sed -n 's/^ *Cipher *: *//p' <<<"$out" | head -1)
    ocsp=$(grep -q 'OCSP Response Status: successful' <<<"$out" && echo stapled || echo none)
    echo "$v: ${proto:-refused} ${cipher} ocsp=$ocsp"
  done
  echo | openssl s_client -connect "$host:443" -servername "$host" 2>/dev/null \
    | openssl x509 -noout -issuer -enddate -ext subjectAltName 2>/dev/null | tr -s ' ' || true
  # TLS 1.3 early data (0-RTT) offered by the server?
  sess=$(mktemp); trap 'rm -f "$sess"' EXIT
  echo | timeout 10 openssl s_client -connect "$host:443" -servername "$host" -tls1_3 -sess_out "$sess" -ign_eof >/dev/null 2>&1 <<<"" || true
  sleep 1
  ed=$(echo | timeout 10 openssl s_client -connect "$host:443" -servername "$host" -tls1_3 -sess_in "$sess" 2>/dev/null | grep -E 'Reused|Max Early Data' | tr '\n' ' ' || true)
  echo "resume/0-RTT: ${ed:-n/a}"

  echo "--- headers (h2, Accept-Encoding: br, zstd, gzip)"
  curl -sS -o /dev/null -D - --http2 -A "$UA" -H 'Accept-Encoding: br, zstd, gzip' "$url" \
    | grep -i -E '^(HTTP/|server|content-type|content-encoding|content-length|cache-control|age|etag|last-modified|expires|vary|alt-svc|strict-transport|content-security|x-cache|x-served-by|via|cf-|link|access-control-allow-origin|x-content-type|referrer-policy|permissions-policy|cross-origin)' || true

  echo "--- compression offered (bytes on the wire)"
  for enc in identity gzip br zstd; do
    curl -sS -o /dev/null --http2 -A "$UA" -H "Accept-Encoding: $enc" \
      -w "$enc: %{size_download} B, content-encoding=%header{content-encoding}\n" "$url"
  done

  echo "--- 103 Early Hints"
  curl -sS -o /dev/null -D - --http2 -A "$UA" "$url" | grep -E '^HTTP/[0-9.]+ 103' || echo "none"

  echo "--- HTTP/3"
  h3curl -sS -o /dev/null --http3-only -m 10 -w 'h3: %{http_version} %{http_code} ttfb=%{time_starttransfer}s\n' "$url" 2>&1 | tail -1 || true

  echo "--- timing over $RUNS cold connections (h2): dns connect tls ttfb total, seconds"
  for _ in $(seq "$RUNS"); do
    curl -sS -o /dev/null --http2 -A "$UA" -H 'Accept-Encoding: br, gzip' \
      -w '%{time_namelookup} %{time_connect} %{time_appconnect} %{time_starttransfer} %{time_total} %{http_version}\n' "$url"
  done | awk '{d+=$1;c+=$2;t+=$3;f+=$4;s+=$5;n++; if(min==""||$4<min)min=$4; if($4>max)max=$4}
     END{printf "avg dns=%.3f connect=%.3f tls=%.3f ttfb=%.3f (min %.3f max %.3f) total=%.3f\n",d/n,c/n,t/n,f/n,min,max,s/n}'
  echo "--- revalidation (If-None-Match / If-Modified-Since)"
  et=$(curl -sS -o /dev/null -D - "$url" -A "$UA" -H 'Accept-Encoding: gzip' | sed -n 's/^etag: *//Ip' | tr -d '\r')
  if [ -n "$et" ]; then
    curl -sS -o /dev/null -A "$UA" -H 'Accept-Encoding: gzip' -H "If-None-Match: $et" -w "If-None-Match -> %{http_code}\n" "$url"
  else echo "no ETag"; fi
done
