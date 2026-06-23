# Ephemeral EKS Auto Mode PoC infrastructure (issue #93)

Terraform that stands up a **real, ephemeral EKS Auto Mode cluster** so the
node-rotation-controller PoC scenarios can be validated against real cloud
capacity and storage — the half of the spec
[§7.2](../../../docs/specification.md#72-validated-assumptions) PoC that the
local [KWOK harness](../kwok/README.md) (#92) deliberately cannot cover.

It is the **real-cloud companion** to the KWOK harness:

| Harness | Cost | Covers |
|---------|------|--------|
| [`test/e2e/kwok/`](../kwok/) (#92) | free, CI-reproducible | Karpenter v1 contract on virtual nodes |
| `test/e2e/eks-automode/` (this) (#93) | **real AWS spend** | same-AZ surge + zonal-EBS rebind, `readyTimeout` rollback, NodePool `limits` gating, multi-NodePool confinement, PDB-respecting drain, `do-not-disrupt`, `expired` outcome, `expireAfter` backstop margin (genuine same-AZ capacity-shortage/ICE and a full tight-race soak remain open — #109) |

> **This is test-only infrastructure.** It lives entirely under `test/e2e/` and
> does not touch the controller (`internal/`, `cmd/`). EKS Auto Mode provides
> Karpenter v1 natively, so the controller still routes every node operation
> through the Karpenter `NodeClaim` CRD — the project's core architectural
> invariant (see [`docs/specification.md`](../../../docs/specification.md)) is preserved.

## Cost warning — ephemeral by design

**This provisions billable AWS resources: an EKS control plane, a NAT gateway, an
EBS volume per surged node, and EC2 instances launched on demand by Auto Mode.**

Treat the stack as disposable: **create it, run the scenarios, then destroy it.**
Do not leave it running. `terraform destroy` removes everything this module
created; EC2/EBS launched by Auto Mode are owned by the cluster and torn down
with it. The default tags mark the stack `Ephemeral = "true"` so it is easy to
find and sweep. Restrict `public_access_cidrs` to your egress IP for anything
that outlives a single run.

## Prerequisites

With **AWS credentials configured** (`aws sts get-caller-identity` must succeed)
for a principal allowed to create VPC / EKS / IAM / ECR resources.

`awscli`, `terraform`, `kubectl`, `helm`, and `ko` are version-pinned in
[`aqua.yaml`](../../../aqua.yaml) — install [aqua](https://aquaproj.github.io)
and they resolve from `$PATH` automatically (the repo-root `make` targets, e.g.
`make e2e-eks-up`, link them for you; aqua lazily installs each on first use).

One tool is **not** managed by aqua and must be on `PATH` yourself:

- [Docker](https://docs.docker.com/get-docker/) with
  [`buildx`](https://docs.docker.com/build/) — to build and push a **multi-arch**
  controller image (step 3). Multi-arch matters: Auto Mode may launch amd64
  *or* arm64 (Graviton) EC2, and a single-arch image fails to run on the other.
  (The aqua-pinned `ko` can build/push instead of buildx — see step 3.)

## Lifecycle: apply -> validate -> destroy

### 1. Configure

```bash
cd test/e2e/eks-automode
cp terraform.tfvars.example terraform.tfvars
$EDITOR terraform.tfvars   # set region, name, etc. (terraform.tfvars is gitignored)
```

Nothing is hard-coded: region, cluster name, Kubernetes version, VPC CIDR, AZ
count, and the enabled Auto Mode NodePools are all variables (see
[`variables.tf`](variables.tf)). The committed
[`terraform.tfvars.example`](terraform.tfvars.example) carries only placeholders.

### 2. Apply

```bash
terraform init
terraform apply        # ~15 min for the control plane to come up
```

Then write a kubeconfig and point your tools at the cluster. `make
e2e-eks-kubeconfig` prints the exact `export KUBECONFIG=…` line to paste:

```bash
eval "$(terraform output -raw kubeconfig_command)"   # or: make e2e-eks-kubeconfig
export KUBECONFIG=$PWD/kubeconfig
kubectl get nodepools.karpenter.sh        # built-in Auto Mode NodePools
```

`terraform output` also exposes `cluster_name`, `cluster_endpoint`,
`availability_zones`, `auto_mode_node_pools`, and `ecr_repository_url` for the
scenario drivers.

### 3. Build and push the controller image to ECR

The chart defaults to an unpublished `ghcr.io` image, so the image must be
delivered to the cluster first. `kind load` (used by the KWOK harness) has no
equivalent here — real EC2 nodes pull from a registry. This stack manages a
private, same-account ECR repo for exactly this; Auto Mode's node role pulls
same-account images, so **no `imagePullSecret` is needed**.

```bash
REPO=$(terraform output -raw ecr_repository_url)
REGION=$(terraform output -raw region)

# Log Docker in to ECR
aws ecr get-login-password --region "$REGION" \
  | docker login --username AWS --password-stdin "${REPO%/*}"

# Build + push multi-arch, reusing the repo Dockerfile (run from the repo root):
docker buildx build --platform linux/amd64,linux/arm64 \
  -t "$REPO:poc" --push ../../..

# …or with ko (no buildx builder setup needed), from the repo root:
#   KO_DOCKER_REPO=$REPO ko build ./cmd --bare --platform=linux/amd64,linux/arm64 -t poc
```

### 4. Validate (run the PoC scenarios)

Install the controller from this repo's chart — pointed at the ECR image you
just pushed — and run the scenarios (#77, #81), then record the outcomes back
into spec §7.2:

```bash
# Install with the scenario values overlay (selector, always-open window, single
# replica, off-nrc-poc affinity) — WITHOUT -f scenarios/controller-values.yaml the
# chart defaults (workload=api selector, Tokyo Wed/Sat window, 2 replicas/PDB) make
# the nrc-poc scenarios neither start nor match the validated setup.
helm install node-rotation-controller ../../../charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  -f scenarios/controller-values.yaml \
  --set image.repository="$REPO" --set image.tag=poc --wait --timeout 8m
# then apply the NodePool + scenario workloads and drive rotations — see SCENARIOS.md
```

> **Step-by-step runbook:** [`SCENARIOS.md`](SCENARIOS.md) is the reproducible
> walkthrough — the exact NodePool, workloads, and controller config (in
> [`scenarios/`](scenarios/)), the trigger/observe commands, the **expected
> output** for each scenario, and the non-obvious gotchas. Follow it to re-run the
> validation below and reproduce the spec §7.2 results.

The PoC subset this cluster validates (issue #93 acceptance criteria), as run in
[`SCENARIOS.md`](SCENARIOS.md) and recorded in spec §7.2 — **validated** unless the
row says otherwise:

| § / Issue | Scenario | Status |
|-----------|----------|--------|
| §7.2, §3.3 | Same-AZ surge node lets the CSI driver re-attach a zonal EBS volume (zonal-PV rebind) | validated |
| §3.2, Refs #81 | Surge `readyTimeout` rollback; NodePool `limits` exhaustion gates a rotation | validated — but a **genuine** same-AZ capacity shortage (ICE) was stood in by a short `readyTimeout`; real ICE still open (#109) |
| Refs #81 | Rollback cleanup: placeholder deleted, candidate unfrozen/uncordoned, `noderotation_completed_total{outcome="failure"\|"expired"}` increments | validated |
| R6 | `expireAfter` stays a backstop the controller's lead time wins against | validated via a **scaled** multi-rotation soak; a full multi-hour tight-race soak remains deferred (#109) |

Results are recorded in spec
[§7.2 Validated Assumptions](../../../docs/specification.md#72-validated-assumptions);
remaining real-cloud items are tracked in #109. File any divergence as a follow-up
`fix(...)` issue before any production-readiness claim (see the roadmap, spec §6.2).

### 5. Destroy

```bash
terraform destroy        # or: make e2e-eks-down
```

Confirm nothing lingers (Auto Mode EC2/EBS, load balancers) before walking away.

## From the repo root (Make targets)

```bash
make e2e-eks-up          # terraform init + apply
make e2e-eks-kubeconfig  # write ./kubeconfig + print the export KUBECONFIG line
make e2e-eks-down        # terraform destroy
```

These wrap the same Terraform; `terraform.tfvars` must exist first. Like
`make e2e-kwok`, the targets are standalone and never run by `make test`.

## Layout

- `versions.tf` — pinned Terraform / provider versions.
- `variables.tf` — every input (no hard-coded account/region/name values).
- `main.tf` — VPC + EKS cluster with `compute_config.enabled = true` (Auto Mode)
  + the private ECR repo the PoC controller image is pushed to.
- `outputs.tf` — cluster coordinates + `ecr_repository_url` + `kubeconfig_command`
  for the scenario drivers.
- `terraform.tfvars.example` — placeholder values to copy to `terraform.tfvars`.
- `.gitignore` — excludes state, `.terraform/`, real `*.tfvars`, and the kubeconfig.
