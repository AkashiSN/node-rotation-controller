#!/usr/bin/env bash
# Verify the Go toolchain version is identical across every place it is pinned.
#
# The version is hand-duplicated in four build inputs that must stay in lockstep
# (see CONTRIBUTING / the comments on each pin). Renovate manages three of them
# in separate managers and cannot manage the KWOK heredoc at all, so nothing but
# this guard catches a drift — including the case where a module bump (e.g.
# karpenter) forces a higher Go version that only some of the pins follow.
#
# Usage: check-go-toolchain-sync.sh
set -euo pipefail

repo_root="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"

fail=0
error() {
  echo "::error::$*" >&2
  fail=1
}

# extract <file> <sed/awk pipeline label> — echoes the version or empty string.
extract() {
  local file="${repo_root}/$1"
  [[ -f "$file" ]] || { error "$1: file not found"; return; }
  case "$1" in
    go.mod)
      # `go 1.26.5` directive (first bare `go <version>` line).
      awk '$1=="go" && $2 ~ /^[0-9]/ {print $2; exit}' "$file"
      ;;
    aqua.yaml)
      # `- name: golang/go@go1.26.5`
      sed -n 's/.*golang\/go@go\([0-9][0-9.]*\).*/\1/p' "$file" | head -n1
      ;;
    Dockerfile)
      # `FROM golang:1.26.5-bookworm@sha256:...`
      sed -n 's/.*golang:\([0-9][0-9.]*\)-bookworm.*/\1/p' "$file" | head -n1
      ;;
    test/e2e/kwok/build-kwok-image.sh)
      # `go 1.26.5` inside the generated go.mod heredoc.
      awk '$1=="go" && $2 ~ /^[0-9]/ {print $2; exit}' "$file"
      ;;
    *)
      error "$1: no extractor defined"
      ;;
  esac
}

files=(
  go.mod
  aqua.yaml
  Dockerfile
  test/e2e/kwok/build-kwok-image.sh
)

reference=""
reference_file=""
for f in "${files[@]}"; do
  version="$(extract "$f")"
  if [[ -z "$version" ]]; then
    error "$f: could not extract a Go toolchain version"
    continue
  fi
  printf '  %-34s %s\n' "$f" "$version" >&2
  if [[ -z "$reference" ]]; then
    reference="$version"
    reference_file="$f"
  elif [[ "$version" != "$reference" ]]; then
    error "$f pins Go ${version}, but ${reference_file} pins ${reference} — all four must match"
  fi
done

if [[ "$fail" -ne 0 ]]; then
  echo "Bump the Go toolchain in lockstep across go.mod, aqua.yaml, Dockerfile, and test/e2e/kwok/build-kwok-image.sh." >&2
  exit 1
fi

echo "Go toolchain is synchronized at ${reference}"
