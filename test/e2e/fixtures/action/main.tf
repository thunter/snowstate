terraform {
  backend "http" {}
}

resource "terraform_data" "example" {
  input = "action"
}
