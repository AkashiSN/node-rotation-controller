#!/usr/bin/env bash
# Scenario P local recorder (secondary evidence layer; the in-cluster scraper is
# the recorder of record). Usage: soak-record.sh <run-dir>. Runs until killed.
#   snapshots.jsonl   every 120s: nodeclaims+nodes+pods+rotationpolicies (full
#                     specs preserved BEFORE deletion — deleted claims cannot be
#                     queried later)
#   events-list.jsonl every 120s: full event list (watch-loss backstop)
#   events-watch.log  kubectl get events -A -w stream (auto-restarting)
#   controller.log    kubectl logs -f of the controller (auto-restarting;
#                     the "rotation complete" lines are the rotation ledger)
#   placeholders.log  pods -w on the noderotation.io/surge-for label — the
#                     continuous placeholder ledger (proves "no placeholder"
#                     for the epi claim)
set -euo pipefail
run="${1:?usage: soak-record.sh <run-dir>}"; mkdir -p "$run"
ns=node-rotation-system
watchloop() { # $1=outfile, rest=command; restarts on exit, stamps restarts
  local out="$1"; shift
  while true; do
    echo "== $(date -u +%FT%TZ) (re)start: $*" >> "$out"
    "$@" >> "$out" 2>>"$run/recorder-errors.log" || true
    sleep 5
  done
}
snaploop() {
  while true; do
    local ts; ts=$(date -u +%FT%TZ)
    # nodeclaims/nodes/rotationpolicies are cluster-scoped and pods is
    # namespaced; -A is a documented no-op for cluster-scoped kinds (kubectl
    # just omits the namespace segment), so one uniform loop is correct for
    # all four kinds without special-casing.
    for kind in nodeclaims nodes pods rotationpolicies; do
      kubectl get "$kind" -A -o json 2>>"$run/recorder-errors.log" \
        | jq -c --arg ts "$ts" --arg kind "$kind" '{ts:$ts, kind:$kind, items:.items}' \
        >> "$run/snapshots.jsonl" || echo "{\"ts\":\"$ts\",\"kind\":\"$kind\",\"error\":true}" >> "$run/snapshots.jsonl"
    done
    kubectl get events -A -o json 2>>"$run/recorder-errors.log" \
      | jq -c --arg ts "$ts" '{ts:$ts, items:.items}' >> "$run/events-list.jsonl" || true
    sleep 120
  done
}
trap 'kill 0' INT TERM
snaploop &
watchloop "$run/events-watch.log" kubectl get events -A -w -o wide &
watchloop "$run/controller.log" kubectl logs -n "$ns" -f -l app.kubernetes.io/name=node-rotation-controller --all-containers --prefix --timestamps --tail=-1 &
watchloop "$run/placeholders.log" kubectl get pods -A -w -l noderotation.io/surge-for -o wide &
echo "recording into $run (pid $$)"
wait
