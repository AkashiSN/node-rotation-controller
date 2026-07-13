#!/usr/bin/env bash
# Classify a list of changed file paths (newline-separated on stdin) into the
# five CI concern flags. Pure: no git, no GitHub Actions context — the workflow
# feeds it `git diff --name-only` output. Unit-tested by detect-ci-changes.test.sh.
set -euo pipefail

changed="$(cat)"

has() { grep -qE "$1" <<<"$changed"; }   # here-string: no pipe, so no SIGPIPE under pipefail

go=false; chart=false; docker=false; infra=false; docs=false
if has '(\.go$|^go\.(mod|sum)$|^api/|^config/|^\.golangci\.ya?ml$)'; then go=true; fi
if has '^charts/'; then chart=true; fi
if has '(^Dockerfile$|^\.dockerignore$)'; then docker=true; fi
if has '(^Makefile$|^aqua\.yaml$|^aqua-policy\.yaml$|^aqua/|^\.github/workflows/ci\.yaml$|^\.github/scripts/)'; then infra=true; fi
# The docs site now BUILDS the wasm policy simulator from Go (issue #240), so the docs
# build is a gate on those sources too — a change to internal/sim or cmd/wasm changes
# the module the simulator page serves.
if has '(^docs/|^README(\.ja)?\.md$|^package(-lock)?\.json$|^Makefile$|^aqua\.yaml$|^go\.(mod|sum)$|^api/|^cmd/wasm/|^internal/)'; then docs=true; fi

printf 'go=%s\nchart=%s\ndocker=%s\ninfra=%s\ndocs=%s\n' "$go" "$chart" "$docker" "$infra" "$docs"
