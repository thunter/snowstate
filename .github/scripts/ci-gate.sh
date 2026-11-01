#!/usr/bin/env bash
# The single required check for a workflow. A skipped matrix job does not report
# the check name a ruleset expects, so rulesets require this job instead.
# Usage: ci-gate.sh <result of the changes job> <result of each needed job>...
set -euo pipefail

changes="${1:-}"
if [[ "$changes" != "success" ]]; then
  echo "::error::the changes job did not succeed: ${changes:-no result}" >&2
  exit 1
fi
shift

for result in "$@"; do
  case "$result" in
    success | skipped) ;;
    *)
      echo "::error::a required job finished with: $result" >&2
      exit 1
      ;;
  esac
done
