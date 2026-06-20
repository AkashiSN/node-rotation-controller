# Provider and Terraform version constraints.
#
# Versions are pinned with pessimistic constraints (`~>`) for reproducibility,
# matching the repo's convention of pinning e2e tooling. Bump deliberately, in
# its own change, when a newer EKS Auto Mode contract needs to be exercised.
terraform {
  required_version = ">= 1.6.0, < 2.0.0"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.70"
    }
  }
}
