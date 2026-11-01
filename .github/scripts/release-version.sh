#!/usr/bin/env bash
# Checks a release tag is semver and prints step outputs for it. A tag with a
# suffix, such as v1.2.0-rc.1, is a prerelease and must not move the major tag.
set -euo pipefail

tag="${1:?usage: release-version.sh <tag>}"
if [[ ! "$tag" =~ ^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "::error::$tag is not a semver tag such as v1.2.3 or v1.2.3-rc.1" >&2
  exit 1
fi

prerelease=false
if [[ "$tag" == *-* ]]; then
  prerelease=true
fi
echo "major=v${BASH_REMATCH[1]}"
echo "prerelease=$prerelease"
