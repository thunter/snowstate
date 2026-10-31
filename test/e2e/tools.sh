#!/usr/bin/env bash
# Downloads the pinned Terraform and OpenTofu binaries the e2e suite drives
# into .tools/, verifying each archive against its published SHA256SUMS.
set -euo pipefail

TF_OLD=1.5.7
TF_NEW=1.17.1
TOFU=1.13.2

root="$(cd "$(dirname "$0")/../.." && pwd)"
tools="$root/.tools"
os="$(uname -s | tr '[:upper:]' '[:lower:]')"
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "unsupported architecture $(uname -m)" >&2; exit 1 ;;
esac

sha256() {
  if command -v sha256sum >/dev/null; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

fetch() { # name version zip-url sums-url binary
  local name=$1 version=$2 url=$3 sums=$4 bin=$5
  local dest="$tools/$name-$version"
  [ -x "$dest/$bin" ] && return
  local tmp
  tmp="$(mktemp -d)"
  local zip="$tmp/$(basename "$url")"
  echo "downloading $name $version"
  curl -fsSL -o "$zip" "$url"
  curl -fsSL -o "$tmp/SHA256SUMS" "$sums"
  local want
  want="$(grep " $(basename "$url")\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)"
  [ -n "$want" ] && [ "$want" = "$(sha256 "$zip")" ] || { echo "checksum mismatch for $url" >&2; exit 1; }
  mkdir -p "$dest"
  unzip -q -o "$zip" "$bin" -d "$dest"
  rm -rf "$tmp"
}

hc=https://releases.hashicorp.com/terraform
for v in "$TF_OLD" "$TF_NEW"; do
  fetch terraform "$v" "$hc/$v/terraform_${v}_${os}_${arch}.zip" "$hc/$v/terraform_${v}_SHA256SUMS" terraform
done
gh=https://github.com/opentofu/opentofu/releases/download
fetch tofu "$TOFU" "$gh/v$TOFU/tofu_${TOFU}_${os}_${arch}.zip" "$gh/v$TOFU/tofu_${TOFU}_SHA256SUMS" tofu
