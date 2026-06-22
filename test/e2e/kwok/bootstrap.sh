#!/usr/bin/env bash
# Bootstrap a local kind cluster running the real Karpenter v1 KWOK reference
# cloudprovider plus this controller, for the issue #92 e2e harness.
#
# Everything is pinned for reproducibility (issue #92):
#   - kind node image       → test/e2e/kwok/kind.yaml (digest-pinned)
#   - Karpenter (KWOK)      → built with `ko` from the EXACT vendored upstream
#                             module tag (sigs.k8s.io/karpenter, see go.mod) in a
#                             THROWAWAY module so the controller module never
#                             imports the cloudprovider (v1 invariant). CRDs and
#                             the Helm chart come from that same pinned module.
#   - KWOK controller       → kubernetes-sigs/kwok, pinned by KWOK_RELEASE below.
#
# Idempotent-ish: re-running reuses an existing cluster of the same name.
#
# Required tools on PATH (kind, kubectl, helm, ko, kustomize are pinned in
# aqua.yaml and resolved from $PATH via aqua; plus go and docker): kind,
# kubectl, helm, ko, kustomize, go, docker.
set -euo pipefail

# ── Pinned versions ─────────────────────────────────────────────────────────
# KWOK (node lifecycle stage manager) release. Matches upstream Karpenter's
# hack/install-kwok.sh for the vendored tag.
KWOK_RELEASE="${KWOK_RELEASE:-v0.5.2}"
# The exact Karpenter module tag we vendor; the KWOK provider is built from it.
KARPENTER_VERSION="$(cd "$(git rev-parse --show-toplevel)" && go list -m -f '{{.Version}}' sigs.k8s.io/karpenter)"

CLUSTER_NAME="${CLUSTER_NAME:-nrc-kwok-e2e}"
KARPENTER_NAMESPACE="${KARPENTER_NAMESPACE:-kube-system}"
CONTROLLER_IMAGE="${CONTROLLER_IMAGE:-ghcr.io/akashisn/node-rotation-controller:e2e}"

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO_ROOT="$(git -C "${SCRIPT_DIR}" rev-parse --show-toplevel)"
# The vendored Karpenter module in the module cache: source of the KWOK Helm
# chart, CRDs, and stage manifests — never re-downloaded, always tag-consistent.
KARPENTER_MOD="$(go env GOMODCACHE)/sigs.k8s.io/karpenter@${KARPENTER_VERSION}"

log() { echo "==> $*" >&2; }

# ── 1. kind cluster ─────────────────────────────────────────────────────────
if kind get clusters 2>/dev/null | grep -qx "${CLUSTER_NAME}"; then
  log "kind cluster ${CLUSTER_NAME} already exists; reusing"
else
  log "creating kind cluster ${CLUSTER_NAME}"
  kind create cluster --name "${CLUSTER_NAME}" --config "${SCRIPT_DIR}/kind.yaml" --wait 120s
fi
kind export kubeconfig --name "${CLUSTER_NAME}"

# ── 2. KWOK controller (node lifecycle) ─────────────────────────────────────
# Install the KWOK controller + CRDs + RBAC + service, pinned to KWOK_RELEASE,
# by overlaying the upstream kustomize/kwok bundle. A local overlay pins the
# image tag and tolerates the CriticalAddonsOnly taint Karpenter puts on its
# fake nodes (so the kwok-controller itself never lands on a fake node).
log "installing KWOK controller ${KWOK_RELEASE}"
KWOK_WORK="$(mktemp -d)"
trap 'rm -rf "${KWOK_WORK}"' EXIT

cat >"${KWOK_WORK}/tolerate-all.yaml" <<'EOF'
apiVersion: apps/v1
kind: Deployment
metadata:
  name: kwok-controller
  namespace: kube-system
spec:
  template:
    spec:
      tolerations:
        - operator: "Equal"
          key: CriticalAddonsOnly
          effect: NoSchedule
      affinity:
        nodeAffinity:
          requiredDuringSchedulingIgnoredDuringExecution:
            nodeSelectorTerms:
              - matchExpressions:
                  - key: kwok.x-k8s.io/node
                    operator: DoesNotExist
EOF

cat >"${KWOK_WORK}/kustomization.yaml" <<EOF
apiVersion: kustomize.config.k8s.io/v1beta1
kind: Kustomization
namespace: kube-system
resources:
  - "https://github.com/kubernetes-sigs/kwok/kustomize/kwok?ref=${KWOK_RELEASE}"
