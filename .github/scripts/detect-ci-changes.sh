#!/usr/bin/env bash
# Classify a list of changed file paths (newline-separated on stdin) into the
# four CI concern flags. Pure: no git, no GitHub Actions context — the workflow
# feeds it `git diff --name-only` output. Unit-tested by detect-ci-changes.test.sh.
set -euo pipefail

changed="$(cat)"

has() { grep -qE "$1" <<<"$changed"; }   # here-string: no pipe, so no SIGPIPE under pipefail

go=false; chart=false; docker=false; infra=false
if has '(\.go$|^go\.(mod|sum)$|^api/|^config/)'; then go=true; fi
if has '^charts/'; then chart=true; fi
if has '^Dockerfile$'; then docker=true; fi
if has '(^Makefile$|^aqua\.yaml$|^aqua-policy\.yaml$|^aqua/|^\.github/workflows/ci\.yaml$|^\.github/scripts/)'; then infra=true; fi

printf 'go=%s\nchart=%s\ndocker=%s\ninfra=%s\n' "$go" "$chart" "$docker" "$infra"
