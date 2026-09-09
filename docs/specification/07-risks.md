# 7. Risks & Status

## 7.1 Risks

| # | Risk | Mitigation |
|---|------|------------|
| R1 | Controller crash / leader loss | `replicas=2` + leader election; backstop retained; failure alerts |
| R2 | Window too short for all candidates | §3.2 throughput check warns; alert on sustained candidates |
| R3 | Surge capacity unavailable (AZ shortage / limits) | Pre-check headroom (§5.2); `readyTimeout` rollback; multi-AZ / multi-instance-type |
| R4 | Drain blocks on misconfigured PDB | `terminationGracePeriod` forces drain; PDB review is app owner's responsibility |
| R5 | Forgotten freeze during critical period | Manage declaratively (GitOps) rather than ad-hoc |
| R6 | Test clusters routinely turn over | Disable shutdown for a soak exceeding `ageThreshold` |

- **R3 zonal caveat:** surge is pinned to the candidate's AZ (§3.7). A same-AZ shortage cannot fall back to another zone — keep per-AZ headroom for zonal-PV NodePools.

## 7.2 Validated Assumptions

::: tip Validation summary
**Validated (20+ scenarios):** core surge, same-AZ zonal-PV rebind, rollback, limits gating, multi-pool confinement, PDB drain, do-not-disrupt markers, force-expiry detection, capacity-absorb, whole-node surge reservation, placeholder preemption, window-boundary, leader-change resume, forceful fallback, earliest-deadline ordering, operator opt-out, and a 12-hour tight-race soak.

