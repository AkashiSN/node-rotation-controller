# Ephemeral EKS Auto Mode cluster for the node-rotation-controller real-cloud
# PoC (issue #93). This stands up the minimum real-cloud surface needed to run
# the §7.2 PoC items the KWOK harness (test/e2e/kwok, #92) cannot cover:
# same-AZ surge + zonal-EBS rebind, real capacity-shortage rollback, NodePool
# `limits` exhaustion, and the `expireAfter` real-soak race.
#
# Ephemeral by design: apply -> run scenarios -> destroy. See README.md.

provider "aws" {
  region = var.region

  default_tags {
    tags = local.tags
  }
}

data "aws_availability_zones" "available" {
  state = "available"

  filter {
    name   = "opt-in-status"
    values = ["opt-in-not-required"]
  }
}

locals {
  azs = slice(data.aws_availability_zones.available.names, 0, var.azs_count)

  tags = merge(
    {
      "Project"   = "node-rotation-controller"
      "Component" = "e2e-eks-automode"
      "Ephemeral" = "true"
      "ManagedBy" = "terraform"
    },
    var.tags,
  )
}

# ---------------------------------------------------------------------------
# VPC — minimal, with the subnet tags EKS / Karpenter discovery relies on.
# ---------------------------------------------------------------------------
module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "6.6.1"

  name = var.name
  cidr = var.vpc_cidr

  azs             = local.azs
  private_subnets = [for i, _ in local.azs : cidrsubnet(var.vpc_cidr, 4, i)]
  public_subnets  = [for i, _ in local.azs : cidrsubnet(var.vpc_cidr, 4, i + 8)]

  enable_nat_gateway = true
  single_nat_gateway = true # one NAT for the whole stack — cheapest for an ephemeral PoC.

  # EKS Auto Mode discovers subnets via the cluster's own selectors, but the
  # shared/ELB role tags keep load-balancer and node placement working as
  # expected and match standard EKS conventions.
  public_subnet_tags = {
    "kubernetes.io/role/elb" = "1"
  }
  private_subnet_tags = {
    "kubernetes.io/role/internal-elb" = "1"
  }
}

# ---------------------------------------------------------------------------
# EKS cluster with Auto Mode enabled.
#
# `compute_config.enabled = true` turns on EKS Auto Mode: AWS manages
# the Karpenter control loop and ships built-in NodePools / NodeClasses. We do
# NOT create our own Karpenter install — the architectural invariant is that the
# controller routes all node operations through the Karpenter NodeClaim CRD,
# which Auto Mode provides natively.
# ---------------------------------------------------------------------------
module "eks" {
  source  = "terraform-aws-modules/eks/aws"
  version = "21.23.0"

  name               = var.name
  kubernetes_version = var.kubernetes_version

  endpoint_public_access       = true
  endpoint_public_access_cidrs = var.public_access_cidrs

  vpc_id     = module.vpc.vpc_id
  subnet_ids = module.vpc.private_subnets

  # EKS Auto Mode. No self-managed/managed node groups are defined — Auto Mode
  # provisions and manages all compute, exposing it through Karpenter v1 CRDs.
  compute_config = {
    enabled    = true
    node_pools = var.auto_mode_node_pools
  }

  # The principal running `terraform apply` gets cluster-admin so the PoC driver
  # (kubectl / helm) can install the controller and apply scenario manifests
  # immediately after apply.
  enable_cluster_creator_admin_permissions = true
}
