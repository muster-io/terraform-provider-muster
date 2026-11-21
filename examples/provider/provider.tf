terraform {
  required_providers {
    muster = {
      source = "muster-io/muster"
    }
  }
}

# The endpoint and the token come from MUSTER_ENDPOINT and MUSTER_TOKEN when the block leaves them out.
provider "muster" {
  endpoint = "https://muster.example.org"
  token    = var.muster_token
}

variable "muster_token" {
  type      = string
  sensitive = true
}
