# Forceful fallback — Scenario O

Real-EKS validation of the window-bounded forceful fallback (#156), earliest-deadline
ordering (#157), and do-not-disrupt exclusion (#170) running together on a shared
deadline. See the [specification §7.2](/specification#_7-2-validated-assumptions) for
the assumptions this exercises, and the
[runbook](/runbook#_3-interpreting-the-noderotation-metrics) for what the
`noderotation_forceful_fallback_total` metric and `ForcefulFallback` Warning
Event mean operationally.

<TimelineForcefulFallback />

## Scenario coverage

The matrix below tracks every EKS Auto Mode PoC scenario run so far against the
assumptions and edge cases in the specification's roadmap and open-questions
sections. A scenario stays "planned" until it has been re-run and observed on a
real cluster — code coverage (unit/envtest/KWOK) is necessary but not sufficient
for a row to flip to validated.

<CoverageMatrix />