**Open:** genuine same-AZ capacity shortage (ICE) driving rollback on real cloud (issue #109).

Two of these are published as full reports: [Forceful fallback (Scenario O)](../validation/forceful-fallback) and [Tight-race soak (Scenario P)](../validation/tight-race-soak). The evidence below summarizes them; the reports carry the method and the raw record.
:::

### Core mechanism

| Assumption | Status |
|------------|--------|
| Standalone `NodeClaim` is provisionable on Auto Mode | Validated |
| Placeholder-Pod surge completes make-before-break | Validated |
| Same-AZ surge lets EBS re-attach (zonal-PV rebind) | Validated |
| `readyTimeout` miss rolls back cleanly | Validated |
| NodePool `limits` exhaustion gates surge | Validated |
| Required `karpenter.sh/nodepool` confines surge to pool | Validated |
| Explicit `NodeClaim` deletion drains via voluntary path (PDBs) | Validated |
| `do-not-disrupt` applied to both nodes, removed on completion | Validated |
| Force-expiry mid-pending records `expired` (not success/failure) | Validated |

### Advanced scenarios

| Assumption | Status |
|------------|--------|
| Capacity-absorb path (bin-pack onto spare, no new node) | Validated |
| `surge.wholeNodeReservation` turns absorb into provisioned under host-level anti-affinity | Validated |
| Leader-change resumes purely from annotations | Validated |
| In-flight rotation completes past window boundary | Validated |
| Placeholder is preemption victim; hostile preemption → rollback | Validated |
| `do-not-disrupt` honored against Drift | Validated |

### Forceful fallback and selection

| Assumption | Status |
|------------|--------|
| Forceful fallback (12-node batch, graceful + surge-less mix) | Validated |
| Earliest-deadline candidate ordering | Validated |
| Operator `do-not-disrupt` excludes from selection | Validated |

### Soak tests

| Assumption | Status |
|------------|--------|
| 12h tight-race soak: 71/71 graceful, 0 expired, 0 failure | Validated |
| Forceful fallback fires deterministically for bounded claim | Validated |

::: details Full validation evidence — click to expand

#### Standalone NodeClaim (capability, not the surge mechanism)

A NodeClaim with only `nodeClassRef` + `requirements` reached `Ready` (~30s, real EC2); admission accepted `--dry-run=server`; graceful finalizer-driven deletion confirmed. K8s 1.35, `karpenter.sh/v1` (2026-05-29).

#### Placeholder-Pod surge (2026-06-22)

A low-priority placeholder induced a NodePool-owned surge `NodeClaim` that reached `Ready` (~30s) before the old was deleted; drain followed the voluntary path; workload rescheduled onto surge node; `noderotation_completed_total{outcome="success"}` incremented.

#### Same-AZ zonal-PV rebind (2026-06-22)

StatefulSet gp3 PVC in `us-west-2a`; `matchNodeRequirements` kept every surge node in `us-west-2a`; the same PV re-attached (not reprovisioned); sentinel data survived.

#### Timeout rollback (2026-06-22)

`readyTimeout` set below node-ready time → timeout → surge claim reaped, placeholder deleted, candidate retained + uncordoned, `outcome="failure"` incremented.

#### Limits gating (2026-06-22)

`spec.limits.cpu` with no headroom: eligible candidate not surged; logged `insufficient limits headroom; cannot surge`; `in_progress` stayed 0.

#### Multi-pool confinement (2026-06-22)

Second pool with same-AZ spare: surge stayed in the candidate's pool (`karpenter.sh/nodepool=nrc-poc`); other pool unchanged.

#### PDB-respecting drain (2026-06-22)

Blocking PDB (`minAvailable=2`, 2 replicas) stalled drain; relaxing to `minAvailable=1` let migration complete one at a time.

#### `do-not-disrupt` markers (2026-06-22)

Both old and surge nodes carried `do-not-disrupt=true` + `do-not-disrupt-owned`; unfreeze removed both on completion.

#### Force-expiry detection (2026-06-22)

Froze pool + deleted pending candidate → `state=expired`, anchor cleared, no surge left behind, `outcome="expired"` incremented.

#### Capacity-absorb path (2026-06-23)

Young same-AZ spare with ~1970m free absorbed the 250m placeholder (no new NodeClaim induced); pool stayed at 2 claims throughout; `outcome="success"` incremented.

#### Leader-change resume (2026-06-23)

Mid-rotation leader killed; new replica took Lease and continued the same rotation from annotations — same `surge-claim`, same `started-at`, completed without restart.

#### Window-boundary behavior (2026-06-23)

Rotation started in-window; closing window did not abort it; second eligible candidate did not start (`window_active=0`).

#### Placeholder preemption (2026-06-23)

Higher-priority Pod preempted placeholder; placeholder never preempts (`preemptionPolicy=Never`); with limits blocking re-provision, stayed Pending until `readyTimeout` → clean rollback.

#### `do-not-disrupt` vs Drift (2026-06-23)

Drifted node with `do-not-disrupt=true` was not replaced for >3 min; removing annotation triggered immediate drift-replace.

#### Forceful fallback — 12-node batch (2026-07-04)

12 nodes, fixed 2h `expireAfter`, `N=12 > K·C=2`: first 6 gracefully, surplus 6 surge-less. `rotation-mode=forceful-fallback` while in flight; `ForcefulFallback` Warning Events; no placeholder for forceful candidates; `noderotation_forceful_fallback_total` climbed `0→6`; PDB held throughout; zero `expired`.

#### Earliest-deadline ordering (2026-07-04)

12-node batch shared one `creationTimestamp` → ordering degraded to Name tiebreak: claims consumed exactly ascending (`2rvd5 < 6ssql < dtkgz < ...`).

#### Operator `do-not-disrupt` exclusion (2026-07-04)

Annotating a candidate's Node `do-not-disrupt=true` (no owned marker) dropped `candidates` gauge 4→3; removing restored it.

#### 12h tight-race soak (2026-07-15)

`E=2h12m`, `leadTime=1h12m`, 48 windows/day, 5-node pool. 71/71 rotations gracefully (~12m cadence). Min margin 68.3m, median 70.3m, max 71.2m. Zero `expired`, zero `failure`, `forceful_fallback_total=0` (armed but never needed). Controller `restartCount=0`, 909 scrapes, contiguous `seq`. Full record: `test/e2e/eks-automode/VALIDATION.md`.

#### Forceful fallback boundary (2026-07-15)

Separate single-node pool, released after aging past candidacy. Graceful surge no longer fit → surge-less branch: `forceful_fallback_total` 0→1; claim deleted 56s after release (10m04s ahead of deadline); no placeholder at any point; `expired` stayed 0.

#### Whole-node surge reservation (2026-09-09)

Two windows on the 4-node `m5.xlarge` Auto Mode pool that produced the capacity-absorb result above, whose two CPU-heavy services are separated by `podAntiAffinity` on `kubernetes.io/hostname`. Default sizing: 8/8 `absorbed` (`surge_wait` mean 2.31s), and every one of the 8 made Karpenter provision a node 16–17s **after** the drain began. `wholeNodeReservation.enabled: true`: 8/8 `provisioned` (`surge_wait` mean 31.35s), surge node Ready 6–20s **before** the drain, no follow-up node. The placeholder reserved `cpu=3420m,memory=13811392Ki` against a `drain` footprint of `cpu=1400m,memory=1792Mi` — the candidate's **NodeClaim** allocatable (`3770m/15092824Ki`) less its four DaemonSet Pods. Instance count was identical either way (4 → 5 → 4): the mode changed *when* the extra node is bought, not how many. The measured cost is the `surge_wait`, ~+29s per rotation; drain was unchanged (96.5s → 103.1s, overlapping ranges). No `InsufficientHeadroom` Events and no headroom block in the log across the 3h — the `surge_headroom` gate never bound on this pool.

`absorbed` reaching zero is a property of this pool — one instance type, the same workload mix on every node, no genuinely empty hosts — not evidence for a claim §3.3 does not make. The control is on the time axis: the same pool ran both windows the same day, with the toggle patched onto the live policy between them. What rules out drift is the scheduler-side mechanism — in the second window the placeholder was rejected by every existing node (`Insufficient cpu`/`Insufficient memory`, with `preemptionPolicy=Never` barring preemption) before being nominated onto the new NodeClaim.

:::

### Open item

A genuine same-AZ **capacity shortage (ICE)** driving rollback — stood in for by a short `readyTimeout` (not deterministically inducible on demand). Tracked in issue #109.

### Notes

- The standalone `NodeClaim` result de-risks the project but is **not** the surge mechanism (§3.3 — standalone nodes break NodePool accounting)
- RBAC sufficiency and `karpenter.sh/v1` CRD decode are implicitly exercised by all scenarios

## 7.3 Open Questions

1. **Holiday-aware scheduling** — skip rotation if a window day falls on a holiday. v1 intentionally ignores holidays.
2. **Pre-pull image source** — whether to use Karpenter NodeClass image-pulling or a dedicated Job (v2).
3. **Multi-cloud verification** — AKS NAP, GKE testing before claiming compatibility beyond EKS Auto Mode.

::: tip Resolved
*CRD-based policy migration* and *per-NodePool maintenance window* — both delivered by the `RotationPolicy` CRD (§5.4).
:::
