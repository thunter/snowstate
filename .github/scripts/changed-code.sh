#!/usr/bin/env bash
# Prints code=false when every file changed between two commits is documentation,
# and code=true otherwise. When in doubt it says true, so CI runs in full.
set -uo pipefail

base="${1:-}"
head="${2:-HEAD}"

if [[ -z "$base" || "$base" =~ ^0+$ ]]; then
  echo "code=true"
  exit 0
fi
if ! files="$(git diff --name-only "$base" "$head" 2>/dev/null)" || [[ -z "$files" ]]; then
  echo "code=true"
  exit 0
fi

while IFS= read -r file; do
  case "$file" in
    docs/* | *.md | LICENSE | NOTICE) ;;
    *)
      echo "code=true"
      exit 0
      ;;
  esac
done <<<"$files"
echo "code=false"
