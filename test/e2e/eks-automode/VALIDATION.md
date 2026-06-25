# Recorded validation runs

This is the **recorded outcome** of running the [`SCENARIOS.md`](SCENARIOS.md)
runbook against a real EKS Auto Mode cluster. `SCENARIOS.md` is the *how* (the
reproducible steps, expected output, and gotchas); this file is the *what
happened* — a dated log of actual results, so a reader can see that the spec
[§7.2](../../../docs/specification.md#72-validated-assumptions) assumptions were
exercised end-to-end, not just described.

Each run records the artifacts under test, the per-scenario verdict with the
concrete evidence observed, and any findings. Node/NodeClaim IDs and exact
seconds are run-specific; the **shape** is what matters and is what the runbook
guarantees is reproducible.

---

## Run: 2026-06-24 — v0.3.0-rc.2 (pre-GA re-validation)

**Artifacts under test**

| | |
|---|---|
| Controller image | `ghcr.io/akashisn/node-rotation-controller:0.3.0-rc.2` |
| Helm chart | `node-rotation-controller-0.3.0-rc.2` (appVersion `0.3.0-rc.2`) |
| Platform | EKS Auto Mode, Kubernetes 1.33, `karpenter.sh/v1`, `us-west-2` |
| Policy | `RotationPolicy` CRD (`ageThreshold: 5m`, 24×7 window, `surge.readyTimeout: 15m`, `cooldownAfter: 1m`, `retryBackoff: 30m`), `expireAfter: 336h` |

**Verdict: all scenarios PASS.** rc.2 rotation behavior is identical to rc.1;
this run re-confirms the surge MVP is GA-ready. The spec §7.2 *Validated
Assumptions* rows already capture every behavior below (rc.1-era); rc.2 exercises
the same content on the released artifacts.

| Scenario | Verdict | Evidence observed |
|----------|---------|-------------------|
| 0 — core make-before-break surge | PASS | surge NodeClaim Ready **before** old drains; `surge_wait`→`drain` phase order; `completed_total{success}` increments; `in_progress=0`; no `failure`/`expired` |
| A — same-AZ zonal-EBS rebind | PASS | surge node same AZ (`us-west-2a`); after drain the stateful pod is Running on the new node with the **same PV re-attached** and the sentinel file intact |
| B — `limits` exhaustion gates surge | PASS | `limits.cpu=2` (zero headroom) → `candidates=1` but `in_progress=0`, log `insufficient limits headroom; cannot surge`, claim count held at 1, no new node |
| C — `readyTimeout` rollback | PASS | `readyTimeout=15s` → surge started then rolled back: candidate retained `state=failed`, `retry-count=1`, node un-cordoned, placeholder deleted, induced surge claim reaped; `completed_total{failure}=1` |
| D — `expireAfter` backstop (R6) | PASS | `expireAfter=336h` retained; `age_threshold_seconds=300`, `rotation_chances=13` (≫ K=2), `window_period_seconds=86400`, `short_lead_nodes=0` |
| E — `expired` outcome | PASS | froze pool at candidate `state=pending` then deleted the candidate NodeClaim → `completed_total{expired}=1` (not success/failure), anchor cleared, no placeholder/surge left |
| F — multi-NodePool confinement | PASS | with a same-AZ spare in an out-of-scope pool, the surge claim carried `nodepool=nrc-poc`; the other pool's claim list was unchanged (no provision/absorb there) |
| G — PDB-respected voluntary drain | PASS | `minAvailable=2` → drain stalled (replicas pinned, old NodeClaim lingering); relaxing to `minAvailable=1` → replicas migrated one at a time, old NodeClaim deleted, rotation completed |
| H — `do-not-disrupt` on both nodes | PASS | during the surge both the candidate node and surge host carried `karpenter.sh/do-not-disrupt=true` + `noderotation.io/do-not-disrupt-owned` + `surge-for`; all cleared on completion; Warning Events `AVeryAggressive` + `ThroughputBelowArrival` emitted |
| I — scaled R6 soak | PASS | consecutive rotations (`last-rotation-at` advanced each cycle), `failure`/`expired` stayed 0, `short_lead_nodes=0` throughout; each cycle fired at ~5–6m, far inside the 336h backstop |
| J — capacity-absorb (bin-packed) | PASS | placeholder bound Running on the **pre-existing spare** (no `FailedScheduling`); claim count held at **2** (never 3 = no new provision); candidate pod re-landed on the spare's headroom; pool collapsed to one node |
| K — leader-change resume | PASS | killed the leader mid-rotation (candidate `pending`, `surge-claim` stamped); a new replica took the Lease and resumed the **same** rotation purely from annotations (`surge-claim` unchanged) to completion |
| L — window boundary | PASS | closed the window live while rotation 1 was in-flight → rotation 1 completed past the boundary, rotation 2 never started; `window_active=0`, `candidates=1`, `in_progress=0` |
| M-A — placeholder preemption victim / Never | PASS | victim role: placeholders show `Normal Preempted` events when higher-priority pods reclaim the node; never-preemptor role: placeholders log `FailedScheduling … preemption: not eligible due to preemptionPolicy=Never` (the synthetic single-run preempt races the fast absorb bind→complete, as the runbook notes — both behaviors are evidenced organically and pinned by envtest) |
| M-B — `readyTimeout`-bounded rollback | PASS | placeholder kept Pending (candidate node cordoned, blocker node full, new node blocked by `limits`, cannot preempt the blocker) until `readyTimeout` fired a clean rollback: candidate retained `state=failed`, `retry-count=1`, node un-cordoned, no induced surge claim |
| N — `do-not-disrupt` honored vs Drift | PASS | node went `Drifted=True` but was **not** replaced while `do-not-disrupt=true` was set; removing the annotation triggered immediate make-before-break drift replacement |

**Findings (not GA blockers)**

- **[#141](https://github.com/AkashiSN/node-rotation-controller/issues/141)** —
  taking a NodePool out of RotationPolicy governance *mid-rotation* (removing its
  selector label, or deleting/repointing the policy) orphans the in-flight
  rotation's placeholder Pod and leaves the `do-not-disrupt` marker behind: the
  controller stops reconciling the now-ungoverned pool and never advances or
  cleans up the rotation. Surfaced as a test-methodology artifact; hand-cleaned.
  Runbook lesson: in cleanups, **drop the in-scope label first**, then restore
  other knobs.
- Metrics methodology: the `noderotation_*` per-NodePool series (including
  `completed_total`) are dropped when a pool leaves governance and recreated
  fresh on re-entry, so they reset per in-scope period. Read metrics **while the
  pool is in scope**, right after the rotation, before dropping the label.

**Still open** (carried over, unchanged from spec §7.2): a genuine same-AZ
capacity-shortage (ICE) rollback (not deterministically inducible — stood in for
by a short `readyTimeout`), and a full multi-hour *tight-race* `expireAfter` soak
(not achievable under a daily window — see issue
[#118](https://github.com/AkashiSN/node-rotation-controller/issues/118)).
