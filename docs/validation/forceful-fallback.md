# Forceful Fallback — Scenario O

::: tip What this validates
Window-bounded forceful fallback (#156), earliest-deadline ordering (#157), and do-not-disrupt exclusion (#170) running together on a shared deadline — all on real EKS Auto Mode.
:::

Real-EKS validation of the three features above exercised simultaneously. See [§7.2](/specification/07-risks#72-validated-assumptions) for the assumptions this flips to validated, and the [runbook](/runbook#3-interpreting-the-noderotation_-metrics) for what `noderotation_forceful_fallback_total` and `ForcefulFallback` mean operationally.

<TimelineForcefulFallback />

## Scenario coverage

The matrix below tracks every EKS Auto Mode PoC scenario against the specification's roadmap and open-questions. A scenario stays "planned" until re-run and observed on a real cluster — code coverage (unit/envtest/KWOK) is necessary but not sufficient for a row to flip to validated.

<CoverageMatrix />
