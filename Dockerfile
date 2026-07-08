# Build the manager binary. Pin the Go toolchain to the version declared in
# go.mod, by digest for reproducibility. When bumping the go.mod Go version,
# update this tag and re-resolve the digest:
#   docker buildx imagetools inspect golang:<ver>-bookworm --format '{{.Manifest.Digest}}'
FROM golang:1.26.4-bookworm@sha256:b305420a68d0f229d91eb3b3ed9e519fcf2cf5461da4bef997bf927e8c0bfd2b AS builder
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
FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
WORKDIR /
COPY --from=builder /workspace/manager .
USER 65532:65532
ENTRYPOINT ["/manager"]
