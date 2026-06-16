# Image URL for docker-build
IMG ?= ghcr.io/akashisn/node-rotation-controller:dev

# Helm chart location.
CHART ?= charts/node-rotation-controller

# Kubernetes version for envtest assets. Keep in sync with the k8s.io/api
# minor in go.mod (v0.<minor>.x -> 1.<minor>).
ENVTEST_K8S_VERSION ?= 1.36

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

ENVTEST ?= $(LOCALBIN)/setup-envtest
ENVTEST_VERSION ?= v0.24.1
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2
GOPLS ?= $(LOCALBIN)/gopls
GOPLS_VERSION ?= v0.22.0

.PHONY: all
all: build

.PHONY: fmt
fmt:
	go fmt ./...

.PHONY: vet
vet:
	go vet ./...

.PHONY: build
build: fmt vet
	go build -o $(LOCALBIN)/manager ./cmd

.PHONY: test
test: envtest
	KUBEBUILDER_ASSETS="$(shell $(ENVTEST) use $(ENVTEST_K8S_VERSION) --bin-dir $(LOCALBIN) -p path)" go test ./... -coverprofile cover.out

.PHONY: lint
lint: golangci-lint
	$(GOLANGCI_LINT) run

# gopls-check surfaces gopls' static diagnostics down to hint severity (the
# modernize suggestions etc. that golangci-lint does not cover). gopls check
# takes file paths, not packages, and exits 0 even with findings — so feed it the
# tracked Go files and fail when it prints anything. Findings land on stdout
# (captured) so stderr is dropped to avoid double-printing; a non-zero gopls exit
# (e.g. a load error) fails the target too, so a tool failure can't pass green.
.PHONY: gopls-check
gopls-check: gopls
	@out="$$($(GOPLS) check -severity=hint $$(git ls-files '*.go') 2>/dev/null)"; status=$$?; \
	if [ $$status -ne 0 ]; then \
		echo "gopls check failed to run (exit $$status); re-run for detail: $(GOPLS) check -severity=hint \$$(git ls-files '*.go')"; \
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
helm-lint:
	helm lint $(CHART)
	helm template rot $(CHART) --namespace node-rotation-system >/dev/null
	helm template rot $(CHART) --namespace node-rotation-system --set metrics.serviceMonitor.enabled=true >/dev/null

.PHONY: envtest
envtest: $(LOCALBIN)
	test -s $(ENVTEST) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@$(ENVTEST_VERSION)

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN)
	test -s $(GOLANGCI_LINT) || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)

.PHONY: gopls
gopls: $(LOCALBIN)
	test -s $(GOPLS) || GOBIN=$(LOCALBIN) go install golang.org/x/tools/gopls@$(GOPLS_VERSION)
