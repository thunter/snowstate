#!/usr/bin/env bash
# Downloads one release asset and verifies it against checksums.txt. It is a
# bash script because Git Bash is on every hosted runner, including Windows, so
# no Node runtime or PowerShell variant is needed.
set -euo pipefail

base="${BASE_URL:-https://github.com/$REPOSITORY/releases/download/$VERSION}"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

curl -fsSL --retry 3 -o "$tmp/$ASSET" "$base/$ASSET"

if [ "$VERIFY" = "true" ]; then
  curl -fsSL --retry 3 -o "$tmp/checksums.txt" "$base/checksums.txt"
  want="$(grep -E "[ *]$ASSET\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
  if [ -z "$want" ]; then
    echo "::error::$ASSET is not listed in checksums.txt" >&2
    exit 1
  fi
  if command -v sha256sum >/dev/null; then
    got="$(sha256sum "$tmp/$ASSET" | cut -d' ' -f1)"
  else
    got="$(shasum -a 256 "$tmp/$ASSET" | cut -d' ' -f1)"
  fi
  if [ "$want" != "$got" ]; then
    echo "::error::checksum mismatch for $ASSET" >&2
    exit 1
  fi
fi

mkdir -p "$DIR"
mv "$tmp/$ASSET" "$DIR/$BINARY"
chmod +x "$DIR/$BINARY"
