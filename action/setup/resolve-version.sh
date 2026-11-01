#!/usr/bin/env bash
# Turns what a workflow asked for into a release that exists. A full version is
# used as it is. A major tag such as v1 only marks a commit, with no binaries of
# its own, so it resolves to the newest stable release in that major.
# Usage: resolve-version.sh <repository> <version>
set -euo pipefail

repository="${1:?usage: resolve-version.sh <repository> <version>}"
requested="${2:-}"
remote="${REMOTE_URL:-https://github.com/$repository.git}"

if [[ "$requested" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]]; then
  echo "$requested"
  exit 0
fi

if [[ ! "$requested" =~ ^v[0-9]+$ ]]; then
  echo "::error::cannot install snowstate from '${requested:-no version}': pin the action to a release such as @v1 or @v1.2.3, or set the version input" >&2
  exit 1
fi

if ! tags="$(git ls-remote --tags --refs "$remote" "refs/tags/$requested.*")"; then
  echo "::error::could not list the release tags of $repository" >&2
  exit 1
fi

newest="$(sed 's#.*refs/tags/##' <<<"$tags" | grep -E "^$requested\.[0-9]+\.[0-9]+\$" | sort -V | tail -n 1 || true)"
if [[ -z "$newest" ]]; then
  echo "::error::$repository has no stable $requested release" >&2
  exit 1
fi
echo "$newest"
