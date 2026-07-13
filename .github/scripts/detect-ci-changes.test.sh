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

ALL_FALSE=$'go=false\nchart=false\ndocker=false\ninfra=false\ndocs=false'
assert "chart-only"       $'go=false\nchart=true\ndocker=false\ninfra=false\ndocs=false'   "charts/node-rotation-controller/values.yaml"
assert "go-only"          $'go=true\nchart=false\ndocker=false\ninfra=false\ndocs=true'   "internal/reconciler/foo.go"
assert "api-generates"    $'go=true\nchart=true\ndocker=false\ninfra=false\ndocs=true'    "api/v1/rotationpolicy_types.go" "charts/node-rotation-controller/crds/noderotation.io_rotationpolicies.yaml"
assert "dockerfile"       $'go=false\nchart=false\ndocker=true\ninfra=false\ndocs=false'   "Dockerfile"
assert "infra-makefile"   $'go=false\nchart=false\ndocker=false\ninfra=true\ndocs=true'   "Makefile"
assert "infra-detector"   $'go=false\nchart=false\ndocker=false\ninfra=true\ndocs=false'   ".github/scripts/detect-ci-changes.sh"
assert "config-is-go"     $'go=true\nchart=false\ndocker=false\ninfra=false\ndocs=false'   "config/crd/bases/noderotation.io_rotationpolicies.yaml"
assert "gomod"            $'go=true\nchart=false\ndocker=false\ninfra=false\ndocs=true'   "go.mod"
assert "golangci-config"  $'go=true\nchart=false\ndocker=false\ninfra=false\ndocs=false'   ".golangci.yml"
assert "dockerignore"     $'go=false\nchart=false\ndocker=true\ninfra=false\ndocs=false'   ".dockerignore"
assert "docs-md"          $'go=false\nchart=false\ndocker=false\ninfra=false\ndocs=true'  "docs/specification/03-design.md"
assert "docs-package"     $'go=false\nchart=false\ndocker=false\ninfra=false\ndocs=true'  "package.json"
assert "readme-is-docs"   $'go=false\nchart=false\ndocker=false\ninfra=false\ndocs=true'  "README.md"
# The simulator page RUNS these packages: a change to them changes the wasm module the
# page serves, so the docs build (and the Pages redeploy) must see it.
assert "sim-go-is-docs"   $'go=true\nchart=false\ndocker=false\ninfra=false\ndocs=true'   "internal/sim/loop.go"
assert "simapi-is-docs"   $'go=true\nchart=false\ndocker=false\ninfra=false\ndocs=true'   "internal/simapi/simapi.go"
assert "cmdwasm-is-docs"  $'go=true\nchart=false\ndocker=false\ninfra=false\ndocs=true'   "cmd/wasm/main.go"
assert "docs-only"        $'go=false\nchart=false\ndocker=false\ninfra=false\ndocs=true'  "docs/specification/03-design.md" "README.md" "docs/ja/runbook.md"
assert "empty-input"      "$ALL_FALSE"                                                    ""

[ "$fail" -eq 0 ] && echo "ALL PASS" || { echo "SOME FAILED"; exit 1; }
