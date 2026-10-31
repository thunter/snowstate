# Server mode: one address per workspace. The password comes from TF_HTTP_PASSWORD.
terraform {
  backend "http" {
    address        = "https://snowstate.example.com/v1/states/platform/network/prod"
    lock_address   = "https://snowstate.example.com/v1/states/platform/network/prod"
    unlock_address = "https://snowstate.example.com/v1/states/platform/network/prod"
    username       = "ci-platform"
  }
}

resource "terraform_data" "example" {
  input = "managed by snowstate"
}
