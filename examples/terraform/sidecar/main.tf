# Sidecar mode: leave the block empty. `snowstate exec` supplies everything
# through TF_HTTP_* variables, which is why saved plans stay portable.
#
#   export SNOWSTATE_KEY=platform/network/prod
#   snowstate exec -- terraform init
#   snowstate exec -- terraform apply
terraform {
  backend "http" {}
}

resource "terraform_data" "example" {
  input = "managed by snowstate"
}
