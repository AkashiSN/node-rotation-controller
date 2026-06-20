# KWOK-based Karpenter e2e harness (issue #92)

This harness exercises the v1 **surge** rotation lifecycle against the **real
Karpenter v1 reconcilers and CRDs**, using Karpenter's [KWOK reference
cloudprovider](https://github.com/kubernetes-sigs/karpenter/tree/main/kwok) on a
local `kind` cluster with virtual (KWOK) nodes — zero cloud cost,
CI-reproducible. It validates the achievable subset of the spec §7.2 PoC: the
parts that do **not** require real cloud capacity or EBS. The real-cloud half
lives in the EKS Auto Mode companion (#93).

It is a **standalone** target: it is never compiled or run by `make test` /
`go test ./...` (every file carries the `//go:build e2e` tag), and the
controller module (`internal/`, `cmd/`) never imports any of it — preserving the
v1 "no cloud-provider API dependency" invariant.

## What runs in the cluster

| Component | Version (pinned) | Source |
|-----------|------------------|--------|
| kind node image | `kindest/node:v1.35.0` (digest) | `kind.yaml` (matches `kind v0.31.0`) |
| KWOK controller | `v0.5.2` | `github.com/kubernetes-sigs/kwok` kustomize + stages |
| Karpenter KWOK cloudprovider | the **exact** tag in the repo's `go.mod` (`sigs.k8s.io/karpenter`, currently `v1.13.0`) | built with `ko` from a throwaway module — see below |
| node-rotation-controller | the PR-built image | this repo's Helm chart (`charts/`) |

### How the Karpenter KWOK provider is built (reproducibly, in isolation)

There is no published, version-matched KWOK provider image for an arbitrary
Karpenter tag, so `build-kwok-image.sh` builds one with
[`ko`](https://ko.build) from **`sigs.k8s.io/karpenter/kwok`** at the exact tag
this repo already vendors. The build happens in a **throwaway Go module** under a
temp dir whose only dependency is that pinned tag, so the cloudprovider package
never enters the controller module's `go.mod`/`go.sum`. The CRDs and the
Karpenter Helm chart come from the same module in the Go module cache, so they
are always tag-consistent with the binary. The image is tagged
`ko.local/karpenter-kwok:<tag>` and loaded into kind.

> **Chart caveat (v1.13.0):** the vendored KWOK Helm chart's deployment template
> references `settings.featureGates.staticCapacity`, which the chart's
> `values.yaml` does not default — left unset it renders `FEATURE_GATES=…
> StaticCapacity=,…` and the controller panics. `bootstrap.sh` therefore passes
> `--set settings.featureGates.staticCapacity=false` (and the other two gates).

## Prerequisites

- Docker running, and these tools on `PATH`: `kind`, `kubectl`, `helm`, `ko`,
  `kustomize`, `go`, `docker`. `make e2e-kwok` installs the Go-installable ones
  (`kind`, `ko`, `kustomize`) into `./bin` at pinned versions.
- Network access to pull the KWOK release manifests and the kind/pause images.

## Running

```bash
make e2e-kwok                 # build image → kind up → install → go test → teardown
make e2e-kwok E2E_KWOK_KEEP=true   # leave the cluster up for debugging
```

`make e2e-kwok` builds the controller image, runs `bootstrap.sh` (cluster +
components + chart), then `go test -tags e2e ./test/e2e/kwok/...`.

## Layout

- `kind.yaml` — single-node kind cluster (digest-pinned node image).
- `manifests/` — NodePools + KWOKNodeClass (`nodepools.yaml`), the deterministic
  instance-types file (`instance-types.json`, single zone `test-zone-a`), and the
  controller Helm values overlay (`controller-values.yaml`).
- `bootstrap.sh` — provisions the cluster and installs everything (idempotent).
- `build-kwok-image.sh` — builds the pinned KWOK provider image in isolation.
- `*_test.go` (`e2e` tag) — the Go driver + assertions.

## Acceptance criteria coverage

| Criterion (issue #92) | Status | Where |
|---|---|---|
| Capacity-absorb → `complete` with **no** new NodeClaim | ✅ proven | `testCapacityAbsorb` |
| Completion chain (placeholder deleted, target unfrozen, anchor cleared last) | ✅ proven | `testCapacityAbsorb` |
| Success + drain-duration via **scraping `/metrics`** | ✅ proven | `metrics_test.go` |
| Multi-NodePool confinement incl. "other pool has spare, not absorbed" | ✅ proven | `testConfinement` |
| Placeholder required `karpenter.sh/nodepool` selector; surge `Node` pool label | ✅ proven | `testConfinement` |
| Voluntary drain honors a blocking PDB; loosening lets it finish | ✅ proven | `testPDB` |
| `karpenter.sh/do-not-disrupt` present + controller-owned on **both** surge-pair nodes | ✅ proven (annotation-set form) | `testDoNotDisrupt` |
| Bare-placeholder preemption: a competing workload preempts the negative-priority placeholder (it is the victim), the real workload is **not** evicted in its place, and it does **not** re-pend | ✅ proven | `testPreemption` |

`testDoNotDisrupt` parks the surge in flight with a blocking PDB on the candidate
workload, so both surge-pair nodes stay frozen for a deterministic window rather
than the few seconds KWOK's fast drain would otherwise leave to observe.

## KWOK limitations — what is **not** asserted here (and why)

These are honest gaps, not skipped assertions. They move to EKS (#93) or a
follow-up.

1. **New-NodeClaim provisioning of the surge node.** Core Karpenter v1 lists
   `kubernetes.io/hostname` in `RestrictedLabels`, so the **provisioner rejects
   any provisionable Pod whose `nodeAffinity` references it** ("using label
   kubernetes.io/hostname is not allowed …"). The controller's placeholder
   **always** carries the §3.3 candidate-exclusion (`kubernetes.io/hostname
   NotIn {candidate, near-deadline}`). Consequently a brand-new surge node cannot
   be *induced* under KWOK — the placeholder stays `Pending` and the attempt
   rolls back at `readyTimeout`. The harness therefore drives completion via the
   **capacity-absorb** path, where `kube-scheduler` (not Karpenter's provisioner)
   evaluates the hostname `NotIn` while bin-packing onto an existing node. The
   new-provision **assertion of the new NodeClaim/Node's pool labels** is covered
   indirectly (the absorb surge-target's pool label is asserted), but a *brand-new
   surge NodeClaim reaching `complete`* is out of scope here. NOTE: because this
   is **core** Karpenter behavior (not KWOK-specific), it is tracked as a
   controller/spec **design decision** in **#96** (placeholder hostname-exclusion
   design); it is *not* fixed under #92.

2. **do-not-disrupt honored against voluntary disruption.** We assert the
   annotation is *set and owned* on both surge-pair nodes, but do **not** claim
   Karpenter honored it: the NodePools run `consolidationPolicy: WhenEmpty` with a
   very long `consolidateAfter`, so no voluntary Consolidation/Drift is induced
   under KWOK to honor. The stronger "no disruption while the annotation is set"
   claim is deferred to EKS (#93), per the issue's explicit branch.

3. **Mid-surge (pending-phase) preemption → `readyTimeout` rollback (issue #92
   P1).** `testPreemption` proves the bare-placeholder victim path by parking the
   rotation in its **drain** phase (old NodeClaim deleted, drain held by a PDB),
   where `advanceDraining` does not recreate the placeholder — so a competing
   priority-0 workload pinned to the surge host evicts the placeholder
   permanently, and we assert it is the victim, the real workload is not evicted
   in its place, and it does not re-pend. The remaining, deferred piece is
   preemption during the **pending** phase, where the controller *intentionally*
   recreates the placeholder and the attempt is bounded by `readyTimeout` →
   rollback; that recreate-vs-complete race is timing-fragile under KWOK and is
   the issue #92 P1 item, left to the follow-up / EKS (#93).

4. **Real same-AZ capacity shortage, NodePool `limits` exhaustion, zonal-PV/EBS
   rebind, and the `expireAfter` real-soak race** — these require real cloud and
   are validated on EKS Auto Mode (#93), per §7.2.

## Determinism note (why `ageThreshold` is 4m, not seconds)

The capacity-absorb path needs the absorb-target spare node to stay **out of the
placeholder's near-deadline hostname-exclusion set** (§3.3) for the whole
rotation — i.e. the spare must remain *below* the age threshold while the
candidate is *above* it. The driver provisions the candidate, ages it to roughly
the threshold, then provisions a **fresh** spare; the candidate crosses first
(sole eligible candidate) and the spare stays young through completion. A
few-second threshold would make *every* node near-deadline at once, so the
placeholder would exclude its only possible absorb target. Keep
`controller-values.yaml`'s `ageThreshold` in sync with the constant in
`e2e_test.go`.

## Two KWOK-quiescence accommodations the driver makes

KWOK runs a **static** cluster: once its virtual nodes register it emits almost
no further watch events. Two harness mechanisms compensate — neither changes the
controller or chart; both only let the controller's *normal* code run the way a
live, churning cluster would drive it.

1. **Reconcile nudge (`startNudger`).** The controller's watches are
   predicate-filtered to real transitions (placeholder→Running, node→Ready,
   NodeClaim/NodePool changes), with the periodic requeue as the backstop. On a
   live cluster constant pod/node churn keeps reconciles flowing; under KWOK the
   cluster falls silent during a candidate's age-out, so the controller leans on
   its slow periodic requeue and can miss the candidate crossing `ageThreshold`
   within a subtest's window. Each subtest therefore defers a nudger that touches
   a benign `noderotation-e2e/nudge` NodePool annotation every 20s — a value the
   controller never reads — to wake the reconcile. It changes no rotation logic.

2. **Short `retryBackoff` (5s).** The pending handler stamps `started-at` and
   immediately re-reads the candidate through the controller's informer cache;
   under KWOK that cached read can briefly lag the write, so a freshly selected
   candidate **occasionally** mis-fires the `readyTimeout` rollback and lands in
   `failed`. With the production-style long backoff it would be stranded there
   for the whole subtest; a short backoff lets the state machine re-enter
   `pending` within the window and converge through the *same*
   absorb→drain→complete path. (The value is deliberately small: `EscalatedBackoff`
   caps at 8× the base, so a smaller base lowers the ceiling and roughly doubles
   the attempts that fit in a subtest window — needed on loaded CI runners where
   the cache lag fires more often.) (This is intermittent and harmless to the assertions
   — the spare never ages into the placeholder's hostname exclusion across retries,
   which is keyed on `expireAfter`/`leadTime`, not `ageThreshold`.) The underlying
   read-after-write cache lag is a minor controller observation noted for the
   follow-up; it is *not* fixed under #92.
