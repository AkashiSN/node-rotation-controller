# Provider and Terraform version constraints.
#
# Versions are pinned to exact releases for reproducibility, matching the repo's
# convention of pinning e2e tooling. The `.terraform-version` file (tenv) selects
# the matching Terraform. Bump deliberately, in its own change, when a newer EKS
# Auto Mode contract needs to be exercised.
terraform {
  required_version = "1.15.6"

  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "6.51.0"
    }
  }
}
