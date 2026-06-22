# Image URL for docker-build
IMG ?= ghcr.io/akashisn/node-rotation-controller:dev

# Helm chart location.
CHART ?= charts/node-rotation-controller

# Kubernetes version for envtest assets. Keep in sync with the k8s.io/api
# minor in go.mod (v0.<minor>.x -> 1.<minor>).
ENVTEST_K8S_VERSION ?= 1.36

# The Go toolchain and all CLIs (go, setup-envtest, golangci-lint, gopls, kind,
# ko, kustomize, helm, kubectl, terraform) are pinned in aqua.yaml and invoked by
# bare name; aqua lazily installs the pinned version on first use. LOCALBIN is
# only the build/output dir for the manager binary and the envtest assets.
LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

# Prepend aqua's bin dir to PATH so `make` finds the pinned CLIs even when the
# interactive shell PATH does not already include it. No-op when aqua is absent.
AQUA_ROOT := $(shell command -v aqua >/dev/null 2>&1 && aqua root-dir)
ifneq ($(AQUA_ROOT),)
export PATH := $(AQUA_ROOT)/bin:$(PATH)
endif

# aqua-tools makes `make` self-bootstrapping: `aqua install --only-link` creates
# the command symlinks for every tool in aqua.yaml (cheap, offline, idempotent),
# so tool-using targets that depend on it resolve the bare names without the
# developer having to run `aqua i` by hand. The binaries themselves are still
# fetched lazily at their pinned versions on first use.
.PHONY: aqua-tools
aqua-tools:
	@command -v aqua >/dev/null 2>&1 || { \
		echo "aqua not found — install it (https://aquaproj.github.io) so the pinned CLIs in aqua.yaml resolve"; \
		exit 1; \
	}
	@aqua install --only-link

# Cluster + image names shared by the e2e-kwok target and the Go driver.
E2E_KWOK_CLUSTER ?= nrc-kwok-e2e
E2E_KWOK_IMAGE ?= ghcr.io/akashisn/node-rotation-controller:e2e
# Directory of the ephemeral EKS Auto Mode Terraform (test/e2e/eks-automode, #93).
E2E_EKS_DIR ?= test/e2e/eks-automode

.PHONY: all
all: build

.PHONY: fmt
fmt: aqua-tools
	go fmt ./...

.PHONY: vet
vet: aqua-tools
	go vet ./...

.PHONY: build
build: aqua-tools fmt vet
	go build -o $(LOCALBIN)/manager ./cmd

.PHONY: test
test: aqua-tools $(LOCALBIN)
	KUBEBUILDER_ASSETS="$(shell setup-envtest use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

.PHONY: lint
lint: aqua-tools
	golangci-lint run

# gopls-check surfaces gopls' static diagnostics down to hint severity (the
# modernize suggestions etc. that golangci-lint does not cover). gopls check
# takes file paths, not packages, and exits 0 even with findings — so feed it the
# tracked Go files and fail when it prints anything. Findings land on stdout
# (captured) so stderr is dropped to avoid double-printing; a non-zero gopls exit
# (e.g. a load error) fails the target too, so a tool failure can't pass green.
.PHONY: gopls-check
gopls-check: aqua-tools
	@out="$$(gopls check -severity=hint $$(git ls-files '*.go') 2>/dev/null)"; status=$$?; \
	if [ $$status -ne 0 ]; then \
		echo "gopls check failed to run (exit $$status); re-run for detail: gopls check -severity=hint \$$(git ls-files '*.go')"; \
		exit $$status; \
	fi; \
	if [ -n "$$out" ]; then \
		echo "$$out"; \
		echo "gopls check found issues (severity>=hint)"; \
		exit 1; \
	fi

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: helm-lint
helm-lint: aqua-tools
	helm lint $(CHART)
	helm template rot $(CHART) --namespace node-rotation-system >/dev/null
	helm template rot $(CHART) --namespace node-rotation-system --set metrics.serviceMonitor.enabled=true >/dev/null

# e2e-kwok is a STANDALONE target — deliberately NOT a dependency of `test`. It
# builds the controller image, spins up a kind cluster running the real
# Karpenter v1 KWOK reference cloudprovider + this controller, and runs the
# build-tagged Go driver (test/e2e/kwok, issue #92). The `e2e` build tag keeps
# these files out of `make test` / `go test ./...`. kind/ko/kustomize/helm/kubectl
# are resolved from $PATH via aqua (pinned in aqua.yaml). Set E2E_KWOK_KEEP=true
# to leave the cluster up for debugging after the run.
.PHONY: e2e-kwok
e2e-kwok: aqua-tools docker-build-e2e
	CLUSTER_NAME=$(E2E_KWOK_CLUSTER) \
	CONTROLLER_IMAGE=$(E2E_KWOK_IMAGE) \
		test/e2e/kwok/bootstrap.sh
	E2E_KWOK_CLUSTER=$(E2E_KWOK_CLUSTER) \
	KUBECONFIG="$$(kind get kubeconfig-path --name $(E2E_KWOK_CLUSTER) 2>/dev/null || echo $$HOME/.kube/config)" \
		go test -tags e2e -count=1 -v -timeout 50m ./test/e2e/kwok/...
	@if [ "$(E2E_KWOK_KEEP)" != "true" ]; then \
		echo "==> tearing down kind cluster $(E2E_KWOK_CLUSTER)"; \
		kind delete cluster --name $(E2E_KWOK_CLUSTER); \
	else \
		echo "==> E2E_KWOK_KEEP=true; leaving cluster $(E2E_KWOK_CLUSTER) up"; \
	fi

# Build the controller image under the tag the e2e harness loads into kind.
.PHONY: docker-build-e2e
docker-build-e2e:
	docker build -t $(E2E_KWOK_IMAGE) .

# e2e-eks-* manage the ephemeral, real-cloud EKS Auto Mode PoC cluster
# (test/e2e/eks-automode, issue #93). Like e2e-kwok these are STANDALONE — never
# run by `make test`. They REQUIRE AWS credentials and a `terraform.tfvars`
# (copy from terraform.tfvars.example) and they CREATE BILLABLE AWS RESOURCES.
# Ephemeral by design: up -> run scenarios -> down. See the README in that dir.
.PHONY: e2e-eks-up
e2e-eks-up: aqua-tools
	cd $(E2E_EKS_DIR) && terraform init && terraform apply

.PHONY: e2e-eks-kubeconfig
e2e-eks-kubeconfig: aqua-tools
	cd $(E2E_EKS_DIR) && eval "$$(terraform output -raw kubeconfig_command)"
	@echo
	@echo "==> kubeconfig written. Point your shell at the PoC cluster with:"
	@echo "    export KUBECONFIG=$(abspath $(E2E_EKS_DIR))/kubeconfig"

.PHONY: e2e-eks-down
e2e-eks-down: aqua-tools
	cd $(E2E_EKS_DIR) && terraform destroy
