#!/usr/bin/env bash
# Fails unless the changelog has a section for a stable release tag, so a release
# cannot be published with its changes still under Unreleased.
# Usage: check-changelog.sh <tag> [changelog file]
set -euo pipefail

tag="${1:?usage: check-changelog.sh <tag> [changelog file]}"
changelog="${2:-CHANGELOG.md}"
version="${tag#v}"

if ! grep -Eq "^## ${version//./\\.}( |\$)" "$changelog"; then
  echo "::error::$changelog has no '## $version' section. Add one in a pull request, then tag again." >&2
  exit 1
fi
