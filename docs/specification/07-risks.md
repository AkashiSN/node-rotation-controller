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
**Validated (20+ scenarios):** core surge, same-AZ zonal-PV rebind, rollback, limits gating, multi-pool confinement, PDB drain, do-not-disrupt markers, force-expiry detection, capacity-absorb, placeholder preemption, window-boundary, leader-change resume, forceful fallback, earliest-deadline ordering, operator opt-out, and a 12-hour tight-race soak.

**Open:** genuine same-AZ capacity shortage (ICE) driving rollback on real cloud (issue #109).
:::

### Core mechanism

| Assumption | Status | Date |
|------------|--------|------|
| Standalone `NodeClaim` is provisionable on Auto Mode | Validated | 2026-05-29 |
| Placeholder-Pod surge completes make-before-break | Validated | 2026-06-22 |
| Same-AZ surge lets EBS re-attach (zonal-PV rebind) | Validated | 2026-06-22 |
| `readyTimeout` miss rolls back cleanly | Validated | 2026-06-22 |
| NodePool `limits` exhaustion gates surge | Validated | 2026-06-22 |
| Required `karpenter.sh/nodepool` confines surge to pool | Validated | 2026-06-22 |
| Explicit `NodeClaim` deletion drains via voluntary path (PDBs) | Validated | 2026-06-22 |
| `do-not-disrupt` applied to both nodes, removed on completion | Validated | 2026-06-22 |
| Force-expiry mid-pending records `expired` (not success/failure) | Validated | 2026-06-22 |

### Advanced scenarios

| Assumption | Status | Date |
|------------|--------|------|
| Capacity-absorb path (bin-pack onto spare, no new node) | Validated | 2026-06-23 |
| Leader-change resumes purely from annotations | Validated | 2026-06-23 |
| In-flight rotation completes past window boundary | Validated | 2026-06-23 |
| Placeholder is preemption victim; hostile preemption → rollback | Validated | 2026-06-23 |
| `do-not-disrupt` honored against Drift | Validated | 2026-06-23 |

### Post-v0.4 additions

| Assumption | Status | Date |
|------------|--------|------|
| Forceful fallback (12-node batch, graceful + surge-less mix) | Validated | 2026-07-04 |
| Earliest-deadline candidate ordering | Validated | 2026-07-04 |
| Operator `do-not-disrupt` excludes from selection | Validated | 2026-07-04 |

### Soak tests

| Assumption | Status | Date |
|------------|--------|------|
| 12h tight-race soak: 71/71 graceful, 0 expired, 0 failure | Validated | 2026-07-15 |
| Forceful fallback fires deterministically for bounded claim | Validated | 2026-07-15 |

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
*CRD-based policy migration* and *per-NodePool maintenance window* — both delivered by the `RotationPolicy` CRD (issue #119, §5.4).
:::
