#!/usr/bin/env bash
# Self-test for check-prometheus-rules.sh, run by CI ahead of the guard itself.
#
# A guard that only ever runs against a chart that already passes proves nothing:
# a silent exit 0 is indistinguishable from a guard that checked nothing. Each
# case below breaks ONE thing in a COPY of the chart and asserts the guard says
# so — a bad expression, a template that renders no alerts, and an extraction
# that comes back empty. The unmodified chart is asserted to pass, so a case that
# fails for an unrelated reason is visible rather than mistaken for the control.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
guard="$here/check-prometheus-rules.sh"
repo_root="$(cd "$here/../.." && pwd)"
chart_src="$repo_root/charts/node-rotation-controller"
template="templates/prometheusrule.yaml"
fail=0

# copy_chart <dest> — a full copy, so a case may edit the template freely.
copy_chart() {
  cp -r "$chart_src" "$1"
}

# assert_pass <name> <chart dir>
assert_pass() {
  local name="$1" chart="$2" out
  if out="$(bash "$guard" "$chart" 2>&1)"; then
    echo "ok: $name"
  else
    echo "FAIL: $name — the guard rejected a chart it should accept"
    printf '%s\n' "$out" | sed 's/^/    /'
    fail=1
  fi
}

# assert_rejected <name> <chart dir> <expected substring>
assert_rejected() {
  local name="$1" chart="$2" want="$3" out
  if out="$(bash "$guard" "$chart" 2>&1)"; then
    echo "FAIL: $name — the guard ACCEPTED a chart it must reject"
    printf '%s\n' "$out" | sed 's/^/    /'
    fail=1
    return
  fi
  if ! printf '%s\n' "$out" | grep -qF "$want"; then
    echo "FAIL: $name — rejected, but not for the reason under test"
    echo "    expected to find: $want"
    printf '%s\n' "$out" | sed 's/^/    /'
    fail=1
    return
  fi
  echo "ok: $name"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# Control: the chart as committed must pass, or every rejection below could be
# an artifact of the harness rather than of the break it introduces.
assert_pass "the committed chart parses" "$chart_src"

# A PromQL syntax error must be caught. `increase(...)` with an unclosed bracket
# renders as valid YAML and reaches promtool as a broken expression — exactly the
# failure mode helm lint and helm-unittest let through.
broken="$tmp/broken-expr"
copy_chart "$broken"
sed -i 's|expr: increase(noderotation_completed_total{outcome=~"failure\|expired"}\[1h\]) > 0|expr: increase(noderotation_completed_total{outcome=~"failure\|expired"}[1h) > 0|' \
  "$broken/$template"
grep -q 'expr: increase(noderotation_completed_total{outcome=~"failure|expired"}\[1h) > 0' "$broken/$template" || {
  echo "FAIL: harness — the broken-expr case did not modify the template; the assertion below would be vacuous"
  fail=1
}
assert_rejected "a PromQL syntax error is rejected" "$broken" "PromQL promtool cannot parse"

# A template that renders a PrometheusRule with no alerts at all must be rejected
# rather than passing silently: zero rules checked is not zero problems found.
noalerts="$tmp/no-alerts"
copy_chart "$noalerts"
sed -i '/^        # A rotation finished as failure or expired in the last hour/,/^{{- end }}$/{/^{{- end }}$/!d}' \
  "$noalerts/$template"
grep -q '^\s*- alert:' "$noalerts/$template" && {
  echo "FAIL: harness — the no-alerts case still declares alerts; the assertion below would be vacuous"
  fail=1
}
assert_rejected "a rule set with no alerts is rejected" "$noalerts" "declares no alerts"

# The extraction reads `.spec`. If the manifest ever stops carrying one — a
# restructured template, a renamed key — the guard must fail loudly instead of
# handing promtool an empty file and reporting success. This is the case that
# earns the count comparison: promtool itself exits 0 on an empty rule file and
# prints "SUCCESS: 0 rules found", so without the comparison the guard would go
# green having checked nothing. The rejection must name the mismatch, not merely
# be a rejection.
nospec="$tmp/no-spec"
copy_chart "$nospec"
sed -i '0,/^spec:$/s//specx:/' "$nospec/$template"
grep -q '^specx:' "$nospec/$template" || {
  echo "FAIL: harness — the no-spec case did not rename the key; the assertion below would be vacuous"
  fail=1
}
# Matched on the branch's wording rather than on the counts it prints, so adding
# a ninth alert does not turn this case red.
assert_rejected "an extraction that drops every rule is rejected" "$nospec" \
  "the extraction dropped rules"

[ "$fail" -eq 0 ] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }
