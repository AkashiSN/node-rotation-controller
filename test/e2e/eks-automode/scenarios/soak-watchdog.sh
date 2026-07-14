#!/usr/bin/env bash
# Scenario P watchdog: polls metrics/pods/events/recorder-freshness every 60s
# and prints ONE line per NEW anomaly (stateful via <run-dir>/.watchdog/).
# Silent when healthy. Anomalies (spec rev 4 §4.3):
#   expired>0, main-pool fallback>0, failure increment, short_lead>0,
#   controller restart, ShortLead event, recorder gap>5m, scrape unreachable.
set -euo pipefail
run="${1:?usage: soak-watchdog.sh <run-dir>}"; main="${2:-nodepool-soak}"
ns=node-rotation-system state="$run/.watchdog"; sel=app.kubernetes.io/name=node-rotation-controller; mkdir -p "$state"
alert() { local key="$1"; shift; if [ ! -f "$state/$key" ]; then echo "ALERT $(date -u +%FT%TZ) $*"; touch "$state/$key"; fi }
metric_sum() { # $1=metrics text  $2=grep pattern -> integer sum of values
  # The grep is grouped with `|| true` so a legitimate zero-match (the common,
  # healthy case — e.g. no expired outcomes yet) doesn't leave grep's exit
  # status (1) as the pipeline's status. Under `set -euo pipefail`, a bare
  # `var=$(metric_sum ...)` assignment IS checked by errexit (unlike a command
  # substitution buried inside a larger expression), so a nonzero return here
  # would silently kill the whole watchdog loop on its very first healthy
  # poll. awk always exits 0, so grouping keeps this function's own exit
  # status at 0 regardless of match count.
  { echo "$1" | grep -E "$2" || true; } | awk '{s+=$NF} END {printf "%d", s+0}'
}
while true; do
  pod=$(kubectl get pods -n "$ns" -l "$sel" -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
  if [ -z "$pod" ]; then alert nopod "controller pod not found"; sleep 60; continue; fi
  m=$(kubectl get --raw "/api/v1/namespaces/$ns/pods/${pod}:8080/proxy/metrics" 2>/dev/null | grep '^noderotation_' || true)
  if [ -z "$m" ]; then alert noscrape "metrics unreachable via pods/proxy"; else rm -f "$state/noscrape"
    exp=$(metric_sum "$m" 'noderotation_completed_total\{.*outcome="expired"')
    [ "$exp" -gt 0 ] && alert "expired$exp" "EXPIRED outcome reached $exp — the backstop won; freeze pools and investigate"
    # Quote-anchored on the nodepool label value (`nodepool="$main"`, not a
    # bare substring match): $main=nodepool-soak is a PREFIX of the epi pool's
    # own name nodepool-soak-epi, whose forceful fallback is EXPECTED to fire
    # once by design (spec rev 4 §2). An unanchored match here would raise a
    # false "mainff" alert on every intended epi release.
    ff=$(metric_sum "$m" "noderotation_forceful_fallback_total\{.*nodepool=\"$main\"")
    [ "$ff" -gt 0 ] && alert "mainff$ff" "forceful fallback fired on $main (count $ff) during the main soak"
    fail=$(metric_sum "$m" 'noderotation_completed_total\{.*outcome="failure"')
    [ "$fail" -gt "$(cat "$state/failcount" 2>/dev/null || echo 0)" ] && { echo "ALERT $(date -u +%FT%TZ) failure outcome now $fail (was $(cat "$state/failcount" 2>/dev/null || echo 0))"; echo "$fail" > "$state/failcount"; }
    sl=$(metric_sum "$m" 'noderotation_short_lead_nodes')
    [ "$sl" -gt 0 ] && alert "shortlead" "short_lead_nodes=$sl"
  fi
  # Pure-bash sum instead of piping through `bc`: this host (and possibly the
  # runner) may not have bc installed, and the brief's `... | bc 2>/dev/null
  # || echo 0` swallows that as a silent "0 restarts" — masking real restarts
  # instead of alerting on them.
  rc=0
  rc_list=$(kubectl get pods -n "$ns" -l "$sel" -o jsonpath='{.items[*].status.containerStatuses[*].restartCount}' 2>/dev/null) || rc_list=""
  for n in $rc_list; do rc=$(( rc + n )); done
  [ "$rc" -gt 0 ] && alert "restart$rc" "controller restartCount=$rc — counters reset"
  kubectl get events -A --field-selector reason=ShortLead -o name 2>/dev/null | grep -q . && alert shortleadev "ShortLead event present"
  if [ -f "$run/snapshots.jsonl" ]; then
    age=$(( $(date +%s) - $(stat -c %Y "$run/snapshots.jsonl") ))
    [ "$age" -gt 300 ] && alert "recgap$((age/300))" "local recorder stale ${age}s"
  fi
  sleep 60
done
