# Tight-race `expireAfter` Soak — Scenario P

::: tip What this validates
A 12-hour real-EKS soak where `leadTime` genuinely races `expireAfter`. Demonstrates: (1) the graceful surge always finishes first (`expired` stays 0), and (2) forceful fallback fires deterministically when a graceful surge no longer fits.
:::

The pool ran with fixed `expireAfter: 2h12m` — just above derived `leadTime` of 1h12m — under sub-daily windows (48 × 30 min/day). This validates [§3.2](/specification/03-design#32-candidate-selection) live. See [§7.2](/specification/07-risks#72-validated-assumptions) for the assumptions validated, [Scenario O](/validation/forceful-fallback) for the earlier forceful-fallback validation this extends, and the [runbook](/runbook#3-interpreting-the-noderotation_-metrics) for metric definitions.

**Run**: 2026-07-14T14:20:29Z (T0) → +12h, EKS Auto Mode, K8s 1.36, `us-west-2`.
Canonical record: `test/e2e/eks-automode/VALIDATION.md` (§ "Run: 2026-07-15 — Scenario P").

**Verdict: CLEAN PASS** (12 PASS; criterion 13 N/A — never exercised).

| 71/71 | 0 | 68.3 min | 56 s |
|:---|:---|:---|:---|
| Graceful rotations (60 in 12h = 5.0/h) | expired · failure · fallback · short_lead · restarts | Min deadline margin | Epilogue: release → fallback |

## Derived schedule

Earlier PoCs used daily windows with huge `expireAfter` (E=336h) — the race was always won by a wide margin. This run puts E at **2h12m** and holds a genuinely tight race for 12 hours. `surge.forcefulFallback` stays **armed** the whole time (demonstrating quiescence).

| Quantity | Value |
|---|---|
| `t_rot` (bound) | `5m + 5m + 2m = 12m` |
| `t_rot_est` (forecast) | `5m + 5m = 10m` |
| `leadTime` | `2·30m + 12m = 1h12m` |
| `ageThreshold` (auto) | `2h12m − 1h12m = 1h` |
| `C` (per-window) | `ceil(28m / 12m) = 3` (6/h) |
| Steady load | N=5, 5/h → 83% of forecast (tight) |
| Expected findings | Exactly 1 warn: `RotationSpansNextWindow` |

Derivation pinned by `TestDeriveScenarioPSoak` in [`internal/schedule/schedule_test.go`](https://github.com/AkashiSN/node-rotation-controller/blob/main/internal/schedule/schedule_test.go).

## Margin picture

From T0 to end of recording (T0+14.1h): **71 rotations, all graceful make-before-break surge onto a newly provisioned node**. 60 inside [T0, T_end] (= 5.0/h), 11 more during the epilogue at unchanged ~12-min cadence.

Margin (deadline − completion): **68.3–71.2 min** across all 71 (spread < 3 min). No degradation accumulated.

<SoakMarginChart />

## The 13 criteria

| # | Criterion | Verdict |
|---|---|---|
| 1 | `expired` == 0 | PASS |
| 2 | `success` ≈5/h, total ≥ 40 | PASS (60 in 12h) |
| 3 | Main-pool `forceful_fallback` == 0 | PASS (quiescence) |
| 4 | `short_lead_nodes` == 0 | PASS (909 scrapes) |
| 5 | Restarts 0, seq contiguous | PASS |
| 6 | Load present at every snapshot | PASS (desired=available=ready=5) |
| 7 | Config-under-test throughout | PASS (6 gauges × 909 scrapes) |
| 8 | Per-rotation margin > 0 | PASS (min 68.3m) |
| 9 | End census clean | PASS (all 5 claims < A) |
| 10 | No unexpected Karpenter disruption | PASS |
| 11 | Epi: frozen = candidate-only | PASS (~2h hold) |
| 12 | Epi: release fires fallback | PASS (56 s) |
| 13 | Abort rule (missed release) | N/A |

::: details Full criterion observations — click to expand

| # | Observed detail |
|---|---|
| 1 | 0 for full run + tail-follow + epilogue; no Karpenter `Expiration` events |
| 2 | 60 in [T0, T_end] (5.0/h); 71 by end (72 incl. epi); steady ~12m cadence |
| 3 | 0 the entire run — first demonstration of quiescence |
| 4 | max 0 across all 909 scrapes |
| 5 | controller `restartCount=0` for 12h; 0 gaps, 0 restarts, 0 `SCRAPE_ERROR` |
| 6 | desired=available=ready=5 throughout; no Pending backlog |
| 7 | Six derived gauges exact at 909/909 scrapes; both policies `Accepted`; exactly 1 expected warn |
| 8 | min 68.3 / median 70.3 / max 71.2 min (n=71) |
| 9 | At T_end all 5 claims aged 11–59m (right-censored); 0 stale, 0 `failed` |
| 10 | Only `DisruptionBlocked`/`Unconsolidatable` (budgets + `do-not-disrupt` suppressing) |
| 11 | `candidates=1`, `in_progress=0` held ~2h while frozen |
| 12 | See epilogue below (56 s, zero placeholder, main pool undisturbed) |
| 13 | Release completed inside bounds; fail-closed verified offline only |

:::

## Anatomy of a rotation

Each of the 71 rotations followed the same sequence:

1. Claim reaches age **1h** (= A) → placeholder Pod created
2. Karpenter provisions new node → placeholder binds → surge node Ready (**surgeWait median 34 s**, range 23–54 s)
3. Both nodes get `do-not-disrupt` → old NodeClaim deleted → voluntary drain (**drain median 44 s**, range 18–83 s)
4. Cleanup → **total median 81 s** (range 45–131 s). Old node collected by `WhenEmpty`/60s

<SoakAnatomyChart />

<SoakLedger />

## Epilogue — deterministic fallback firing

::: tip Why a separate pool
The main run's "never fires" is only half the validation. A frozen single-node pool drives one claim across the boundary deliberately and deterministically. Pool size = 1 because completion can take up to `tGP + Buffer = 7m` (multi-node evidence is [Scenario O](/validation/forceful-fallback)).
:::

| Time | Event |
|---|---|
| 02:22:50Z | Epi claim `gtx42` born (pool created with freeze) → deadline **d = 04:34:50Z** |
| 03:22:50Z | Becomes candidate at age 1h; frozen → `candidates=1`, `in_progress=0` |
| 04:23:50Z | Freeze removed at R (interval search: d−12m < R < d−8m). 11 min left < `t_rot` 12m |
| 04:24:46Z | **56 s after release**: claim deleted (drain 48 s < 7m bound). `mode=forceful-fallback`, no `surgeNode` |

**Evidence:**
- `forceful_fallback_total{nodepool-soak-epi}` 0→1
- `ForcefulFallback` Warning event with spec's message
- Continuous placeholder ledger: **zero** placeholder for epi claim
- `expired` stayed 0
- Main pool undisturbed (70→71 across the window, gaps ≤ `P + t_rot` = 42m)

The abort rule (missed release: if freeze cannot be removed before d−8m, tear down still frozen) was not exercised. Fail-closed behavior verified offline (3 legs, all exit 3).

## Reproducing this run

Runbook: [`test/e2e/eks-automode/SCENARIOS.md`](https://github.com/AkashiSN/node-rotation-controller/blob/main/test/e2e/eks-automode/SCENARIOS.md) § Scenario P.

Pre-flight checks:

```sh
go test ./internal/schedule/ -run TestDeriveScenarioPSoak -v  # pins A=1h / C=3 / G=2 / 1 warn
test/e2e/eks-automode/scenarios/soak-analyze-fixture.sh       # analyzer self-check
```

### Operational notes

- **JSON logging required** (`logging.development: false`): the analyzer reads zap JSON; console format produces empty ledger
- **Check credential lifetime up front** — operator credentials expired mid-run, blinding the secondary recorder for 15 min (in-cluster primary stayed gapless)
- **Never live-patch `expireAfter`** — recreate the pool (a patch induces Karpenter drift)
- **Cost**: ≈ $11 (control plane + NAT ~20h, 5 × 2-vCPU nodes × 14h + epilogue). Remember `terraform destroy`

## What this run settles

Spec §7.2 gains two validated rows:
- Under sub-daily windows, `leadTime` beats a genuinely racing `expireAfter` for 12h with fallback armed but quiescent
- The moment a claim crosses the point where graceful surge no longer fits, forceful fallback fires deterministically

**Remaining open item:** genuine same-AZ capacity shortage (ICE) — see [roadmap (§6.2)](/specification/06-release#62-roadmap) and [§7.2](/specification/07-risks#72-validated-assumptions).
