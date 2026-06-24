#!/usr/bin/env bash
# Assert the release tag matches the chart's version/appVersion. The git tag is
# the source of truth for a release; Chart.yaml stays the human-readable record
# and the two must agree (pre-1.0 version == appVersion move together, spec §6.1).
# Usage: check-chart-version.sh <tag>   e.g. check-chart-version.sh v0.3.0
# On success prints "version=<x.y.z>" (GITHUB_OUTPUT-shaped) and exits 0;
# on mismatch exits 1.
set -euo pipefail

tag="${1:?usage: check-chart-version.sh <tag>}"
chart_dir="${CHART_DIR:-charts/node-rotation-controller}"
chart_file="${chart_dir}/Chart.yaml"

version="${tag#v}"   # strip a leading v: v0.3.0 -> 0.3.0

chart_ver="$(grep -E '^version:' "$chart_file" | awk '{print $2}')"
app_ver="$(grep -E '^appVersion:' "$chart_file" | awk '{gsub(/"/,"",$2); print $2}')"

fail=0
if [ "$chart_ver" != "$version" ]; then
  echo "::error::tag $tag (version $version) != Chart.yaml version $chart_ver" >&2
  fail=1
fi
if [ "$app_ver" != "$version" ]; then
  echo "::error::tag $tag (version $version) != Chart.yaml appVersion $app_ver" >&2
  fail=1
fi
if [ "$fail" -ne 0 ]; then
  echo "Bump Chart.yaml version+appVersion to $version (or tag the right version) and retry." >&2
  exit 1
fi

echo "version=$version"
