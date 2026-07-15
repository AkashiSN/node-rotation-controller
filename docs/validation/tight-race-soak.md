# Tight-race `expireAfter` soak — Scenario P

A 12-hour real-EKS soak (issue #118, PR #272) in which the controller's
`leadTime` genuinely races Karpenter's `expireAfter` backstop: the pool ran
with a fixed `expireAfter: 2h12m` — just above the derived `leadTime` of
1h12m — under sub-daily maintenance windows (48 × 30 min per day). The run
demonstrates the [specification §3.2](/specification/03-design#32-candidate-selection)
guarantee live (the graceful surge always finishes first, `expired` stays 0)
and, on a separate epilogue pool, that the window-bounded forceful fallback
fires deterministically the moment a graceful surge no longer fits. See
[§7.2](/specification/07-risks#72-validated-assumptions) for the assumptions
this run flips to validated, the
[runbook](/runbook#3-interpreting-the-noderotation_-metrics) for what the
metrics quoted below mean operationally, and
[Scenario O](/validation/forceful-fallback) for the earlier forceful-fallback
validation this run extends.

**Run**: 2026-07-14T14:20:29Z (T0) → +12h, EKS Auto Mode, Kubernetes 1.36,
`us-west-2`. The canonical record is `test/e2e/eks-automode/VALIDATION.md`
(§ "Run: 2026-07-15 — Scenario P") in the repository.

**Verdict: CLEAN PASS on all applicable criteria** (12 PASS; criterion 13, the
missed-release abort rule, was never exercised and is N/A).

| 71<small>/71</small> | 0 | 68.3<small> min</small> | 56<small> s</small> |
|:---|:---|:---|:---|
| graceful surge rotations over the 12h main run (5.0/h) | `expired` · `failure` · main-pool fallback · `short_lead` · restarts | minimum deadline margin (median 70.3, max 71.2) | epilogue release → surge-less fallback completed |

## Aim and derived schedule

Earlier PoCs used daily windows, which force a huge `expireAfter` (E=336h) —
the race against Karpenter's forceful expiration was always won by a wide
margin. This run puts E at **2h12m, immediately above `leadTime` = 1h12m**,
and holds that genuinely tight race for 12 hours. `surge.forcefulFallback`
stays **armed the whole time**, so the run also demonstrates quiescence: the
fallback is available but never needed while the graceful path keeps winning.

| Quantity | Derivation |
|---|---|
| `t_rot` (bound) | `readyTimeout 5m + tGP 5m + Buffer 2m = 12m` |
| `t_rot_est` (forecast) | `min(readyTimeout, 5m) + min(tGP, 10m) = 5m + 5m = 10m` |
| `leadTime` | `K·P + t_rot = 2·30m + 12m = 1h12m` |
| `ageThreshold` (auto) | `A = E − leadTime = 2h12m − 1h12m = 1h` |
| `C` (per-window capacity) | `ceil(D / (t_rot_est + cooldownAfter)) = ceil(28m / 12m) = 3` (6/h) |
| Steady load | N=5, arrival rate N/A = 5/h → **83%** of forecast capacity (deliberately tight) |
| Expected findings | **exactly one** warn, `RotationSpansNextWindow` — structurally unavoidable (window gap 2m < `t_rot_est` + cooldown 12m). `ThroughputBelowArrival` / `ThroughputBurstShortfall` must not appear |

The derivation is pinned by `TestDeriveScenarioPSoak` in
[`internal/schedule/schedule_test.go`](https://github.com/AkashiSN/node-rotation-controller/blob/main/internal/schedule/schedule_test.go)
— that test passing is the proof that a re-run's configuration matches this
report.

## The margin picture

Over 12 hours from T0 the main pool completed **71 rotations, every one a
graceful make-before-break surge onto a newly provisioned node**, landing
roughly every 12 minutes with five slots keeping phase. The margin — the old
claim's deadline minus the rotation's completion — stayed within 68.3–71.2
minutes across all 71, a spread under 3 minutes: **no degradation accumulated
over the 12-hour run**.

<SoakMarginChart />

## The 13 criteria

| # | Criterion | Verdict | Observed |
|---|---|---|---|
| 1 | `outcome="expired"` == 0 | PASS | 0 for the full run + tail-follow + epilogue; no Karpenter `Expiration` events |
| 2 | `success` climbs ≈5/h, total ≥ 40 | PASS | 71 (72 incl. the epilogue), steady ~12m cadence |
| 3 | main-pool `forceful_fallback_total` == 0 (armed) | PASS | 0 the entire run — first demonstration of quiescence |
| 4 | `short_lead_nodes` == 0 at every scrape | PASS | max 0 across all 909 scrapes |
| 5 | restarts 0; scraper `seq` contiguous | PASS | controller `restartCount=0` for 12h; 0 gaps, 0 restarts, 0 `SCRAPE_ERROR` |
| 6 | load present at every snapshot | PASS | desired=available=ready=5 throughout; no Pending backlog |
| 7 | config-under-test present throughout | PASS | six derived gauges exact at 909/909 scrapes; both policies `Accepted`; findings exactly the one expected warn |
| 8 | per-rotation margin > 0 | PASS | min 68.3 / median 70.3 / max 71.2 min (n=71) |
| 9 | end census | PASS | at T_end all 5 live claims younger than A (11–59m, right-censored); 0 stale, 0 `failed` |
| 10 | no unexpected Karpenter disruption | PASS | only `DisruptionBlocked`/`Unconsolidatable` events (budgets + `do-not-disrupt` suppressing, as designed) |
| 11 | epilogue: frozen pool stays candidate-only | PASS | `candidates=1`, `in_progress=0` held for ~2h while frozen |
| 12 | epilogue: release fires the fallback deterministically | PASS | see the epilogue section below (56 s, zero placeholder, main pool undisturbed) |
| 13 | abort rule (missed release) | N/A | the release completed inside its bounds; the fail-closed path was exercised offline only |

## Anatomy of a rotation

Each of the 71 rotations followed the same observed sequence:

1. The claim reaches age **1h** (= A) and becomes a candidate → the controller
   creates the low-priority **placeholder Pod** (`noderotation-surge-<claim>`,
   label `noderotation.io/surge-for`).
2. Karpenter cannot bin-pack the placeholder and **provisions a new node**
   (the surge claim is born). The placeholder binds → the surge node goes
   Ready (**surgeWait: median 34 s**, range 23–54 s).
3. Both nodes get `karpenter.sh/do-not-disrupt` → the old `NodeClaim` is
   deleted → Karpenter drains it via the voluntary path (**drain: median
   44 s**, range 18–83 s). The workload pod re-lands on the surge node.
4. Including cleanup (annotation removal, placeholder deletion), **total:
   median 81 s** (range 45–131 s). The emptied old node's remains are
   collected by `WhenEmpty`/60s.

<SoakAnatomyChart />

<SoakLedger />

## Epilogue — deterministic fallback firing

The main run's "never fires" is only half a validation of the fallback, so a
**frozen single-node pool** (`nodepool-soak-epi`) was used to drive one claim
across the boundary deliberately — and deterministically. The pool size is 1
because forceful rotations are serial per pool and one completion may legally
take up to the drain bound `tGP + Buffer = 7m`: with more nodes, completion
inside the 12-minute release band could not be *guaranteed* (multi-node mixed
evidence is [Scenario O](/validation/forceful-fallback)'s).

| | |
|---|---|
| 02:22:50Z | epi claim `gtx42` born (the pool is created with `noderotation.io/freeze` already set) → deadline **d = 04:34:50Z** |
| 03:22:50Z → | becomes a candidate at age 1h; frozen, so it sits at `candidates=1` / `in_progress=0` (freeze semantics hold for the full attended wait) |
| 04:23:50Z | `soak-epi-release.sh` removes the freeze at R, found by interval search (first in-window instant ≥ d−11m, proving d−12m < R < d−8m). 11 min left < `t_rot` 12 min → a graceful surge no longer fits |
| 04:24:46Z | **56 s after release**, the claim is deleted (measured drain 48 s < the 7 min bound). The log's `mode=forceful-fallback` with no `surgeNode` field is itself proof of the surge-less path |
| Evidence | `forceful_fallback_total{nodepool-soak-epi}` 0→1 · `ForcefulFallback` Warning event with the spec's message · the continuous placeholder ledger (`pods -w` on `noderotation.io/surge-for`) recorded **zero** placeholder for the epi claim · `expired` stayed 0 · the main pool rotated undisturbed through the epilogue (70→71, completion gaps ≤ `P + t_rot` = 42m) |

The abort rule for a missed release — if the freeze cannot be removed before
d−8m, tear the pool down **still frozen** (a late unfreeze cannot prevent the
expiry) — was not exercised in this run; the release script's fail-closed
behavior was verified offline against kubectl stubs (all three failure legs
exit 3).

## Reproducing this run

The runbook is
[`test/e2e/eks-automode/SCENARIOS.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/test/e2e/eks-automode/SCENARIOS.md)
§ Scenario P; all manifests and scripts are committed under
`test/e2e/eks-automode/scenarios/`. Before touching AWS:

```sh
go test ./internal/schedule/ -run TestDeriveScenarioPSoak -v  # pins A=1h / C=3 / G=2 / 1 warn
test/e2e/eks-automode/scenarios/soak-analyze-fixture.sh       # analyzer self-check
```

Operational notes from this run:

- **JSON logging is a prerequisite** (`logging.development: false`): the
  analyzer reads zap's JSON output; with console logging the ledger comes out
  empty.
- **Check credential lifetime up front.** The operator workstation's cloud
  credentials expired mid-run and blinded the local (secondary) recorder for
  15 minutes; the in-cluster scraper — the recorder of record — stayed
  gapless, which is exactly what the two-recorder design is for.
- **Never live-patch `expireAfter`** — recreate the pool instead (a patch
  induces Karpenter drift).
- Cost: ≈ $11 (control plane + NAT ~20h, five 2-vCPU nodes ×14h + epilogue).
  Do not forget `terraform destroy`.

## What this run settles

Spec §7.2 gains two validated rows: under sub-daily windows the derived
`leadTime` beats a genuinely racing `expireAfter` for 12 hours straight with
the fallback armed but quiescent; and the moment a claim crosses the point
where a graceful surge no longer fits, the window-bounded forceful fallback
fires deterministically. The one remaining open real-cloud item is a genuine
same-AZ capacity shortage (ICE) — see the
[roadmap (§6.2)](/specification/06-release#62-roadmap) and
[§7.2](/specification/07-risks#72-validated-assumptions).
