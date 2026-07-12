# Recorded validation runs

This is the **recorded outcome** of running the [`SCENARIOS.md`](SCENARIOS.md)
runbook against a real EKS Auto Mode cluster. `SCENARIOS.md` is the *how* (the
reproducible steps, expected output, and gotchas); this file is the *what
happened* — a dated log of actual results, so a reader can see that the spec
[§7.2](../../../docs/specification/07-risks.md#72-validated-assumptions) assumptions were
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

---

## Run: 2026-07-04/05 — `test/eks-ff-validation` @ `0c35c91` (forceful fallback / #157 / #170)

**Artifacts under test**

| | |
|---|---|
| Controller image | branch `test/eks-ff-validation` @ `0c35c91`, built and pushed to the ephemeral stack's ECR as `:poc` (linux/amd64) |
| Helm chart | `charts/node-rotation-controller` (this branch), `-f scenarios/controller-values.yaml` |
| Platform | EKS Auto Mode, Kubernetes 1.36, `karpenter.sh/v1`, `us-west-2` |
| NodePool | `nodepool-ff`, **fixed `expireAfter: 2h`** (never patched — trick-free invariant), disruption budgets block Underutilized/Drifted |
| Policy | `RotationPolicy` `nrc-ff` (`ageThreshold: auto`, `forcefulFallback.enabled: true`, 30-min-period windows `P=30m`/`WindowLen=28m` → `C=1`, `readyTimeout: 5m`, `maxUnavailable: 1`, `retryBackoff: 30m`; `cooldownAfter` tuned live — 10m up-front to hold the surplus to the deadline, then 2m at the graceful→forceful boundary to fire it ahead of the backstop) |
| Workload | 12-replica PDB-backed Deployment (`minAvailable: 11`, hostname anti-affinity) → one pod per node, a **synchronized 12-node batch** sharing one deadline |

**Verdict: forceful fallback (#156), earliest-deadline ordering (#157), and
do-not-disrupt exclusion (#170) all PASS on real EKS.** The 12-node batch (all
created `00:46:33Z`, so one shared deadline) rotated as a **graceful + forceful
mix in one pool**: the first 6 gracefully (make-before-break surge), the surplus
6 surge-less once inside `t_rot` of the shared 2h deadline — with **zero**
`expireAfter` backstop, and `noderotation_forceful_fallback_total` climbing
cleanly **0→6** across **zero controller restarts**. `expireAfter` stayed **2h
fixed** the whole run; forceful was induced purely by the `N=12 > K·C=2`
synchronized-batch throughput shortfall (plus live `cooldownAfter` tuning to slow
the serial graceful cadence), **never** by patching a deadline — the trick-free
property the KWOK e2e cannot exercise.

Firing math held to the second: `age_threshold_seconds=2100` (A=35m), first
forceful fire at `02:22:19Z` = batch age **1h35m46s** (just past `E − t_rot =
1h35m`), grace band `K·P = 1h`.

| Check | Verdict | Evidence observed |
|-------|---------|-------------------|
| Surge-less forceful fallback fires in-window (#156, spec §3.3) | PASS | 6 NodeClaims (`nkfbh`, `pdfwl`, `s7l9r`, `vvsqr`, `w9kx7`, `wcmwr`) rotated with the NodePool anchor `noderotation.io/rotation-mode=forceful-fallback` while in flight; each raised a `Warning`/`ForcefulFallback` Event *"rotating NodeClaim … surge-less: a graceful surge cannot complete before its deadline; deleting in-window via the voluntary path (PDBs apply)"*; `noderotation_forceful_fallback_total{nodepool="nodepool-ff"}` climbed **`0→6`** (one per surge-less rotation) with the controller pod at `restartCount=0` throughout — a clean, monotonic counter |
| No placeholder for a forceful candidate (surge-less) | PASS | for every forceful candidate `kubectl get pod noderotation-surge-<candidate>` → `NotFound`; the `surge_wait` duration histogram counted **6** (only the 6 graceful rotations), never 12; `completed_total{outcome="success"}` reached **12** (6 graceful + 6 forceful) |
| Graceful + forceful **mix** in one pool | PASS | first 6 (`2rvd5`, `6ssql`, `dtkgz`, `fswsg`, `gxsfs`, `krcdc`) rotated gracefully — placeholder Pod `noderotation-surge-<candidate>` staged (`Pending`→`Running`), `mode` empty (default surge) — before the surplus went surge-less; **0** `expired` backstop outcomes (all 12 rotated in-window) |
| Voluntary path / PDB respected | PASS | forceful deletes routed through Karpenter's termination controller; `PodDisruptionBudget minAvailable: 11` held throughout (serial `maxUnavailable: 1`) |
| Earliest-deadline ordering (#157) | PASS | all 12 consumed in exact ascending `(creationTimestamp, name)`; the shared `00:46:33Z` timestamp degrades the order to the `Name` tiebreak, observed exactly: `2rvd5 < 6ssql < dtkgz < fswsg < gxsfs < krcdc < nkfbh < pdfwl < s7l9r < vvsqr < w9kx7 < wcmwr` |
| do-not-disrupt exclusion (#170, spec §3.2) | PASS | annotating a **not-in-flight** candidate's Node `karpenter.sh/do-not-disrupt=true` dropped `noderotation_candidates{nodepool="nodepool-ff"}` **4→3** and the excluded NodeClaim was never chosen (no `deletionTimestamp`); **removing** the annotation let the count climb back — causation confirmed both directions (a reconcile nudge is needed because a frozen idle pool reconciles slowly) |
| Intentional schedule feasibility warns | PASS | the pool logged `ThroughputBurstShortfall` (`N=12 exceeds K·C=2`) and `ThroughputBelowArrival` (`N=12, P=30m, A=35m`) each pass — the designed predictors of the surge-less path, not errors |

**Findings (not behavior blockers)**

- **Scenario O harness gap — controller placement (found, fixed, and re-confirmed
  in this branch).** An initial pass exposed this: the shared
  `scenarios/controller-values.yaml` off-pool affinity used a *blocklist* on the
  nrc-poc label (`noderotation-poc/pool NotIn [poc]`). `nodepool-ff` nodes do not
  carry that label, so the constraint passed for them — when the controller's
  `general-purpose` host was consolidated by Auto Mode, the controller Pod
  rescheduled **onto a `nodepool-ff` node** and was later evicted by a rotation of
  that node, restarting the controller mid-run and **resetting its in-memory
  metric counters** (`completed_total` observed `7→2`). This is a test-harness
  observability artifact, not a controller defect — but it masks the clean
  counter. **Fix:** `controller-values.yaml` now uses a *positive allowlist* onto
  the Auto Mode built-in pools (`karpenter.sh/nodepool In [general-purpose,
  system]`), which no scenario rotates; the controller's node was additionally
  pinned with `karpenter.sh/do-not-disrupt` for the run. **The recorded run above
  is post-fix:** the controller held `restartCount=0` end-to-end and
  `forceful_fallback_total` climbed cleanly `0→6`. Runbook lesson: keep the
  controller strictly on the built-in pools, and anchor durable claims from
  Events (which survive any restart) as well as the counter.
- **Live cadence tuning is expected, not a trick.** Real graceful rotations
  complete in ~1–2 min (far under the worst-case `t_rot` budget), so with the
  default `cooldownAfter` the serial surge clears the batch before any candidate
  reaches its deadline and no forceful fires. Raising `cooldownAfter` to 10m held
  ~6 candidates un-rotated until the shared deadline (forceful), then lowering it
  back to 2m fired the surplus quickly ahead of the 2h backstop. `expireAfter`
  never changed — only a policy cadence knob (`SCENARIOS.md` §Scenario O
  documents this).

---

## Run: 2026-07-12 — `test/230-scenario-o-buffer2m` @ `30febf7` (Scenario O re-validation under Buffer=2m, #230)

**Artifacts under test**

| | |
|---|---|
| Controller image | branch `test/230-scenario-o-buffer2m` @ `30febf7` (current `main`, `schedule.Buffer = 2m`), built and pushed to the ephemeral stack's ECR as `:poc` (linux/amd64+arm64) |
| Helm chart | `charts/node-rotation-controller` (this branch), `-f scenarios/controller-values.yaml` |
| Platform | EKS Auto Mode, Kubernetes 1.36, `karpenter.sh/v1`, `us-west-2` |
| NodePool | `nodepool-ff`, **fixed `expireAfter: 2h`** (never patched — trick-free invariant), `terminationGracePeriod: 5m`, disruption budgets block Underutilized/Drifted |
| Policy | `RotationPolicy` `nrc-ff` (`ageThreshold: auto`, `forcefulFallback.enabled: true`, 30-min-period windows `P=30m`/`WindowLen=28m`, `readyTimeout: 5m`, `maxUnavailable: 1`, `retryBackoff: 30m`; `cooldownAfter` tuned live — 6m up-front to hold the surplus to the deadline, auto-dropped to 1m at 10:24 to fire it ahead of the backstop) |
| Workload | 12-replica PDB-backed Deployment (`minAvailable: 11`, hostname anti-affinity) → one pod per node, a **synchronized 12-node batch** (all `creationTimestamp 08:38:20Z`) sharing one 2h deadline (`10:38:20Z`) |

**Verdict: Scenario O re-validated under `Buffer=2m` (#215). The derived firing
math moved exactly as the constant predicts, confirmed both by the live schedule
metrics and by the observed graceful→forceful split.** This run supersedes the
derived figures of the `Buffer=15m` run above (kept as a dated record); the
behavior — trick-free forceful fallback, earliest-deadline order, no backstop — is
unchanged.

Live schedule metrics (read while in-scope, default `cooldownAfter=2m`) matched the
recomputed constants to the second: `age_threshold_seconds=2880` (**A=48m**, was
35m), `t_rot_bound_seconds=720` (**t_rot=12m**, was 25m),
`t_rot_estimate_seconds=600` (**t_rot_est=10m**), `throughput_capacity=3`
(**C=3**), `rotation_chances=2`, `window_period_seconds=1800`. **`C` did not move
with the Buffer** — the layer-2 forecast is budgeted on `t_rot_est`
(`provisioningEstimate 5m + drainEstimate 5m`), which carries no buffer (ADR-0003,
#220); only the deadline-side `t_rot`, `A`, and the forceful-fire age `E − t_rot`
shrank/grew with `Buffer 15m → 2m`.

The 12-node batch rotated as a **9 graceful + 3 forceful mix**, and the
forceful-fire boundary `E − t_rot = 1h48m` was captured to the second: node **9
(`rnwb7`) rotated gracefully at age 1h46m1s** (just inside the boundary), node **10
(`sgh57`) went surge-less at age 1h48m35s** (just past it), and every remaining
node followed. `noderotation_forceful_fallback_total` climbed **0→3** with **zero**
`expired` backstop outcomes (all 12 rotated in-window, the last forceful completing
`10:33:37`, ~5m before the `10:38:20` deadline) across **zero** controller
restarts. `expireAfter` stayed **2h fixed** the whole run.

| Check | Verdict | Evidence observed |
|-------|---------|-------------------|
| Derived firing math under `Buffer=2m` (#215/#230) | PASS | live metrics `t_rot_bound=720` (12m), `age_threshold=2880` (48m), `t_rot_estimate=600` (10m), `throughput_capacity=3`; schedule logs echoed `A=48m0s`, `C=3`, `K·C=6` and the `t_rot_est 10m + cooldown 2m = 12m` denominator |
| Surge-less forceful fallback fires at the new boundary (#156, spec §3.3) | PASS | first surge-less pick `sgh57` at batch age **1h48m35s** (just past `E − t_rot = 1h48m`); `t7sb5` (1h52m39s) and `tkkf7` (1h54m56s) followed; each raised a `Warning`/`ForcefulFallback` Event *"rotating NodeClaim … surge-less: a graceful surge cannot complete before its deadline; deleting in-window via the voluntary path (PDBs apply)"*; `noderotation_forceful_fallback_total{nodepool="nodepool-ff"}` **0→3**, controller `restartCount=0` |
| Boundary discrimination (graceful vs forceful on remaining-time) | PASS | node 9 `rnwb7` picked at age **1h46m1s** with ≥ `t_rot` to the deadline → **graceful** (placeholder + surge node `i-03ae34219f5057027`); node 10 `sgh57` picked 2m34s later at age **1h48m35s** with < `t_rot` remaining → **forceful** — the split falls exactly on the `1h48m` boundary |
| No placeholder for a forceful candidate (surge-less) | PASS | `kubectl get pod noderotation-surge-nodepool-ff-sgh57` → `NotFound`; the 9 graceful rotations each staged a placeholder + surge node (`surgeWait` 23–35s, `drain` 22s–1m17s, `total` 46s–1m51s), the 3 forceful none |
| Graceful + forceful **mix** in one pool | PASS | nodes 1–9 (`52t8h … rnwb7`) rotated gracefully (make-before-break: NodeClaim count `12→13→12` each cycle, surge node Ready before old drained), nodes 10–12 surge-less; **0** `expired`, **0** `failure`, **0** rolled back |
| Earliest-deadline ordering (#157) | PASS | the 12 shared `creationTimestamp 08:38:20Z`, so the order degrades to the `Name` tiebreak, observed exactly: `52t8h < 5rp47 < 94f5n < fvd7p < gjgg8 < n8zqt < p2vdk < phmms < rnwb7 < sgh57 < t7sb5 < tkkf7`; the earliest-deadline batch was fully consumed before any younger surge-replacement node (later deadline) was touched |
| Intentional schedule feasibility warns | PASS | the pool logged `ThroughputBurstShortfall` (`N=12 > K·C=6`), `ThroughputBelowArrival` (`N=12, P=30m, A=48m, C=3`), and `RotationSpansNextWindow` (`t_rot_est 10m + cooldown 2m = 12m` > the 2m the window stays closed) each pass — the designed predictors of the surge-less path, not errors |
| Backstop stays unreached (`expireAfter` retained) | PASS | `noderotation_short_lead_nodes=0` and `noderotation_drain_stuck=0` throughout; no `outcome="expired"` series; the narrower `Buffer=2m` forceful window (`E − t_rot` to `E` = 12m) still cleared all 3 surplus with ~5m to spare |

**Findings (not behavior blockers)**

- **The `9 graceful + 3 forceful` split differs from the `Buffer=15m` run's `6 + 6`,
  and it is the Buffer change that moved it.** `A` grew `35m → 48m` (eligibility
  later) and the forceful boundary grew `1h35m → 1h48m`, so the 1h graceful grace
  band sits later; against the fast real cadence (graceful rotations complete in
  46s–1m51s here) the serial surge cleared 9 of the batch before the boundary,
  leaving a 3-node surplus. The mix shape is cadence-dependent (and was shaped by
  the live `cooldownAfter` tuning, as in the prior run); the load-bearing,
  Buffer-driven fact is the **boundary position** (`1h48m`), which held to the
  second.
- **`#170` do-not-disrupt exclusion was not re-exercised this run.** It is
  orthogonal to the `Buffer` constant (it gates candidate *eligibility*, not the
  firing math) and was validated on real EKS in the `2026-07-04/05` run above;
  its behavior is unchanged here.
- **Run overran the batch deadline.** The cluster was left rotating past
  `10:38:20Z` (the ff-workload persists, so surge-replacement nodes aged past
  `A=48m` and rotated again — 45 total graceful completions by teardown, all
  graceful, no further forceful since replacements are not deadline-synchronized).
  Only the original 12-node batch is recorded above; the trailing churn is expected
  steady-state operation, not part of the Scenario O assertion.
