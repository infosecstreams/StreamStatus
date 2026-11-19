#!/usr/bin/env bash
# Installs a pinned, checksum-verified cosign to /usr/local/bin/cosign.
# Run once on the host (needs sudo). Bump VERSION and both sums together;
# the CI signer (sigstore/cosign-installer in build-and-push.yml) uses the
# same release.
set -euo pipefail
VERSION=v3.1.4
declare -A SHA256=(
  [amd64]=424e2c97d0b45fd7a6b608a87f090f35633f4c23ece5f092d48bba11aaf4bbfa
  [arm64]=ade83b7aac62e131cacfe96d958948b2150cc9efec46a570a2649b8e27dedad2
)
case $(uname -m) in
x86_64) arch=amd64 ;;
aarch64 | arm64) arch=arm64 ;;
*) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL --proto '=https' --tlsv1.2 -o "$tmp/cosign" \
  "https://github.com/sigstore/cosign/releases/download/${VERSION}/cosign-linux-${arch}"
echo "${SHA256[$arch]}  $tmp/cosign" | sha256sum -c -
sudo install -m 0755 -o root -g root "$tmp/cosign" /usr/local/bin/cosign
/usr/local/bin/cosign version | grep -i gitversion
