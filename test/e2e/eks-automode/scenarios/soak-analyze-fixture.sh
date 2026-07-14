#!/usr/bin/env bash
# Scenario P analysis dry-run (spec rev4 §3.2): builds a synthetic run-dir
# (controller.log, snapshots.jsonl, scrape.log) with a known-by-construction
# answer, runs soak-analyze.py against it, and asserts the report matches —
# proving the analyzer works BEFORE the real 12h run produces any data.
#
# Covers: 3 rotations at margins +70m/+45m/+0.5m; a counter reset mid-stream on
# one scraped pod; an absent `expired` series (must read as 0, not crash); at
# least one SCRAPE_ERROR line; a seq gap; a clean end-census (young right-
# censored replacement claims, no stale/failed claims).
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
analyze="$here/soak-analyze.py"
run="$(mktemp -d)"
trap 'rm -rf "$run"' EXIT

die() { echo "FIXTURE FAIL: $*" >&2; exit 1; }
must_contain() { grep -qF "$2" "$run/report.md" || die "report.md missing expected text: $2"; }

# --- snapshots.jsonl -------------------------------------------------------
# Births: nc-1 @ T, nc-2 @ T+5m, nc-3 @ T+10m (T = 2026-07-14T00:00:00Z). An
# error-line and a duplicate-kind decoy line exercise the skip paths. The LAST
# nodeclaims snapshot (the criterion-9 census) shows the 3 old claims already
# gone, replaced by 2 young claims — right-censored, must not be flagged.
cat > "$run/snapshots.jsonl" <<'EOF'
{"ts":"2026-07-14T00:11:00Z","kind":"nodeclaims","items":[{"metadata":{"name":"nc-1","creationTimestamp":"2026-07-14T00:00:00Z","labels":{"karpenter.sh/nodepool":"nodepool-soak"},"annotations":{}}},{"metadata":{"name":"nc-2","creationTimestamp":"2026-07-14T00:05:00Z","labels":{"karpenter.sh/nodepool":"nodepool-soak"},"annotations":{}}},{"metadata":{"name":"nc-3","creationTimestamp":"2026-07-14T00:10:00Z","labels":{"karpenter.sh/nodepool":"nodepool-soak"},"annotations":{}}}]}
{"ts":"2026-07-14T00:13:00Z","kind":"nodeclaims","error":true}
{"ts":"2026-07-14T00:20:00Z","kind":"pods","items":[]}
{"ts":"2026-07-14T02:25:00Z","kind":"nodeclaims","items":[{"metadata":{"name":"nc-1b","creationTimestamp":"2026-07-14T02:20:00Z","labels":{"karpenter.sh/nodepool":"nodepool-soak"},"annotations":{}}},{"metadata":{"name":"nc-2b","creationTimestamp":"2026-07-14T02:21:00Z","labels":{"karpenter.sh/nodepool":"nodepool-soak"},"annotations":{}}}]}
EOF

# --- controller.log ----------------------------------------------------
# kubectl logs --prefix --timestamps shape: "[pod/x] <k8s-ts> <stdout-line>",
# with soak-record.sh's own "== <ts> (re)start:" headers interleaved. Margins:
#   nc-1: deadline 00:00:00+7920s=02:12:00Z, completion 01:02:00Z -> +70m
#   nc-2: deadline 00:05:00+7920s=02:17:00Z, completion 01:32:00Z -> +45m
#   nc-3: deadline 00:10:00+7920s=02:22:00Z, completion 02:21:30Z -> +0.5m
cat > "$run/controller.log" <<'EOF'
== 2026-07-14T00:00:00Z (re)start: kubectl logs -n node-rotation-system -f -l app.kubernetes.io/name=node-rotation-controller --all-containers --prefix --timestamps --tail=-1
[pod/node-rotation-controller-0] 2026-07-14T01:02:00.100000000Z {"level":"info","ts":"2026-07-14T01:02:00Z","msg":"rotation complete","nodeclaim":"nc-1","nodepool":"nodepool-soak","mode":"surge","surgeNode":"ip-10-0-1-11","surgeWait":"1m30s","drain":"4m0s","total":"5m30s"}
[pod/node-rotation-controller-0] 2026-07-14T01:32:00.100000000Z {"level":"info","ts":"2026-07-14T01:32:00Z","msg":"rotation complete","nodeclaim":"nc-2","nodepool":"nodepool-soak","mode":"surge","surgeNode":"ip-10-0-1-12","surgeWait":"1m0s","drain":"3m0s","total":"4m0s"}
[pod/node-rotation-controller-0] 2026-07-14T02:21:30.100000000Z {"level":"info","ts":"2026-07-14T02:21:30Z","msg":"rotation complete","nodeclaim":"nc-3","nodepool":"nodepool-soak","mode":"surge","surgeNode":"ip-10-0-1-13","surgeWait":"1m0s","drain":"2m0s","total":"3m0s"}
EOF

