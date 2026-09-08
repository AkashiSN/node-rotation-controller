#!/usr/bin/env bash
# Parse the PromQL the chart's PrometheusRule ships.
#
# Usage:
#   check-prometheus-rules.sh                 # the repo's chart
#   check-prometheus-rules.sh path/to/chart   # a chart copy (used by the self-test)
#
# `helm lint` renders the template and helm-unittest asserts the rendered YAML's
# CONTENT — neither reads the expressions. Until this guard, a PromQL syntax
# error in charts/node-rotation-controller/templates/prometheusrule.yaml would
# have rendered, linted and unit-tested green, and surfaced only in a user's
# cluster when Prometheus rejected the rule. The alerts are also the surface most
# likely to be edited without a Go test to catch it: the §4.2 alert expressions
# have changed with almost every observability issue (#303, #321).
#
# A PrometheusRule's `spec` IS promtool's rule-file schema, so the check is
# `yq '.spec'` piped into `promtool check rules`. Nothing here asserts what the
# expressions MEAN — that is the runbook's and the spec's job; this asserts only
# that Prometheus can parse every one of them, plus their `for` durations, label
# and annotation templates.
#
# The rule count is re-derived from the rendered manifest and compared against
# what promtool reports, because promtool alone cannot tell the guard it checked
# anything: on an empty rule file it exits 0 and prints "SUCCESS: 0 rules found".
# An extraction that silently yields an empty or partial file therefore has to be
# caught here, and the two numbers come from different readings of the same
# render — grep over the raw manifest text, yq over its parsed structure — so a
# broken extraction cannot agree with itself. The self-test drives exactly that
# case (a manifest with no `.spec`).
set -euo pipefail

repo_root="${REPO_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)}"
chart="${1:-${repo_root}/charts/node-rotation-controller}"

for tool in helm yq promtool; do
  command -v "$tool" >/dev/null 2>&1 || {
    echo "::error::${tool} not found — it is pinned in aqua.yaml; run 'make aqua-tools'" >&2
    exit 1
  }
done

workdir="$(mktemp -d)"
trap 'rm -rf "${workdir}"' EXIT
manifest="${workdir}/prometheusrule.yaml"
rules="${workdir}/rules.yaml"

# --show-only fails outright when the template renders nothing, so a chart whose
# PrometheusRule silently stopped rendering cannot reach the checks below.
helm template rules-guard "$chart" \
  --set prometheusRule.enabled=true \
  --show-only templates/prometheusrule.yaml >"$manifest"

# The expected count comes from the MANIFEST, not from the extracted rule file:
# both numbers must come from a different reading of the same render, or a broken
# extraction would agree with itself.
expected="$(grep -c '^\s*- alert:' "$manifest" || true)"
if [ "${expected:-0}" -lt 1 ]; then
  echo "::error::the rendered PrometheusRule declares no alerts — the guard would be vacuous" >&2
  exit 1
fi

yq '.spec' "$manifest" >"$rules"

# promtool reports "SUCCESS: N rules found" on stdout and exits non-zero on a
# parse error. Capture both: the exit code proves the PromQL parses, the count
# proves it parsed all of it.
if ! out="$(promtool check rules "$rules" 2>&1)"; then
  echo "$out" >&2
  echo "::error::the chart's PrometheusRule contains PromQL promtool cannot parse" >&2
  exit 1
fi
echo "$out"

found="$(printf '%s\n' "$out" | sed -n 's/.*SUCCESS: \([0-9]\{1,\}\) rules found.*/\1/p' | head -1)"
if [ -z "$found" ]; then
  echo "::error::could not read the rule count from promtool's output — the guard cannot confirm it checked anything" >&2
  exit 1
fi
if [ "$found" -ne "$expected" ]; then
  echo "::error::promtool checked ${found} rules but the rendered PrometheusRule declares ${expected} — the extraction dropped rules" >&2
  exit 1
fi

echo "PromQL parses for all ${found} chart alerts"
