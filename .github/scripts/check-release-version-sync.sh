#!/usr/bin/env bash
# Verify every current-release marker that must move with Chart.yaml.
#
# Usage:
#   check-release-version-sync.sh             # use Chart.yaml version
#   check-release-version-sync.sh v0.7.0      # also require this version
#
# This guard intentionally checks only current-release markers, not historical
# release references such as old runbook rows or validation evidence.
set -euo pipefail

repo_root="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
chart_file="${repo_root}/charts/node-rotation-controller/Chart.yaml"

chart_version="$(awk '/^version:/ {print $2}' "$chart_file")"
app_version="$(awk '/^appVersion:/ {gsub(/"/, "", $2); print $2}' "$chart_file")"
expected="${1:-$chart_version}"
expected="${expected#v}"
release="v${expected}"

fail=0

error() {
  echo "::error::$*" >&2
  fail=1
}

require_literal() {
  local file="$1"
  local literal="$2"
  local description="$3"

  if ! grep -Fq "$literal" "${repo_root}/${file}"; then
    error "${file}: ${description}; expected literal: ${literal}"
  fi
}

if [[ "$chart_version" != "$expected" ]]; then
  error "Chart.yaml version ${chart_version} != expected ${expected}"
fi
if [[ "$app_version" != "$expected" ]]; then
  error "Chart.yaml appVersion ${app_version} != expected ${expected}"
fi

require_literal "README.md" \
  "status-${release}_released_" \
  "status badge is not synchronized"
require_literal "README.md" \
  "**${release} —" \
  "current release heading is not synchronized"
require_literal "README.ja.md" \
  "status-${release}_released_" \
  "Japanese status badge is not synchronized"
require_literal "README.ja.md" \
  "**${release} —" \
  "Japanese current release heading is not synchronized"
require_literal "AGENTS.md" \
  "The latest release is **${release}**" \
  "agent-facing current release is not synchronized"
require_literal "CONTRIBUTING.md" \
  "The latest release is **${release}**" \
  "contributor-facing current release is not synchronized"
require_literal "docs/runbook.md" \
  "| ${release} |" \
  "CRD-change table has no release row"
require_literal "docs/ja/runbook.md" \
  "| ${release} |" \
  "Japanese CRD-change table has no release row"

if [[ "$fail" -ne 0 ]]; then
  echo "Update every item in CONTRIBUTING.md's release version synchronization checklist." >&2
  exit 1
fi

echo "release version markers are synchronized at ${release}"
