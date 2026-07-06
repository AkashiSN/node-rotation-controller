#!/usr/bin/env bash
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/detect-ci-changes.sh"
fail=0

# assert <name> <expected 4-line block> <input paths...>
assert() {
  local name="$1" expected="$2"; shift 2
  local got; got="$(printf '%s\n' "$@" | "$script")"
  if [ "$got" != "$expected" ]; then
    echo "FAIL: $name"; echo "  expected: $(echo "$expected" | tr '\n' ' ')"; echo "  got:      $(echo "$got" | tr '\n' ' ')"; fail=1
  else echo "ok: $name"; fi
}

ALL_FALSE=$'go=false\nchart=false\ndocker=false\ninfra=false'
assert "docs-only"        "$ALL_FALSE"                                    "docs/specification/03-design.md" "README.md" "docs/ja/runbook.md"
assert "chart-only"       $'go=false\nchart=true\ndocker=false\ninfra=false'   "charts/node-rotation-controller/values.yaml"
assert "go-only"          $'go=true\nchart=false\ndocker=false\ninfra=false'   "internal/reconciler/foo.go"
assert "api-generates"    $'go=true\nchart=true\ndocker=false\ninfra=false'    "api/v1/rotationpolicy_types.go" "charts/node-rotation-controller/crds/noderotation.io_rotationpolicies.yaml"
assert "dockerfile"       $'go=false\nchart=false\ndocker=true\ninfra=false'   "Dockerfile"
assert "infra-makefile"   $'go=false\nchart=false\ndocker=false\ninfra=true'   "Makefile"
assert "infra-detector"   $'go=false\nchart=false\ndocker=false\ninfra=true'   ".github/scripts/detect-ci-changes.sh"
assert "config-is-go"     $'go=true\nchart=false\ndocker=false\ninfra=false'   "config/crd/bases/noderotation.io_rotationpolicies.yaml"
assert "gomod"            $'go=true\nchart=false\ndocker=false\ninfra=false'   "go.mod"
assert "golangci-config"  $'go=true\nchart=false\ndocker=false\ninfra=false'   ".golangci.yml"
assert "dockerignore"     $'go=false\nchart=false\ndocker=true\ninfra=false'   ".dockerignore"
assert "empty-input"      "$ALL_FALSE"                                    ""

[ "$fail" -eq 0 ] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }
