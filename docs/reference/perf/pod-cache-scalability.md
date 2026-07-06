# Cluster-wide Pod cache / list scalability

Status: **decision recorded — current full-list approach accepted for v0.x target
sizes; a `spec.nodeName` field index is recorded as a follow-up optimization.**

Tracks issue #80. This note measures the cost of the controller's cluster-wide
Pod reads and records the resulting decision. It does not change runtime
behavior.

## Why the controller reads all Pods

The placeholder Pod is sized to the **sum of the reschedulable Pod requests on
the candidate node** (spec §3.3), and those Pods may live in *arbitrary
namespaces*. The controller therefore needs cluster-wide Pod visibility — it
cannot be namespace-scoped without breaking cross-namespace workload support.

Today that visibility is a full cache-backed Pod list, `allPods()` in
`internal/controller/rotation_controller.go`, fed to:

| Hot path | Function | Notes |
| --- | --- | --- |
| candidate headroom check | `candidateRequests` → `surge.ReschedulableRequests` | per rotation pass while pending |
| placeholder creation | `createPlaceholder` → `surge.ReschedulableRequests` | once per placeholder (re)create |
| surge-claim reap guard | `reapSurgeClaim` → `hostsRealPods` | rollback / absorb-host guard |

Each of these takes the **whole** Pod slice and scans it, even though only the
Pods on a single node (the candidate or surge node) are relevant. The exclusion
semantics that must be preserved by any optimization live in
`internal/surge/requests.go`: DaemonSet, mirror/static, completed
(`Succeeded`/`Failed`), and node-pinned Pods.

## Benchmark

`internal/surge/requests_bench_test.go` builds synthetic cluster snapshots at
1k / 10k / 50k Pods spread across 200 namespaces, with a realistic mix on the
candidate node (plain workloads plus a DaemonSet, completed, and hostname-pinned
Pod), and the bulk scheduled on ~500 other nodes. It benchmarks the **real
exported** `surge.ReschedulableRequests` and `surge.IsInfraOrCompleted`, plus a
`Scoped` variant that pre-filters to the candidate node (the would-be
`spec.nodeName`-index input) and a coarse snapshot-footprint proxy.

Reproduce:

```
go test -bench='ReschedulableRequests|IsInfraOrCompleted|SnapshotFootprint' \
  -benchmem -run=^$ ./internal/surge/
```

Results (Apple M4 Pro, `goarch=arm64`, Go 1.26):

```
BenchmarkReschedulableRequests/pods=1000-14         128328      8779 ns/op    17152 B/op   86 allocs/op
BenchmarkReschedulableRequests/pods=10000-14         55826     20118 ns/op    17152 B/op   86 allocs/op
BenchmarkReschedulableRequests/pods=50000-14         10000    104697 ns/op    17152 B/op   86 allocs/op
BenchmarkIsInfraOrCompleted-14                       380966      3047 ns/op        0 B/op    0 allocs/op
BenchmarkReschedulableRequestsScoped/pods=1000-14    162265      7521 ns/op    17152 B/op   86 allocs/op
BenchmarkReschedulableRequestsScoped/pods=10000-14   159470      7349 ns/op    17152 B/op   86 allocs/op
BenchmarkReschedulableRequestsScoped/pods=50000-14   171224      7066 ns/op    17152 B/op   86 allocs/op
BenchmarkSnapshotFootprint/pods=1000-14                1627    741693 ns/op   3714746 B/op   10333 allocs/op
BenchmarkSnapshotFootprint/pods=10000-14                223   5365664 ns/op  37091770 B/op  104730 allocs/op
BenchmarkSnapshotFootprint/pods=50000-14                 45  25918844 ns/op 185405625 B/op  524254 allocs/op
```

### Reading the numbers

- **Reconcile-path CPU is negligible.** A full-list scan over **50 000 Pods**
  costs **~105 µs** per call and allocates a fixed **~17 KB / 86 allocs**
  (independent of cluster size — the allocations are the result `ResourceList`,
  not the scan). At 10k Pods it is ~20 µs; at 1k, ~9 µs. These calls run a
  handful of times per rotation pass, not per Pod event, so even a 50k-Pod
  cluster adds well under a millisecond of CPU per pass. This is far below the
  reconcile's network round-trips (Get Node, Create/Delete NodeClaim).
- **The scan scales linearly** with total Pod count (~2 ns/Pod), as expected:
  every Pod is visited and its `spec.nodeName` compared first.