images:
  - name: registry.k8s.io/kwok/kwok
    newTag: "${KWOK_RELEASE}"
patches:
  - path: tolerate-all.yaml
EOF

kustomize build "${KWOK_WORK}" | kubectl apply --server-side -f -
kubectl apply -f "https://github.com/kubernetes-sigs/kwok/releases/download/${KWOK_RELEASE}/stage-fast.yaml"
# Pod lifecycle stages (pod -> Running, deletion) shipped with the vendored
# Karpenter module so the surge placeholder and sample workloads run under KWOK.
kubectl apply -f "${KARPENTER_MOD}/hack/kwok/stages"

# ── 3. Karpenter KWOK reference cloudprovider ───────────────────────────────
log "building Karpenter KWOK provider image from ${KARPENTER_VERSION}"
KARP_IMAGE="$("${SCRIPT_DIR}/build-kwok-image.sh")"
log "loading Karpenter KWOK image ${KARP_IMAGE} into kind"
kind load docker-image "${KARP_IMAGE}" --name "${CLUSTER_NAME}"

log "installing Karpenter KWOK CRDs (${KARPENTER_VERSION})"
kubectl apply --server-side -f "${KARPENTER_MOD}/kwok/charts/crds"
kubectl apply --server-side -f "${KARPENTER_MOD}/kwok/apis/crds"

# A custom instance-types file gives deterministic zones/types for the
# assertions (single zone test-zone-a, fixed e2e-small/e2e-large types).
kubectl create namespace "${KARPENTER_NAMESPACE}" --dry-run=client -o yaml | kubectl apply -f -
kubectl create configmap karpenter-instance-types \
  --namespace "${KARPENTER_NAMESPACE}" \
  --from-file=instance-types.json="${SCRIPT_DIR}/manifests/instance-types.json" \
  --dry-run=client -o yaml | kubectl apply -f -

log "installing Karpenter (KWOK) Helm chart"
KARP_REPO="${KARP_IMAGE%:*}"
KARP_TAG="${KARP_IMAGE##*:}"
helm upgrade --install karpenter "${KARPENTER_MOD}/kwok/charts" \
  --namespace "${KARPENTER_NAMESPACE}" --skip-crds \
  --set controller.image.repository="${KARP_REPO}" \
  --set controller.image.tag="${KARP_TAG}" \
  --set controller.image.digest="" \
  --set-string controller.env[0].name=INSTANCE_TYPES_FILE_PATH \
  --set-string controller.env[0].value=/etc/karpenter/instance-types/instance-types.json \
  --set controller.extraVolumeMounts[0].name=instance-types \
  --set controller.extraVolumeMounts[0].mountPath=/etc/karpenter/instance-types \
  --set controller.extraVolumeMounts[0].readOnly=true \
  --set extraVolumes[0].name=instance-types \
  --set extraVolumes[0].configMap.name=karpenter-instance-types \
  --set replicas=1 \
  --set podDisruptionBudget.maxUnavailable=1 \
  --set settings.featureGates.staticCapacity=false \
  --set settings.featureGates.spotToSpotConsolidation=false \
  --set settings.featureGates.nodeRepair=false \
  --wait --timeout 180s

# ── 4. NodePools / KWOKNodeClass ────────────────────────────────────────────
log "applying NodePools and KWOKNodeClass"
kubectl apply -f "${SCRIPT_DIR}/manifests/nodepools.yaml"

# ── 5. This controller (Helm chart from charts/) ────────────────────────────
log "loading controller image ${CONTROLLER_IMAGE} into kind"
kind load docker-image "${CONTROLLER_IMAGE}" --name "${CLUSTER_NAME}"

CONTROLLER_NS="${CONTROLLER_NS:-node-rotation-system}"
CTRL_REPO="${CONTROLLER_IMAGE%:*}"
CTRL_TAG="${CONTROLLER_IMAGE##*:}"
log "installing node-rotation-controller Helm chart"
helm upgrade --install node-rotation "${REPO_ROOT}/charts/node-rotation-controller" \
  --namespace "${CONTROLLER_NS}" --create-namespace \
  --values "${SCRIPT_DIR}/manifests/controller-values.yaml" \
  --set image.repository="${CTRL_REPO}" \
  --set image.tag="${CTRL_TAG}" \
  --set image.pullPolicy=Never \
  --wait --timeout 180s

log "bootstrap complete: cluster=${CLUSTER_NAME} controllerNs=${CONTROLLER_NS}"
