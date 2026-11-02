#!/usr/bin/env bash
# Checks the Foundation with each pinned Terraform and OpenTofu. It needs no
# credentials. It works on a copy, so nothing is left in the source, and enables
# the backend block so that file is checked too.
set -euo pipefail

root="$(cd "$(dirname "$0")/../.." && pwd)"
src="$root/deploy/terraform/foundation"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

cp "$src"/*.tf "$work"/
cp "$src"/backend.tf.disabled "$work"/backend.tf

for bin in "$root"/.tools/terraform-*/terraform "$root"/.tools/tofu-*/tofu; do
  echo "checking with $bin"
  "$bin" -chdir="$work" fmt -check -diff
  "$bin" -chdir="$work" init -backend=false -input=false -no-color >/dev/null
  "$bin" -chdir="$work" validate -no-color
  rm -rf "$work"/.terraform "$work"/.terraform.lock.hcl
done