- **The would-be indexed path is flat.** `Scoped` (pre-filtered to the candidate
  node) stays ~7 µs regardless of cluster size — the ~15× gap at 50k is the work
  a `spec.nodeName` field index would remove. In absolute terms it removes
  ~100 µs of CPU per call: real, but not a bottleneck.
- **Memory is the meaningful cost, and it lives in the informer cache, not the
  scan.** The footprint proxy shows the *object data* the controller-runtime Pod
  cache must hold: ~**3.7 KB/Pod** here, i.e. ~**3.7 MB @ 1k**, ~**37 MB @ 10k**,
  ~**185 MB @ 50k**. The real cached `corev1.Pod` is heavier than this synthetic
  proxy (full status, conditions, env, volumes, managedFields), so treat these
  as a conservative lower bound; production 50k-Pod caches commonly run several
  hundred MB to ~1 GB.

## Options evaluated (issue #80)

1. **Keep full-list cache-backed scans (status quo).** Simplest, already
   correct, preserves all exclusion semantics. CPU cost is negligible at target
   sizes. The only real cost is the cluster-wide Pod **cache memory**, which is
   inherent to needing cross-namespace, all-node Pod visibility for the absorb
   guard and is *not* avoided by changing the scan.

2. **Field index on `spec.nodeName`.** Register a controller-runtime field
   indexer (`mgr.GetFieldIndexer().IndexField(&corev1.Pod{}, "spec.nodeName", …)`)
   and replace the three `allPods()` scans with
   `List(ctx, &pods, client.MatchingFields{"spec.nodeName": node})`. This removes
   the per-call linear scan (the ~100 µs at 50k becomes ~flat) and shrinks each
   call's working set to one node's Pods. It **does not reduce cache memory** —
   controller-runtime still caches every Pod to maintain the index — so it is a
   CPU/clarity win, not a memory win. Exclusion semantics are untouched: the same
   `surge.ReschedulableRequests` / `hostsRealPods` filters run on the
   index-narrowed slice.

3. **Cache selectors / namespace scoping** (`cache.Options.ByObject` label/field
   selectors, or `DefaultNamespaces`). This is the only option that reduces cache
   **memory** — but it is **incompatible with the cross-namespace requirement**.
   The reschedulable sum and the absorb-host guard must see Pods in *any*
   namespace on the candidate/surge node, so we cannot scope by namespace, and
   there is no stable label predicate that selects "every Pod that could land on
   a rotating node." Rejected for v1. (A future field-selector cache restricted
   to `spec.nodeName in {nodes under rotation}` is not expressible as a static
   cache selector, since the node set is dynamic.)

## Decision

**The current full-list approach is accepted for v0.x.** At the expected target
cluster sizes for the MVP (up to ~10k–50k Pods) the reconcile-path CPU is
sub-millisecond and the allocation profile is fixed and small. The dominant cost
— cluster-wide Pod cache memory — is **inherent to the cross-namespace,
all-node visibility the design requires** and is not removed by any of the scan
optimizations; only namespace/label scoping would shrink it, and that breaks the
core requirement (option 3, rejected).

**Recommended follow-up (not implemented here): add a `spec.nodeName` field
index** and switch the three hot paths to a node-scoped list (option 2). It is a
small, low-risk, clearly-beneficial change that:

- removes the per-call linear scan (flat ~7 µs vs. up-to-~105 µs),
- narrows each call's working set to one node's Pods,
- preserves exclusion semantics unchanged (the existing
  `internal/surge/requests` and controller tests continue to cover the filters),
- but **does not** reduce cache memory, so it is a latency/cleanliness
  improvement rather than a fix for the memory ceiling.

It is recorded as a follow-up rather than implemented in this PR to keep the
measure-and-decide change reviewable and because the win is a CPU
micro-optimization, not a correctness or memory fix — the appropriate threshold
for the "when in doubt, measure + recommend" guidance. If a real EKS Auto Mode
soak test (issues #77/#78) surfaces cache-memory pressure, that is a separate
concern (cache scoping/pagination at the controller-runtime level), tracked
independently from this scan optimization.

### Suggested follow-up issue

> **perf(controller): index Pods by `spec.nodeName` and scope the three Pod-read
> hot paths.** Register a `spec.nodeName` field indexer and replace `allPods()`
> in `candidateRequests`, `createPlaceholder`, and `reapSurgeClaim` with
> node-scoped `MatchingFields` lists. Must keep `internal/surge/requests` and
> controller tests green and preserve DaemonSet / mirror / completed / node-pinned
> exclusion semantics. Reference the benchmark in `docs/reference/perf/pod-cache-scalability.md`.
