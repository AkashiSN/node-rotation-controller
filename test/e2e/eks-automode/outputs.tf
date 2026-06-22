# Outputs the PoC driver needs to target the cluster (#77, #81).

output "region" {
  description = "Region the cluster was created in."
  value       = var.region
}

output "cluster_name" {
  description = "EKS cluster name."
  value       = module.eks.cluster_name
}

output "cluster_endpoint" {
  description = "EKS control-plane API endpoint."
  value       = module.eks.cluster_endpoint
}

output "cluster_version" {
  description = "EKS control-plane Kubernetes version."
  value       = module.eks.cluster_version
}

output "auto_mode_node_pools" {
  description = "Built-in EKS Auto Mode NodePools enabled on the cluster. The controller's surge PoC targets these via the Karpenter v1 NodeClaim CRD."
  value       = var.auto_mode_node_pools
}

output "availability_zones" {
  description = "AZs the cluster spans. Same-AZ surge / zonal-EBS scenarios (#93) pin a candidate and its replacement to one of these."
  value       = local.azs
}

# Ready-to-run command that writes a kubeconfig the PoC scenarios can use. We
# emit the command rather than render the kubeconfig into Terraform state, so no
# cluster credentials are persisted to state. `make e2e-eks-kubeconfig` runs it.
output "kubeconfig_command" {
  description = "Command that writes a kubeconfig for this cluster. Run it (or `make e2e-eks-kubeconfig`) before pointing kubectl/helm at the PoC cluster."
  value       = "aws eks update-kubeconfig --region ${var.region} --name ${module.eks.cluster_name} --kubeconfig ./kubeconfig"
}
