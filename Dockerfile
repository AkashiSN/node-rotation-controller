# Build the manager binary. Pin the Go toolchain to the version declared in
# go.mod, by digest for reproducibility. When bumping the go.mod Go version,
# update this tag and re-resolve the digest:
#   docker buildx imagetools inspect golang:<ver>-bookworm --format '{{.Manifest.Digest}}'
FROM golang:1.26.5-bookworm@sha256:1ecb7edf62a0408027bd5729dfd6b1b8766e578e8df93995b225dfd0944eb651 AS builder
ARG TARGETOS
ARG TARGETARCH

WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
# TARGETOS/TARGETARCH are injected by buildx; fall back to the host's values so
# a plain `docker build` (e.g. `make docker-build`) is explicit, not empty.
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-$(go env GOOS)} GOARCH=${TARGETARCH:-$(go env GOARCH)} go build -o manager ./cmd

# Minimal runtime image. static-debian12 names the Debian major explicitly (the
# `static` alias tracks whatever the current distroless base is), pinned by
# digest for reproducibility. Re-resolve when bumping:
#   docker buildx imagetools inspect gcr.io/distroless/static-debian12:nonroot --format '{{.Manifest.Digest}}'
FROM gcr.io/distroless/static-debian12:nonroot@sha256:aef9602f8710ec12bde19d593fed1f76c708531bb7aba205110f1029786ead7b
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
