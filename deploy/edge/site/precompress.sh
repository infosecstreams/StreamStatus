#!/usr/bin/env bash
# Write .br (brotli -q 11) and .zst (zstd -19) next to every compressible file
# in an Astro build, for servers that serve precompressed siblings (Caddy
# `file_server precompressed`, nginx `brotli_static`/`gzip_static`).
# Skips files under 512 B and keeps a sibling only if it is smaller.
#   deploy/edge/site/precompress.sh site/dist
set -euo pipefail
dir=${1:?usage: precompress.sh <dist dir>}
if ! command -v brotli >/dev/null || ! command -v zstd >/dev/null; then echo "need brotli and zstd" >&2; exit 1; fi
find "$dir" -type f \( -name '*.html' -o -name '*.css' -o -name '*.js' -o -name '*.mjs' -o -name '*.svg' \
  -o -name '*.json' -o -name '*.xml' -o -name '*.txt' -o -name '*.ics' -o -name '*.webmanifest' \) -size +511c -print0 |
while IFS= read -r -d '' f; do
  size=$(stat -c %s "$f")
  brotli -q 11 -f -o "$f.br" "$f"
  zstd -19 -q -f -o "$f.zst" "$f"
  for c in "$f.br" "$f.zst"; do
    [ "$(stat -c %s "$c")" -lt "$size" ] || rm -f "$c"
  done
done
echo "precompressed: $(find "$dir" -name '*.br' | wc -l) br, $(find "$dir" -name '*.zst' | wc -l) zst"
