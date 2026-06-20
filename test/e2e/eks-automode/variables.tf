# All inputs are variables. Nothing organization-specific (account IDs, regions,
# names) is hard-coded anywhere in this module — supply concrete values via a
# `terraform.tfvars` file (see `terraform.tfvars.example`). This keeps the module
# vendor-neutral and safe to commit to a public repository.

variable "region" {
  description = "AWS region to create the ephemeral cluster in. Required — no default so the region is never baked into the module."
  type        = string
}

variable "name" {
  description = "Base name for the cluster and all derived resources. Keep it short; it prefixes the VPC, EKS cluster, and tags."
  type        = string
  default     = "nrc-eks-automode-e2e"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,30}[a-z0-9]$", var.name))
    error_message = "name must be 3-32 chars, lowercase alphanumeric or hyphen, starting with a letter."
  }
}

variable "kubernetes_version" {
  description = "EKS control-plane Kubernetes version. EKS Auto Mode requires a recent version; pin it explicitly per run."
  type        = string
  default     = "1.33"
}

variable "vpc_cidr" {
  description = "CIDR block for the ephemeral VPC."
  type        = string
  default     = "10.0.0.0/16"

  validation {
    condition     = can(cidrhost(var.vpc_cidr, 0))
    error_message = "vpc_cidr must be a valid IPv4 CIDR block."
  }
}

variable "azs_count" {
  description = "Number of availability zones to span. Same-AZ surge/zonal-EBS PoC scenarios (#93) need at least 2 so a candidate and its surge replacement can be pinned to the same AZ while the cluster still spans multiple zones."
  type        = number
  default     = 2

  validation {
    condition     = var.azs_count >= 2 && var.azs_count <= 3
    error_message = "azs_count must be between 2 and 3."
  }
}

variable "auto_mode_node_pools" {
  description = "Built-in EKS Auto Mode NodePools to enable. EKS Auto Mode ships managed `general-purpose` and `system` NodePools; the controller's surge PoC targets `general-purpose`. Set to [] to manage NodePools yourself via the manifests applied after apply."
  type        = list(string)
  default     = ["general-purpose", "system"]
}

variable "public_access_cidrs" {
  description = "CIDR blocks allowed to reach the public EKS API endpoint. Defaults to fully open for an ephemeral PoC cluster; restrict to your egress IP/32 for anything longer-lived."
  type        = list(string)
  default     = ["0.0.0.0/0"]
}

variable "tags" {
  description = "Extra tags applied to every resource. Use these to mark the stack as ephemeral / owned by your PoC run (e.g. ttl, owner) for cost tracking and cleanup sweeps."
  type        = map(string)
  default     = {}
}
