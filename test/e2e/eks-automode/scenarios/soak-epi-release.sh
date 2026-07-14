#!/usr/bin/env bash
# Scenario P epilogue release (issue #118, spec rev 4 §2). Interval-search from
# the single epi claim's ACTUAL deadline d = creationTimestamp + E(2h12m):
#   R = first in-window instant >= d-11m, REQUIRED: d-12m < R < d-8m, in-window
# (UTC half-hour grid, open [:00,:28) / [:30,:58)). Fails CLOSED: any unprovable
# bound or missed timing tears the epilogue down WHILE STILL FROZEN — freeze
# never stops Karpenter's expiration controller, so a late unfreeze can neither
# guarantee fallback completion (drain bound tGP+Buffer=7m) nor prevent expiry.
set -euo pipefail
pool=nodepool-soak-epi policy=nrc-soak-epi deploy=soak-epi-workload
E=7920 # 2h12m in seconds
teardown() {
  echo "FAIL-CLOSED frozen teardown: $*" >&2
  kubectl scale deploy/$deploy --replicas=0 || true
  kubectl delete rotationpolicy $policy --ignore-not-found
  kubectl delete nodepool $pool --ignore-not-found
  exit 3
}
claim=$(kubectl get nodeclaims -l karpenter.sh/nodepool=$pool -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)
[ -n "$claim" ] || teardown "no epi NodeClaim found"
# Guarded (2>/dev/null || true): a bare `var=$(kubectl ...)` assignment is NOT
# exempt from `set -e` — if the claim vanished between the lookup above and
# here, an unguarded call would crash raw and skip teardown() entirely,
# leaving the epilogue frozen with no cleanup. Route every failure through
# the explicit non-empty check below instead.
created=$(kubectl get nodeclaim "$claim" -o jsonpath='{.metadata.creationTimestamp}' 2>/dev/null || true)
[ -n "$created" ] || teardown "could not read creationTimestamp for claim $claim"
d=$(( $(date -u -d "$created" +%s) + E ))
in_window() { [ $(( $1 % 1800 )) -lt 1680 ]; }
R=$(( d - 660 ))                                  # d - 11m
in_window "$R" || R=$(( (R / 1800 + 1) * 1800 ))  # next occurrence start (gap <= 2m)
# prove the bounds: d-12m < R < d-8m, in-window
{ [ "$R" -gt $(( d - 720 )) ] && [ "$R" -lt $(( d - 480 )) ] && in_window "$R"; } \
  || teardown "R=$(date -u -d @$R +%FT%TZ) violates (d-12m, d-8m) or the window grid"
# Guarded for the same reason as `created` above — an unreachable API server
# or a pool deleted out-of-band must not crash raw past teardown().
frozen=$(kubectl get nodepool $pool -o jsonpath='{.metadata.annotations.noderotation\.io/freeze}' 2>/dev/null || true)
[ -n "$frozen" ] || teardown "pool is not frozen — refusing an unmanaged release"
echo "claim=$claim created=$created d=$(date -u -d @$d +%FT%TZ) R=$(date -u -d @$R +%FT%TZ) (d-R=$(( (d-R)/60 ))m)"
while [ "$(date -u +%s)" -lt "$R" ]; do sleep 5; done
now=$(date -u +%s)
{ [ "$now" -lt $(( d - 480 )) ] && in_window "$now"; } || teardown "missed the release interval (now=$(date -u -d @$now +%FT%TZ))"
# At this instant the pool is still frozen (the annotate below is the act of
# unfreezing) — if the API call itself fails, we are still frozen, so route
# through teardown() rather than crashing raw.
kubectl annotate nodepool $pool noderotation.io/freeze- || teardown "failed to release freeze — pool remains frozen, unmanaged state"
echo "released at $(date -u +%FT%TZ); expecting surge-less forceful fallback within tGP+Buffer=7m"
kubectl wait --for=delete "nodeclaim/$claim" --timeout=10m
echo "epi claim $claim deleted at $(date -u +%FT%TZ)"
