#!/usr/bin/env bash
# Build the Karpenter KWOK reference cloudprovider image from the EXACT upstream
# tag this repo vendors (sigs.k8s.io/karpenter in go.mod), without adding the
# cloudprovider to the controller module — preserving the v1 "no cloud-provider
# API dependency" invariant (CLAUDE.md, issue #92).
#
# It does so in a THROWAWAY Go module under a temp dir whose only dependency is
# the pinned Karpenter tag, then `ko build`s sigs.k8s.io/karpenter/kwok into the
# local docker daemon. The printed line is the resulting image ref (repo:tag),
# the ONLY thing on stdout — all logging goes to stderr — so callers can
# `IMG=$(build-kwok-image.sh)`.
set -euo pipefail

log() { echo "==> $*" >&2; }

REPO_ROOT="$(git rev-parse --show-toplevel)"

# `ko` is pinned in aqua.yaml and resolved from $PATH via aqua. aqua finds its
# config by walking up from $CWD, but the `ko build` below runs in a throwaway
# temp module OUTSIDE the repo tree — so point aqua at the repo's aqua.yaml
# explicitly. No-op when aqua isn't on the toolchain (the var is just unused).
export AQUA_GLOBAL_CONFIG="${AQUA_GLOBAL_CONFIG:-${REPO_ROOT}/aqua.yaml}"

KARPENTER_VERSION="$(cd "${REPO_ROOT}" && go list -m -f '{{.Version}}' sigs.k8s.io/karpenter)"

# Deterministic, content-addressed-ish tag keyed on the pinned version so a
# rebuild of the same tag is a cache hit and `kind load` is idempotent.
IMAGE_REPO="${KWOK_IMAGE_REPO:-ko.local/karpenter-kwok}"
IMAGE_TAG="${KARPENTER_VERSION//[^a-zA-Z0-9_.-]/-}"
IMAGE="${IMAGE_REPO}:${IMAGE_TAG}"

# Reuse an already-built image (e.g. across local iterations).
if [[ "${KWOK_IMAGE_FORCE_REBUILD:-false}" != "true" ]] && docker image inspect "${IMAGE}" >/dev/null 2>&1; then
  log "reusing existing KWOK provider image ${IMAGE}"
  echo "${IMAGE}"
  exit 0
fi

BUILD_DIR="$(mktemp -d)"
trap 'rm -rf "${BUILD_DIR}"' EXIT

cat >"${BUILD_DIR}/go.mod" <<EOF
module nrc-kwok-provider-build

go 1.26.4

require sigs.k8s.io/karpenter ${KARPENTER_VERSION}
EOF

# A tools.go pins the kwok package as a build dependency so `go mod tidy`
# resolves the full transitive set for ko's build.
cat >"${BUILD_DIR}/tools.go" <<'EOF'
//go:build tools

// Package tools pins the Karpenter KWOK reference cloudprovider main package so
// `go mod tidy` resolves its dependency graph for `ko build`. It is never
// compiled into anything (the build tag excludes it from normal builds).
package tools

import _ "sigs.k8s.io/karpenter/kwok"
EOF

log "resolving KWOK provider deps (Karpenter ${KARPENTER_VERSION}) in ${BUILD_DIR}"
(cd "${BUILD_DIR}" && GOFLAGS=-mod=mod go mod tidy >&2)

log "ko build sigs.k8s.io/karpenter/kwok -> ${IMAGE}"
(
  cd "${BUILD_DIR}"
  KO_DOCKER_REPO="${IMAGE_REPO}" GOFLAGS=-mod=mod \
    ko build --local --bare --tags "${IMAGE_TAG}" \
    --platform "linux/$(go env GOARCH)" \
    sigs.k8s.io/karpenter/kwok >&2
)

echo "${IMAGE}"
