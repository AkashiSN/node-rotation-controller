# Image URL for docker-build
IMG ?= ghcr.io/akashisn/node-rotation-controller:dev

# Kubernetes version for envtest assets. Keep in sync with the k8s.io/api
# minor in go.mod (v0.<minor>.x -> 1.<minor>).
ENVTEST_K8S_VERSION ?= 1.34

LOCALBIN ?= $(shell pwd)/bin
$(LOCALBIN):
	mkdir -p $(LOCALBIN)

ENVTEST ?= $(LOCALBIN)/setup-envtest
GOLANGCI_LINT ?= $(LOCALBIN)/golangci-lint
GOLANGCI_LINT_VERSION ?= v2.12.2

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

.PHONY: docker-build
docker-build:
	docker build -t $(IMG) .

.PHONY: envtest
envtest: $(LOCALBIN)
	test -s $(ENVTEST) || GOBIN=$(LOCALBIN) go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

.PHONY: golangci-lint
golangci-lint: $(LOCALBIN)
	test -s $(GOLANGCI_LINT) || GOBIN=$(LOCALBIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