# --- scrape.log --------------------------------------------------------
# Two pods. pod-a's success counter resets mid-stream (seq2 8 -> seq4 2): the
# reset-tolerant delta sum must still add the post-reset epoch rather than
# drop or negate it. seq 3 is entirely absent (gap). One SCRAPE_ERROR line.
# `expired` has zero lines anywhere -> must read as 0, not crash.
#
#   pod-a success: 5(seq1) 8(seq2) [reset] 2(seq4) 6(seq5) -> deltas 5+3+2+4=14
#   pod-a failure: 0 1 1 1                                 -> deltas 0+1+0+0=1
#   pod-b success: 0(seq1) [error@seq2] 3(seq4) 3(seq5)     -> deltas 0+3+0=3
#   totals: success=17 failure=1 expired=0 (absent) forceful_fallback=0 (absent)
cat > "$run/scrape.log" <<'EOF'
1 2026-07-14T01:00:00Z pod-a noderotation_completed_total{nodepool="nodepool-soak",outcome="success"} 5
1 2026-07-14T01:00:00Z pod-a noderotation_completed_total{nodepool="nodepool-soak",outcome="failure"} 0
1 2026-07-14T01:00:00Z pod-b noderotation_completed_total{nodepool="nodepool-soak",outcome="success"} 0
2 2026-07-14T01:01:00Z pod-a noderotation_completed_total{nodepool="nodepool-soak",outcome="success"} 8
2 2026-07-14T01:01:00Z pod-a noderotation_completed_total{nodepool="nodepool-soak",outcome="failure"} 1
2 2026-07-14T01:01:00Z pod-b SCRAPE_ERROR proxy
4 2026-07-14T01:04:00Z pod-a noderotation_completed_total{nodepool="nodepool-soak",outcome="success"} 2
4 2026-07-14T01:04:00Z pod-a noderotation_completed_total{nodepool="nodepool-soak",outcome="failure"} 1
4 2026-07-14T01:04:00Z pod-b noderotation_completed_total{nodepool="nodepool-soak",outcome="success"} 3
5 2026-07-14T01:05:00Z pod-a noderotation_completed_total{nodepool="nodepool-soak",outcome="success"} 6
5 2026-07-14T01:05:00Z pod-a noderotation_completed_total{nodepool="nodepool-soak",outcome="failure"} 1
5 2026-07-14T01:05:00Z pod-b noderotation_completed_total{nodepool="nodepool-soak",outcome="success"} 3
EOF

python3 "$analyze" "$run" > "$run/stdout.log" || die "analyzer exited non-zero: $(cat "$run/stdout.log")"
[ -f "$run/report.md" ] || die "report.md was not written"

must_contain "rotation count" "rotations (ledger): 3"
must_contain "margin distribution" "min=0.5m median=45.0m max=70.0m"
must_contain "criterion 1 expired PASS" "| 1 expired==0 | 0 | PASS |"
must_contain "success survives the reset (17 = pod-a 14 + pod-b 3, spanning the reset)" "success=17"
must_contain "the seq gap is reported" "seq 3-3 missing"
must_contain "criterion 8 margins PASS" "| 8 all margins>0 | min 0.5m | PASS |"
must_contain "criterion 9 census clean (young replacements right-censored)" "| 9 end census clean | 0 stale, 0 failed | PASS |"

echo "fixture ok"
