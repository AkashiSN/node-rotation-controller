# Reproducible PoC validation scenarios (issue #93)

This runbook lets a third party **re-run the spec
[§7.2](../../../docs/specification/07-risks.md#72-validated-assumptions) PoC validation**
against a real EKS Auto Mode cluster and reach the same outcomes. It is the
"observe rotations" half that the infra
[`README.md`](README.md) (steps 4) only sketches: here are the exact NodePool,
workloads, controller config, trigger/observe commands, **expected output**, and
the non-obvious gotchas.

Every manifest referenced lives in [`scenarios/`](scenarios/). All commands are
copy-pasteable; cluster-specific values come from `terraform output`, so nothing
account-specific is hard-coded.

For the **recorded outcomes** of actually running this runbook (per-scenario
verdicts and the concrete evidence observed on a given release), see
[`VALIDATION.md`](VALIDATION.md). This file is the *how*; that file is the *what
happened*.

> **Cost.** Each scenario launches real EC2 (and Scenario A an EBS volume).
> Follow the cleanup at the end of each scenario and the final
> [Teardown](#teardown). The whole suite is a handful of `c6a.large`-class nodes
> for a few minutes each.

---

## 1. Prerequisites

1. The ephemeral cluster is **up** and you have a kubeconfig — infra
   [`README.md`](README.md) steps 1–2:

   ```bash
   cd test/e2e/eks-automode
   eval "$(terraform output -raw kubeconfig_command)"   # or: make e2e-eks-kubeconfig
   export KUBECONFIG=$PWD/kubeconfig
   kubectl get nodepools.karpenter.sh        # general-purpose + system, READY
   ```

2. The controller image is **pushed to the stack's ECR** as tag `poc` — infra
   [`README.md`](README.md) step 3. Multi-arch (amd64 **and** arm64) because Auto
   Mode may launch either.

3. The controller is **not yet installed** (this runbook installs it in
   [§3](#3-shared-setup)).

Export the coordinates the commands below reuse:

```bash
export KUBECONFIG=$PWD/kubeconfig            # from test/e2e/eks-automode
export REPO=$(terraform output -raw ecr_repository_url)
export REGION=$(terraform output -raw region)
```

Reference environment: **EKS Auto Mode, K8s 1.36, `karpenter.sh/v1`**,
controller image tag `poc`, region `us-west-2` (2 AZs: `us-west-2a`,
`us-west-2b`). For the dated record of which release was validated on what, see
[`VALIDATION.md`](VALIDATION.md).

---

## 2. How the scenarios work

The controller rotates a NodePool when **all** of these hold (spec §3.2, §5.2):

- the NodePool carries a label matched by a RotationPolicy's `nodePoolSelector`
  (here `noderotation-poc/in-scope: "true"`, set in `scenarios/rotationpolicy.yaml`);
- a maintenance window is open (here a 24×7 window — always open);
- a candidate NodeClaim is older than `ageThreshold` (here **5m**);
- the start gates pass (no freeze, cooldown elapsed) and the surge fits the
  NodePool's `limits` headroom.

So the **rotation switch is the `noderotation-poc/in-scope` label** on the
`nrc-poc` NodePool:

```bash
# START rotating the pool:
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
# STOP (halt the loop):
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
```

> **Always halt after one rotation.** With `ageThreshold=5m`, the fresh surge
> node itself crosses 5m a few minutes later and rotates again — an endless loop
> that burns nodes. Every scenario below ends by removing the label.

**Watching state.** Two windows help:

```bash
# rotation lifecycle on the candidate's NodePool + its NodeClaims
watch -n2 'kubectl get nodepool nrc-poc -o jsonpath="active={.metadata.annotations.noderotation\.io/active-rotation} state={.metadata.annotations.noderotation\.io/active-rotation-state} lastRot={.metadata.annotations.noderotation\.io/last-rotation-at}{\"\n\"}"; \
  kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc -o wide'

# controller logs
kubectl logs -n node-rotation-system deploy/node-rotation-controller -f
```

**Reading metrics** (spec §4.2) — the authoritative pass/fail signal:

```bash
kubectl port-forward -n node-rotation-system svc/node-rotation-controller-metrics 8080:8080 &
curl -s localhost:8080/metrics | grep -E '^noderotation_'
```

---

## 3. Shared setup

Apply once; every scenario reuses it (Scenario A swaps the stateless workload for
a stateful one).

### 3.1 The dedicated NodePool

```bash
kubectl apply -f scenarios/nodepool.yaml
```

`scenarios/nodepool.yaml` is the **only in-scope pool**: a custom NodePool
referencing the built-in Auto Mode NodeClass `default`, labelled
`noderotation-poc/in-scope: "true"`. Key fields and **why** (full rationale in the
file header):

| Field | Value | Why |
|-------|-------|-----|
| `expireAfter` | `336h` | §3.5 backstop, set far out. **Must** be large: a daily window has worst-case period P=24h, and the controller fatally refuses to start unless `expireAfter` guarantees ≥ `minRotationChances` window occurrences after `ageThreshold` (else `OverrideGBelowOne`, see [Gotchas](#6-gotchas--troubleshooting)). |
| `disruption.consolidationPolicy` / `consolidateAfter` | `WhenEmpty` / `60s` | Reclaim leftover empty surge nodes quickly without disrupting the busy test node. |
| `limits.cpu` | `16` | Headroom for a make-before-break surge (2 small nodes at once). Scenario B patches this down. |
| `eks.amazonaws.com/instance-cpu` | `2`–`4` | Keep cost low and the surge node size predictable. |

### 3.2 Install the controller

```bash
helm install node-rotation-controller ../../../charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  -f scenarios/controller-values.yaml \
  --set image.repository="$REPO" --set image.tag=poc \
  --wait --timeout 8m

# Apply the live RotationPolicy (issue #119). The chart's crds/ installed the CRD
# first, and controller-values.yaml set rotationPolicies: [], so this is
# the only policy object. The controller WATCHES it, so later scenarios change
# knobs with `kubectl patch rotationpolicy nrc-poc …` — no restart.
kubectl apply -f scenarios/rotationpolicy.yaml
```

Installing onto a zero-node cluster makes Auto Mode launch a node in the
`general-purpose` pool to host the controller — expected, and **out of scope**
(only `nrc-poc` is rotated). Confirm health:

```bash
kubectl -n node-rotation-system get deploy node-rotation-controller          # 1/1
kubectl -n node-rotation-system get lease node-rotation-controller.noderotation.io
kubectl -n node-rotation-system logs deploy/node-rotation-controller | grep -i "Starting workers"
```

`scenarios/controller-values.yaml` sets the controller knobs (single replica,
off-pool affinity); the rotation policy lives in `scenarios/rotationpolicy.yaml`
(rationale in the files): `ageThreshold: 5m`, always-open window,
`surge.readyTimeout: 15m`, `cooldownAfter: 1m`, `retryBackoff: 30m`. **The
RotationPolicy is watched live** (issue #119) — changing a knob is a
`kubectl patch rotationpolicy nrc-poc …` that takes effect without a controller
restart (used in Scenarios C, L, M).

---

## 4. Scenarios

Each scenario: **Goal → Preconditions → Run → Expected → Cleanup.** "Expected"
is the run-independent shape a correct run produces — the per-step pass
criteria. The illustrative values are drawn from real runs; node/claim IDs and
exact seconds will differ, the shape will not. For dated per-release run records
see [`VALIDATION.md`](VALIDATION.md).

### Scenario 0 — core make-before-break surge

**Goal.** The §3.3 placeholder-Pod surge induces a NodePool-owned replacement and
completes make-before-break (surge node Ready **before** the old node drains).

**Preconditions.** [§3](#3-shared-setup) applied. Apply the stateless workload to
force one node up:

```bash
kubectl apply -f scenarios/workload.yaml
kubectl wait --for=condition=Ready pod -l app=poc-workload --timeout=5m
kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc      # one Ready c6a.large
```

**Run.** The pool is in-scope already (the label ships on the manifest); the node
crosses `ageThreshold` ~5m after it became Ready. Just watch. (If you removed the
label, re-add it per [§2](#2-how-the-scenarios-work).)

**Expected** (≈1 min once the candidate is eligible):

| Phase | Observation |
|-------|-------------|
| start | candidate NodeClaim annotation `noderotation.io/state=pending`; a **new** surge NodeClaim appears (`Ready=Unknown`); placeholder Pod `noderotation-surge-<candidate>` is `Pending` in `node-rotation-system` |
| surge ready | surge NodeClaim → `Ready=True` (~30s) **while the old one still exists**; candidate → `state=draining`; pool `active-rotation-state=draining` |
| complete | old candidate NodeClaim **deleted**; only the new claim remains; pool `last-rotation-at` stamped, anchor annotations cleared; the workload pod reschedules onto the surge node |

Metrics:

```text
noderotation_completed_total{nodepool="nrc-poc",outcome="success"} 1
noderotation_in_progress{nodepool="nrc-poc"} 0
```

**Cleanup.**

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-     # halt the loop
```

---

### Scenario A — same-AZ surge → zonal-EBS (zonal-PV) rebind

**Goal.** A same-AZ surge lets the EBS CSI driver re-attach a **zonal** EBS
volume, so a stateful pod keeps its data across the rotation (spec §7.2, §3.3).

**Preconditions.** [§3](#3-shared-setup) applied. Replace the stateless workload
with the EBS-backed StatefulSet:

```bash
kubectl delete -f scenarios/workload.yaml --ignore-not-found
kubectl apply -f scenarios/statefulset-ebs.yaml
kubectl wait --for=condition=Ready pod poc-stateful-0 --timeout=5m

# the sentinel + the volume's AZ
kubectl logs poc-stateful-0 | grep -i sentinel                # WROTE sentinel: sentinel-...
PV=$(kubectl get pvc data-poc-stateful-0 -o jsonpath='{.spec.volumeName}')
kubectl get pv "$PV" -o jsonpath='{.spec.nodeAffinity.required.nodeSelectorTerms[0].matchExpressions}'
#   ... topology.kubernetes.io/zone In [us-west-2a]   <-- volume is zonal
```

**Run.** Trigger a rotation and let it complete:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
# watch until the candidate NodeClaim is replaced and poc-stateful-0 is Running again
kubectl label nodepool nrc-poc noderotation-poc/in-scope-    # halt after one rotation
```

**Expected.** The surge node is in the **same AZ** as the candidate
(`us-west-2a`), so:

```bash
# pod is Running on the NEW node:
kubectl get pod poc-stateful-0 -o wide
# the PVC still points at the SAME PV (re-attached, not reprovisioned):
kubectl get pvc data-poc-stateful-0 -o jsonpath='{.spec.volumeName}'    # == $PV
# the sentinel survived:
kubectl exec poc-stateful-0 -- cat /data/sentinel                      # same value as before
# every nrc-poc node is us-west-2a:
kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc \
  -o custom-columns=NAME:.metadata.name,ZONE:.metadata.labels.topology\\.kubernetes\\.io/zone
```

If the surge node had landed in a different AZ, the pod would be stuck `Pending`
on a volume-node-affinity conflict — that it stays `Running` with the same PV is
the rebind.

**Cleanup.**

```bash
kubectl delete -f scenarios/statefulset-ebs.yaml             # also deletes the PVC/PV (Delete reclaim)
```

---

### Scenario B — NodePool `limits` exhaustion gates the surge

**Goal.** With no resource headroom, the controller **refuses to start** a surge
(it does not churn or fail) — spec §5.2 step 3.

**Preconditions.** [§3](#3-shared-setup) applied with the stateless workload
(Scenario 0's `scenarios/workload.yaml`), one Ready `nrc-poc` node. The candidate
must be a **fresh** eligible claim — if a prior scenario left it `state=failed`,
clear its rotation annotations first (see [Gotchas](#6-gotchas--troubleshooting)).

**Run.** Shrink the pool's cpu budget to the already-provisioned amount, then make
the candidate eligible:

```bash
# the surge needs room for the candidate's reschedulable requests (~1 cpu);
# leaving no headroom blocks it. provisioned cpu for one c6a.large is 2:
kubectl get nodepool nrc-poc -o jsonpath='provisioned={.status.resources.cpu} limits={.spec.limits.cpu}{"\n"}'
kubectl patch nodepool nrc-poc --type merge -p '{"spec":{"limits":{"cpu":"2"}}}'   # remaining = 0
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
```

`limits.cpu` lives in `spec.limits` (not the template), so this is read live — **no
controller restart and no Karpenter drift**.

**Expected** (within ~30s). The candidate is eligible but no surge starts:

```text
# controller log:
INFO  insufficient limits headroom; cannot surge   ... "candidate":"nrc-poc-..."

# metrics — eligible but gated, no in-flight rotation:
noderotation_candidates{nodepool="nrc-poc"} 1
noderotation_in_progress{nodepool="nrc-poc"} 0
```

No new NodeClaim is created; the NodePool stays at one node; no
`active-rotation` anchor is set.

**Cleanup.**

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
kubectl patch nodepool nrc-poc --type merge -p '{"spec":{"limits":{"cpu":"16"}}}'   # restore headroom
```

---

### Scenario C — `readyTimeout` rollback + cleanup

**Goal.** A surge that does not reach Ready within `readyTimeout` **rolls back
cleanly**: the induced node is reaped, the candidate is retained (not rotated) and
un-cordoned, and a failure is recorded (spec §3.2 / §5.2, R3).

**Preconditions.** [§3](#3-shared-setup) applied with the stateless workload, one
Ready `nrc-poc` node, candidate **fresh** (clear `state=failed` if a prior
scenario left it — see [Gotchas](#6-gotchas--troubleshooting)). The pool must have
headroom (`limits.cpu: 16`, restored after Scenario B).

**Run.** Shorten `readyTimeout` below the real node-ready time (~30s) so even a
normal surge times out, then trigger. The RotationPolicy is watched live, so the
patch applies without a restart:

```bash
# patch readyTimeout 15m -> 15s live (keep the long retryBackoff)
kubectl patch rotationpolicy nrc-poc --type merge \
  -p '{"spec":{"surge":{"readyTimeout":"15s"}}}'

kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
```

**Expected.** The surge starts (candidate `pending`, node cordoned, placeholder
+ surge claim appear), then at ~15s the readyTimeout fires **before** the surge is
Ready and rolls back:

| After rollback | Check |
|----------------|-------|
| candidate **retained** (NOT rotated), now `state=failed`, `retry-count=1` | `kubectl get nodeclaim <candidate> -o jsonpath='{.metadata.annotations}'` |
| candidate node **un-cordoned** | `kubectl get node <node> -o jsonpath='{.spec.unschedulable}'` → empty |
| placeholder Pod **deleted** | `kubectl get pod -n node-rotation-system noderotation-surge-<candidate>` → NotFound |
| induced surge claim **reaped** | `kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc` → back to 1 |

Metrics:

```text
noderotation_completed_total{nodepool="nrc-poc",outcome="failure"} 1
noderotation_retry_count{nodepool="nrc-poc"} 1
noderotation_in_progress{nodepool="nrc-poc"} 0
```

Remove the label promptly — `retryBackoff` (30m) keeps it from retrying while you
inspect, but the label off is the definitive halt.

**Cleanup.** Restore the normal `readyTimeout` (live patch) and clear the failed
marker so the candidate is reusable:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
kubectl patch rotationpolicy nrc-poc --type merge \
  -p '{"spec":{"surge":{"readyTimeout":"15m"}}}'
# make the candidate fresh again (see Gotchas):
C=$(kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc -o jsonpath='{.items[0].metadata.name}')
kubectl annotate nodeclaim "$C" noderotation.io/state- noderotation.io/failed-at- noderotation.io/retry-count-
```

---

### Scenario D — `expireAfter` stays a backstop the controller wins against (R6)

**Goal.** The controller's `ageThreshold`-driven rotation fires far inside the
retained `expireAfter` backstop, so forceful expiration never races it.

**Preconditions.** [§3](#3-shared-setup) applied.

**Run / Expected.** This is validated **by construction plus observation** rather
than a multi-hour soak: the candidate keeps `expireAfter=336h` (the controller
never removes it, spec §3.5) while rotation is driven at `ageThreshold=5m`. The
§4.2 gauges quantify the margin:

```bash
kubectl get nodeclaim <candidate> -o jsonpath='expireAfter={.spec.expireAfter}{"\n"}'   # 336h
kubectl port-forward -n node-rotation-system svc/node-rotation-controller-metrics 8080:8080 &
curl -s localhost:8080/metrics | grep -E \
  'noderotation_age_threshold_seconds|noderotation_rotation_chances|noderotation_window_period_seconds|noderotation_short_lead_nodes'
```

```text
noderotation_age_threshold_seconds{nodepool="nrc-poc"} 300       # rotate at 5m
noderotation_rotation_chances{nodepool="nrc-poc"}      13        # 13 guaranteed chances before expireAfter
noderotation_window_period_seconds{nodepool="nrc-poc"} 86400     # daily window, P=24h
noderotation_short_lead_nodes{nodepool="nrc-poc"}      0         # nothing at risk of the backstop racing
```

`rotation_chances=13` (≫ `minRotationChances=2`) and `short_lead_nodes=0` show the
lead time wins with a wide margin; every rotation observed in Scenarios 0/A fired
at ~5m, hundreds of times before a single 336h `expireAfter` could fire.

> A genuine multi-hour soak with a *tight* race is not achievable with a daily
> window (P=24h forces a large `expireAfter`, see [Gotchas](#6-gotchas--troubleshooting));
> Scenario I below runs the scaled form — several consecutive rotations, none
> approaching expiry.

### Scenario E — `expired` outcome (force-expiry caught in pending)

**Goal.** When a candidate is force-expired (its NodeClaim deleted) *while a
rotation is pending*, the controller records it as **expired**, not success/failure
— nothing rotated, no cooldown consumed (spec §5.2, #81 / #93).

**Preconditions.** [§3](#3-shared-setup), stateless workload, one Ready node,
fresh candidate, `readyTimeout: 15m` (the base value — long, so the pending window
does not time out first).

**Run.** Trigger, then the moment the candidate is `pending`, freeze the pool (to
hold escalation so no surge progresses) and delete the candidate NodeClaim — the
real-world force-expiry:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
# wait until: kubectl get nodeclaim <cand> -o jsonpath='{.metadata.annotations.noderotation\.io/state}' == pending
kubectl annotate nodepool nrc-poc noderotation.io/freeze="$(date -u -d '+15 min' +%Y-%m-%dT%H:%M:%SZ)" --overwrite
kubectl delete nodeclaim <cand> --wait=false
```

**Expected.** The candidate is caught in pending and the rotation aborts to the
expired path: `state=expired`, the pool `active-rotation` anchor cleared, no
placeholder/surge left behind (the freeze meant none was created).

```text
noderotation_completed_total{nodepool="nrc-poc",outcome="expired"} 1
noderotation_in_progress{nodepool="nrc-poc"} 0
```

**Cleanup.** `kubectl annotate nodepool nrc-poc noderotation.io/freeze-` and remove
the in-scope label.

> **Pin the controller off nrc-poc first.** If the controller Pod is running on the
> nrc-poc node you delete here, it restarts and resets the in-memory metric
> counters before you can read them. `scenarios/controller-values.yaml` carries the
> `affinity` that keeps it on the general-purpose/system pools — keep it.

### Scenario F — multi-NodePool confinement

**Goal.** The required `karpenter.sh/nodepool=nrc-poc` selector on the placeholder
confines **both** kube-scheduler binding **and** Karpenter provisioning to nrc-poc
— a same-AZ spare in another pool is never used (spec §3.3, #77 P0).

**Preconditions.** [§3](#3-shared-setup) with the stateless workload (the nrc-poc
candidate must be `us-west-2a`). Stand up a second, out-of-scope pool with same-AZ
spare:

```bash
kubectl apply -f scenarios/nodepool-b.yaml          # nrc-poc-b (pool=b, NOT in-scope) + a filler
kubectl wait --for=condition=Ready pod -l app=poc-filler-b --timeout=5m
# nrc-poc-b now has one us-west-2a node with ~1.7 cpu free — ample for the ~1cpu placeholder
```

**Run.** Note the nrc-poc-b claims, trigger an nrc-poc rotation, let it complete,
then halt.

**Expected.** Despite the same-AZ spare, the surge stays in nrc-poc:

- the surge NodeClaim carries `karpenter.sh/nodepool=nrc-poc` (not `nrc-poc-b`);
- the placeholder binds an nrc-poc node (`noderotation-poc/pool=poc`), not pool b;
- **nrc-poc-b's NodeClaim list is unchanged** — no node provisioned or absorbed there.

**Cleanup.** `kubectl delete -f scenarios/nodepool-b.yaml`.

### Scenario G — PDB-respected voluntary drain

**Goal.** The old node drains through Karpenter's **voluntary** (PDB-respecting)
termination path, not a forceful one (spec §3.3, #77 P0).

**Preconditions.** [§3](#3-shared-setup). Replace the stateless workload with a
2-replica Deployment + a **blocking** PDB:

```bash
kubectl delete -f scenarios/workload.yaml --ignore-not-found
kubectl apply -f scenarios/pdb-workload.yaml          # 2 replicas + PDB minAvailable=2
kubectl wait --for=condition=Ready pod -l app=poc-pdb --timeout=5m
```

**Run.** Trigger; once the candidate is `draining` (surge ready, old NodeClaim
delete issued), watch the replicas, then relax the PDB:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
# observe: both replicas stay Running on the OLD node, old NodeClaim lingers — eviction is blocked by the PDB
kubectl patch pdb poc-pdb -n default --type merge -p '{"spec":{"minAvailable":1}}'
# now the drain proceeds one replica at a time; old NodeClaim deletes; rotation completes
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
```

**Expected.** With `minAvailable=2` the drain **stalls** — both replicas pinned to
the old node, old NodeClaim lingering (a forceful path would evict immediately).
After relaxing to `minAvailable=1`, replicas migrate to the surge node one at a
time and the rotation completes. (The stall is bounded by the pool's
`terminationGracePeriod=5m`, after which Karpenter force-terminates — relax the PDB
before then.)

**Cleanup.** `kubectl delete -f scenarios/pdb-workload.yaml`.

### Scenario H — `do-not-disrupt` applied to both nodes during the surge

**Goal.** During the surge the controller protects **both** the old and new nodes
with `karpenter.sh/do-not-disrupt` (blocking voluntary Consolidation/Drift), tagged
with its ownership marker, and removes it on completion (spec §3.3, #77 P0).

**Preconditions.** [§3](#3-shared-setup), one Ready node, fresh candidate.

**Run.** Trigger and poll both nodes' annotations during the rotation (escape the
dots in the key — `['karpenter\.sh/do-not-disrupt']`):

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
# during pending/draining, on BOTH the candidate node and the surge host:
kubectl get node <node> -o jsonpath="{.metadata.annotations['karpenter\.sh/do-not-disrupt']} {.metadata.annotations['noderotation\.io/do-not-disrupt-owned']}"
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
```

**Expected.** Both nodes show `do-not-disrupt=true` + `do-not-disrupt-owned=true` +
`surge-for=<candidate>` while the surge is in flight; after completion the
unfreeze removes all three. (That `do-not-disrupt` does **not** block `expireAfter`
is documented Karpenter behavior, not tested here — see spec §3.3.)

The controller also emits actionable **Warning Events** on the NodePool/NodeClaim
(`AVeryAggressive`, `ThroughputBelowArrival`, `ShortLead`) — check with
`kubectl get events --field-selector involvedObject.name=nrc-poc`.

### Scenario I — scaled R6 soak (lead time keeps winning)

**Goal.** Over several consecutive rotations the controller turns every node over
at `ageThreshold` (~5m), so none ever approaches `expireAfter` — no `expired`
outcome, `short_lead_nodes` stays 0 (R6, #93 / #77).

**Preconditions.** [§3](#3-shared-setup), stateless workload.

**Run.** Leave the pool in scope and let it rotate; count completions:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
# watch noderotation.io/last-rotation-at change N times (~6 min/cycle), then:
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
```

**Expected.** `noderotation_completed_total{outcome="success"}` climbs by N;
`{outcome="expired"}` and `{outcome="failure"}` do **not** change;
`noderotation_short_lead_nodes` stays 0 throughout — the ageThreshold rotation
fires far inside the 336h backstop every cycle.

---

### Scenario J — capacity-absorb (bin-packed) path

**Goal.** When same-pool spare capacity already exists, the placeholder
**bin-packs onto it** instead of inducing a new node — the §3.3 *capacity-absorb
path*. The drain is just as safe (the spare's headroom is physically reserved)
but no replacement node is launched. Scenarios 0/A–I all took the *new-provision*
path; this is the other §3.3 outcome.

**Preconditions.** [§3](#3-shared-setup) (controller installed). Uses
`scenarios/workload-absorb.yaml` (two pods, **same AZ** — set the manifest's
`topology.kubernetes.io/zone` to one of `terraform output availability_zones`).
The pool starts **out of scope** so you can stage both nodes before any rotation.
Read the **Caution** below first — the obvious setup silently new-provisions.

**Run.** Stage the candidate first (so it is the oldest, hence the sole
`>ageThreshold` candidate), then the spare last (so it stays *young* — below
`ageThreshold`, so not itself a candidate):

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-          # halt while staging

# 1. candidate first -> own 2-vCPU node
kubectl apply -f scenarios/workload-absorb.yaml -l app=poc-absorb-cand
kubectl wait --for=condition=Ready pod -l app=poc-absorb-cand --timeout=5m
#    wait >5m so it crosses ageThreshold, and any extra empty node consolidates
#    away (until `kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc` == 1)

# 2. spare last, stays young. 1800m forces its own 4-vCPU node (same AZ) with room
kubectl apply -f scenarios/workload-absorb.yaml                    # adds the spare
kubectl wait --for=condition=Ready pod -l app=poc-absorb-spare --timeout=5m

# 3. enable rotation; the candidate rotates and the placeholder absorbs the spare
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
```

Watch the placeholder bind and the claim count hold:

```bash
CAND=$(kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort | head -1)  # oldest = candidate
kubectl get pod -n node-rotation-system "noderotation-surge-$CAND" -o wide -w     # binds to the SPARE node
kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc -w                        # stays at 2 (never 3)
```

**Expected.** The placeholder is `Successfully assigned` straight to the
**pre-existing spare node** (its `describe` shows **no `FailedScheduling`**), and
**no new NodeClaim appears** — the pool's claim count holds at **2** for the whole
rotation (a new-provision would spike it to 3). The `surge-for` marker lands on
the **spare** (the surge target is pre-existing). After the drain the candidate
pod re-lands on the spare's reserved headroom and the old NodeClaim is deleted,
collapsing the pool to **one** node.

```bash
# the bound host == the spare, marked as the surge target:
kubectl get nodes -l noderotation-poc/pool=poc -o json \
  | jq -r '.items[]|select(.metadata.annotations["noderotation.io/surge-for"])|.metadata.name'
# claim count never exceeded 2 (new-provision would have shown 3):
kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc --no-headers | wc -l
```

```text
noderotation_completed_total{nodepool="nrc-poc",outcome="success"} +1
noderotation_in_progress{nodepool="nrc-poc"} 0
```

> **Caution — two traps that silently force the *new-provision* path instead:**
>
> 1. **The spare must be young (below `ageThreshold`).** A spare that is itself a
>    candidate is "near-deadline", so the placeholder's soft hostname exclusion
>    (§3.3, issue #96) lowers its score; `kube-scheduler` hesitates and Karpenter
>    wins the provision race, so a new node appears. Stage the candidate first,
>    wait out `ageThreshold`, then add the spare just before enabling rotation.
> 2. **No pod anti-affinity between the candidate and spare workloads.**
>    Anti-affinity is symmetric: the spare pod's "not with the candidate" term
>    blocks the *drained* candidate pod from re-landing on the spare (the §3.3
>    per-pod-placement disclaimer), so Karpenter provisions a node for it.
>    Separate the two nodes by **sizing** instead (the manifest's 1800m spare
>    can't fit the candidate's 2-vCPU node, so it gets its own 4-vCPU node).
>
> Both nodes must also be **same-AZ** (the placeholder copies the candidate's zone
> as a *required* term); `scenarios/workload-absorb.yaml` pins the zone for this.

**Cleanup.**

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
kubectl delete -f scenarios/workload-absorb.yaml
```

---

### Scenario K — leader-change resume

**Goal.** Killing the leader mid-rotation does not restart or lose the rotation:
a new leader resumes it **purely from annotations** (§5.1, durable state on the
NodeClaim).

**Preconditions.** [§3](#3-shared-setup), the stateless `workload.yaml` (one
`nrc-poc` node, candidate `>ageThreshold`).

**Run.** Enable rotation, wait until the candidate is `state=pending` with a
`surge-claim` stamped, then delete the leader Pod:

```bash
CAND=$(kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc -o jsonpath='{.items[0].metadata.name}')
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
# wait until: kubectl get nodeclaim $CAND -o jsonpath='{.metadata.annotations.noderotation\.io/state}' == pending
#        and  ...noderotation\.io/surge-claim is non-empty; note both it and started-at
kubectl -n node-rotation-system delete pod -l app.kubernetes.io/name=node-rotation-controller
```

**Expected.** A new replica takes the Lease
(`kubectl -n node-rotation-system get lease node-rotation-controller.noderotation.io`
holder changes) and continues the **same** rotation — `surge-claim` and
`started-at` are unchanged — advancing `pending → draining → complete`. The
candidate NodeClaim is rotated away; `noderotation_completed_total{outcome="success"}`
increments. Nothing restarts from scratch.

**Cleanup.** `kubectl label nodepool nrc-poc noderotation-poc/in-scope-`

---

### Scenario L — window boundary (in-flight completes, no new start)

**Goal.** A rotation already in-flight **completes past the window boundary**;
once the window closes, **no new rotation starts** even with an eligible
candidate (§3.1).

**Preconditions.** [§3](#3-shared-setup), the stateless `workload.yaml` scaled to
**two** replicas so two `nrc-poc` nodes exist (`kubectl scale deploy poc-workload
--replicas=2`), both `>ageThreshold`.

**Run.** The boundary is made deterministic by closing the window *live* while a
rotation is in-flight (rather than racing a wall-clock minute). The RotationPolicy
is watched, so the patch closes the window with no restart, and the in-flight
rotation (its state on annotations) keeps going:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite
# wait until the oldest candidate is state=pending (rotation 1 in-flight)
# close the window live: end 23:59 -> 00:01 (now is outside [00:00,00:01])
kubectl patch rotationpolicy nrc-poc --type merge \
  -p '{"spec":{"maintenanceWindows":[{"timezone":"UTC","days":["Mon","Tue","Wed","Thu","Fri","Sat","Sun"],"start":"00:00","end":"00:01"}]}}'
```

**Expected.** Despite the window now being **closed**, rotation 1 **still
completes** — the in-flight rotation finishes past the boundary. Rotation 2 (the
second candidate) does **not** start:

```text
noderotation_window_active 0            # window closed
noderotation_candidates{nodepool="nrc-poc"} 1   # second candidate still eligible
noderotation_in_progress{nodepool="nrc-poc"} 0  # but no new rotation starts
```

**Cleanup.** Drop the in-scope label **first**, *then* restore the open window —
order matters here. Restoring the window while the pool is still in-scope re-opens
a governed window and can start a second rotation; dropping the label immediately
after would then take the pool out of governance mid-rotation. The controller now
reaps that orphan (rolls the in-flight rotation back, issue #141), but the clean
teardown is to take the pool out of scope before touching any other knob, so no
governed window is ever re-opened during teardown:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-     # FIRST: out of scope before re-opening the window
kubectl patch rotationpolicy nrc-poc --type merge \
  -p '{"spec":{"maintenanceWindows":[{"timezone":"UTC","days":["Mon","Tue","Wed","Thu","Fri","Sat","Sun"],"start":"00:00","end":"23:59"}]}}'
kubectl scale deploy poc-workload --replicas=1
```

---

### Scenario M — placeholder preemption (victim + `readyTimeout` rollback)

**Goal.** Two facets of §3.3 *Placeholder priority* / *Rollback*, each with its own
setup because they cannot be observed in the same run:

- **Part A — preemption victim.** The placeholder (negative priority
  `noderotation-placeholder` = -10, `preemptionPolicy: Never`, a bare Pod with no
  ReplicaSet) is the deliberate preemption **victim**: a higher-priority Pod
  evicts it, and it never preempts anything itself.
- **Part B — `readyTimeout`-bounded rollback.** Under sustained higher-priority
  pressure the placeholder can never complete the surge, so the rotation
  self-terminates into a **clean rollback** rather than churning forever.

> Why two setups: to *preempt* a placeholder it must be **Running** (Part A injects
> a preemptor once it is). But a placeholder that is **Running** has already passed
> `surge_ready`, so it cannot also demonstrate the *pending→rollback* path — Part B
> keeps it `Pending` from the start instead. The strict single chain
> *preempt → recreate → readyTimeout* has a narrow window on real EKS too
> (absorb/bind → `surge_ready` is fast), as under KWOK (#95 item 2); the idempotent
> recreate-on-missing is pinned by envtest.

#### Part A — preemption victim

**Preconditions.** [§3](#3-shared-setup), pool **out of scope**. Stage the
capacity-absorb pair from [Scenario J](#scenario-j--capacity-absorb-bin-packed-path)
(candidate 250m on a 2-vCPU node `>ageThreshold`; a *young* 1800m spare on its own
4-vCPU node, same AZ) so the placeholder will be **Running** on the spare.

**Run.** Enable rotation, wait until the placeholder is **Running** on the spare,
then inject the preemptor:

```bash
kubectl apply -f scenarios/workload-absorb.yaml -l app=poc-absorb-cand   # candidate first
# wait >5m (candidate crosses ageThreshold) and consolidation down to ONE nrc-poc node, then:
kubectl apply -f scenarios/workload-absorb.yaml                          # adds the young spare
kubectl wait --for=condition=Ready pod -l app=poc-absorb-spare --timeout=5m
kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite

CAND=$(kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' | sort | head -1)
# wait until pod noderotation-surge-$CAND is Running on the SPARE node, then preempt it:
kubectl apply -f scenarios/preemption.yaml -l app=poc-preemptor   # 1900m high-priority Pod
```

**Expected.** The preemptor evicts the placeholder from the spare — the placeholder
is the victim and never the preemptor:

```text
kubectl get events -n node-rotation-system | grep "noderotation-surge-$CAND"
#   Normal  Preempted        ... Preempted by pod <poc-preemptor-uid> on node <spare>
#   Warning FailedScheduling ... preemption: not eligible due to preemptionPolicy=Never
```

The `Preempted` event confirms the victim role; `not eligible due to
preemptionPolicy=Never` confirms the placeholder never preempts others to make
room (so it never re-pends by evicting anything); being a bare Pod, only the
controller would recreate it. (If the drain was already underway when the preempt
landed, the rotation still completes — a drain-phase preempt is harmless.)

**Cleanup.**

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
kubectl delete -f scenarios/preemption.yaml --ignore-not-found
kubectl delete -f scenarios/workload-absorb.yaml --ignore-not-found
```

#### Part B — `readyTimeout`-bounded rollback

**Preconditions.** [§3](#3-shared-setup), pool **out of scope**. Stage a small
candidate, a high-priority blocker holding the only same-AZ spare, new nodes
blocked by `limits`, and a short `readyTimeout`:

```bash
kubectl apply -f scenarios/workload-absorb.yaml -l app=poc-absorb-cand   # candidate (250m), >ageThreshold
# shorten readyTimeout so the rollback is quick (e.g. 120s) — live patch, no restart
kubectl patch rotationpolicy nrc-poc --type merge \
  -p '{"spec":{"surge":{"readyTimeout":"120s"}}}'
# blocker (high priority, anti-affinity to the candidate) claims the only spare 4-vCPU node
kubectl apply -f scenarios/preemption.yaml -l app=poc-blocker
kubectl wait --for=condition=Ready pod poc-blocker --timeout=5m
# block new nodes: leave headroom for the 250m placeholder pre-check but not a whole node
#   provisioned = cand 2 + blocker 4 = 6; a new 2-vCPU node would be 8
kubectl patch nodepool nrc-poc --type merge -p '{"spec":{"limits":{"cpu":"7"}}}'
```

**Run.** `kubectl label nodepool nrc-poc noderotation-poc/in-scope=true --overwrite`

**Expected.** The placeholder cannot bind (candidate node cordoned, blocker node
full, new node blocked by `limits`) and **cannot preempt the blocker** (it is
negative-priority + `Never`), so it stays `Pending` until `readyTimeout` fires a
clean rollback:

```text
noderotation_completed_total{nodepool="nrc-poc",outcome="failure"} +1
noderotation_retry_count{nodepool="nrc-poc"} 1
```

The candidate is **retained** (not rotated), `state=failed`, its node
**un-cordoned**, and the induced surge claim reaped.

**Cleanup.**

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
kubectl delete -f scenarios/preemption.yaml --ignore-not-found
kubectl delete -f scenarios/workload-absorb.yaml --ignore-not-found
kubectl patch nodepool nrc-poc --type merge -p '{"spec":{"limits":{"cpu":"16"}}}'
# restore readyTimeout (live patch) and clear the failed marker (see Gotchas)
kubectl patch rotationpolicy nrc-poc --type merge \
  -p '{"spec":{"surge":{"readyTimeout":"15m"}}}'
```

---

### Scenario N — `do-not-disrupt` honored against voluntary disruption

**Goal.** Karpenter **honors** `karpenter.sh/do-not-disrupt=true` — the value the
controller applies to both surge nodes (Scenario H) — against **voluntary**
disruption: no node is disrupted while the annotation is set (§3.3, #95). (That it
does *not* block `expireAfter` is documented Karpenter behavior, not retested.)

**Preconditions.** [§3](#3-shared-setup), the stateless `workload.yaml` (one
`nrc-poc` node), pool **out of scope** (this exercises Karpenter's Drift, not the
controller's rotation).

**Run.** First take the pool **out of scope** — the shared NodePool ships the
`noderotation-poc/in-scope` label (it is in scope by default, see [§2](#2-how-the-scenarios-work)),
so without this the controller could rotate the very node this scenario is testing
and contaminate the Drift result. Then put `do-not-disrupt` on the node and induce
Drift by changing the NodePool `spec.template`:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-      # stop the controller rotating it
kubectl get nodepool nrc-poc -o jsonpath='active-rotation=[{.metadata.annotations.noderotation\.io/active-rotation-state}]{"\n"}'  # verify: empty (no in-flight rotation)
NODE=$(kubectl get nodes -l noderotation-poc/pool=poc -o jsonpath='{.items[0].metadata.name}')
kubectl annotate node "$NODE" karpenter.sh/do-not-disrupt=true --overwrite
kubectl patch nodepool nrc-poc --type merge -p '{"spec":{"template":{"metadata":{"labels":{"poc-drift":"v1"}}}}}'
```

**Expected.** The node goes `Drifted=True` but is **not** replaced while the
annotation is set (watch for several minutes — no new NodeClaim, claim count
unchanged). Removing the annotation lets Karpenter immediately drift-replace it
(make-before-break — a replacement claim appears, then the old one is deleted):

```bash
kubectl get nodeclaim "$(kubectl get nodeclaims -l karpenter.sh/nodepool=nrc-poc -o jsonpath='{.items[0].metadata.name}')" \
  -o jsonpath='Drifted={.status.conditions[?(@.type=="Drifted")].status}{"\n"}'   # True, yet not replaced
kubectl annotate node "$NODE" karpenter.sh/do-not-disrupt-     # now it gets drift-replaced
```

**Cleanup.** Revert the template change:

```bash
kubectl patch nodepool nrc-poc --type json -p '[{"op":"remove","path":"/spec/template/metadata/labels/poc-drift"}]'
```

---

### Scenario O — trick-free forceful fallback (production-like mix)

Validates the opt-in window-bounded **surge-less forceful fallback** (spec §3.3,
ADR-0001; #156) end-to-end on real EKS, **without** the KWOK immutable-spec
expireAfter-raise trick: `nodepool-ff` keeps a **fixed** `expireAfter: 2h`. A
12-replica PDB-backed workload lands one pod per node (a synchronized batch of 12
nodes sharing one deadline). Because `N=12 > K·C=6` the serial surge cannot
rotate all of them gracefully within the `K·P=1h` grace, so the surplus is
rotated **surge-less**. This one scenario also exercises earliest-deadline
ordering (#157) and do-not-disrupt exclusion (#170).

Firing math (K=2, `t_rot = readyTimeout 5m + tGP 5m + Buffer 2m = 12m`,
`t_rot_est = provisioningEstimate 5m + drainEstimate 5m = 10m`, `P=30m`,
`WindowLen=28m`, `cooldownAfter=2m` → `C = ceil(28m / (10m + 2m)) = 3`, `E=2h`):
candidate age `A = E − (K·P + t_rot) = 48m`, forceful-fire age `E − t_rot = 1h48m`,
grace `K·P=1h`. `Buffer` is deadline-side only: shrinking it `15m → 2m` (ADR-0002,
#215) moved `t_rot`, `A` and the forceful-fire age, but **not** `C` — the layer-2
throughput forecast is budgeted on `t_rot_est`, which carries no buffer (ADR-0003,
#220). Expect the schedule to emit the intentional `ThroughputBurstShortfall`
(`N=12 > K·C=6`), `ThroughputBelowArrival`, and `RotationSpansNextWindow`
(`t_rot_est 10m + cooldown 2m = 12m` exceeds the 2m the window stays closed between
occurrences) **warn** findings — these predict the surge-less path and are not
errors.

**Setup (from Shared setup §3, controller installed):**

```bash
cd test/e2e/eks-automode
bash scenarios/gen-ff-windows.sh                 # (re)generate nrc-ff-policy.yaml
kubectl apply -f scenarios/nodepool-ff.yaml
kubectl apply -f scenarios/nrc-ff-policy.yaml
kubectl apply -f scenarios/ff-workload.yaml
# wait for the 12-node synchronized batch to be Ready
kubectl get nodeclaim -l karpenter.sh/nodepool=nodepool-ff -w
```

**Observe the mix (over ~1.5–2h):**

```bash
# forceful-fallback counter climbs as the surplus fires surge-less
kubectl port-forward -n node-rotation-system svc/node-rotation-controller-metrics 8080:8080 &
curl -s localhost:8080/metrics | grep noderotation_forceful_fallback_total

# the anchor records the surge-less mode while a forceful rotation is in flight
kubectl get nodepool nodepool-ff -o jsonpath='{.metadata.annotations.noderotation\.io/rotation-mode}{"\n"}'
# expect: forceful-fallback (absent = default surge)

# Warning Events: ForcefulFallback on the NodePool
kubectl get events --field-selector reason=ForcefulFallback -A

# surge-less proof: watch ALL placeholders by the surge-for marker — a graceful
# candidate's placeholder (noderotation-surge-<candidate>) appears here, but a
# forceful candidate's never does
kubectl get pods -n node-rotation-system -l noderotation.io/surge-for -w
# or confirm a specific forceful candidate never got one:
kubectl get pod -n node-rotation-system noderotation-surge-<candidate>   # → NotFound
```

Expected: some early candidates rotate **gracefully** (a placeholder Pod appears,
new node joins, old drains) and the later surplus rotates **surge-less**
(`rotation-mode=forceful-fallback`, `ForcefulFallback` event,
`noderotation_forceful_fallback_total` increments, **no placeholder Pod** for
those) — a mix in one pool.

> **Note.** Forceful fallback is still **serial** per NodePool
> (`surge.maxUnavailable = 1`). If the serial cadence cannot clear all ~6 surplus
> nodes before their fixed 2h `expireAfter`, the latest few may surface as
> Karpenter's native `expired` backstop outcome rather than `ForcefulFallback` —
> this is expected, not a failure; the forceful-fallback behavior is proven by the
> first surplus node firing surge-less at age 1h48m. Note the forceful-fire window
> is only `t_rot` wide (`E − t_rot` to `E` = 1h48m to 2h = **12m**), narrower than
> under the old 15m Buffer (25m), so the surge-less surplus must clear faster to
> beat the backstop.

**If forceful fallback does not appear** (serial surge kept up), tune LIVE (E
stays fixed):

```bash
# slow the serial cadence so the surplus ages past 1h48m before its turn
kubectl patch rotationpolicy nrc-ff --type merge -p '{"spec":{"surge":{"cooldownAfter":"10m"}}}'
# or raise the batch size
kubectl scale deploy/ff-workload --replicas=16
# or if workload pods stay Pending (node allocatable < 2000m after system-reserved + DaemonSets):
kubectl set resources deploy/ff-workload --requests=cpu=1000m  # or edit scenarios/ff-workload.yaml
```

**#157 earliest-deadline order:** the 12 claims share one deadline, so the pick
order is the `creationTimestamp → name` tiebreak. Confirm the rotated NodeClaims
are consumed in ascending `(creationTimestamp, name)`:

```bash
kubectl get nodeclaim -l karpenter.sh/nodepool=nodepool-ff \
  -o custom-columns=NAME:.metadata.name,CREATED:.metadata.creationTimestamp,STATE:'.metadata.annotations.noderotation\.io/state' --sort-by=.metadata.name
```

**#170 do-not-disrupt exclusion:** mark one node's Node object and confirm it is
never chosen. Pick a NodeClaim that is **not in-flight** (empty
`noderotation.io/state`) — a mid-rotation node already carries the controller's own
`do-not-disrupt`, so annotating it would be a no-op and the assertion unreliable:

```bash
# select a NodeClaim with no rotation state, then annotate its Node
claim=$(kubectl get nodeclaim -l karpenter.sh/nodepool=nodepool-ff \
  -o jsonpath='{range .items[?(@.metadata.annotations.noderotation\.io/state=="")]}{.metadata.name} {.status.nodeName}{"\n"}{end}' | head -1)
node=$(echo "$claim" | awk '{print $2}')
kubectl annotate node "$node" karpenter.sh/do-not-disrupt=true
# the candidates gauge drops by one; that node keeps its original NodeClaim
curl -s localhost:8080/metrics | grep 'noderotation_candidates{.*nodepool="nodepool-ff"'
```

**Teardown of the scenario objects (cluster stays up for other scenarios):**

```bash
kubectl delete -f scenarios/ff-workload.yaml -f scenarios/nrc-ff-policy.yaml -f scenarios/nodepool-ff.yaml
```

---

### Scenario P — tight-race `expireAfter` soak (12h, issue #118)

Every other scenario proves a mechanism in minutes. This one proves the spec §3.2
guarantee **holds over time**: with a sub-daily maintenance-window schedule and an
`expireAfter` small enough that Karpenter's own forceful expiration genuinely
races the controller's lead time, every rotation on a steady 5-node pool
completes gracefully before its deadline for 12 unattended hours —
`outcome="expired"` stays **0** — and, with `surge.forcefulFallback` **armed**,
the fallback never needs to fire on that pool (quiescence). A short, attended
**epilogue** then uses a separate, frozen, single-node mini-pool to
deterministically drive one claim across the fallback boundary and prove the
same controller/config takes the surge-less branch when the surge genuinely
cannot fit — the mechanics Scenario O already validated under a synchronized
12-node batch, here isolated to one bounded claim. This is a **verification-only**
run: no controller behavior changes; any anomaly becomes its own finding, as
#224/#236/#244 did in past runs.

Everything below runs against dedicated pools/policies/workloads (`scenarios/nodepool-soak.yaml`,
`nrc-soak-policy.yaml`, `workload-soak.yaml` for the main soak;
`nodepool-soak-epi.yaml`, `nrc-soak-epi-policy.yaml`, `workload-soak-epi.yaml` for
the epilogue) — [§3 Shared setup](#3-shared-setup)'s `nrc-poc` pool stays out of
scope throughout and is unaffected.

#### Derivation

| Knob | Value | Note |
|---|---|---|
| Windows | 48/day: every 30m, each 28m (`P=30m`, `D=28m`), all days, UTC | generated by `scenarios/gen-soak-windows.sh` |
| `minRotationChances` (K) | 2 | spec-recommended floor |
| `surge.readyTimeout` | 5m | |
| `surge.cooldownAfter` | 2m | also gates the *initial* rotation — expect a 2m start delay |
| `surge.retryBackoff` | 30m | long, so a failed claim doesn't churn mid-observation |
| `surge.forcefulFallback.enabled` | **true** | armed; PASS requires it never fires on the main pool |
| NodePool `terminationGracePeriod` (tGP) | 5m | bounds drain |
| NodePool `expireAfter` (E) | **2h12m** | set at pool creation, **never patched** (Karpenter drift) |
| NodePool `limits.cpu` | main **16**, epi **8** | steady 5×2 + surge; epi 1×2 + replacement overlap |
| NodePool disruption | `WhenEmpty` / `60s`; budgets block `Underutilized`/`Drifted` | only the controller turns nodes over |
| Fleet (N) | main **5**, epi **1** | 1.5-cpu pod per 2-vCPU node, no anti-affinity |
| Controller | `replicaCount: 1`, pinned off both soak pools, `priorityClassName: system-cluster-critical` | one metrics stream; a restart resets counters *and* breaks the `restartCount==0` criterion |

Derived (code-backed, `internal/schedule/schedule.go`, pinned by
`TestDeriveScenarioPSoak`):

```text
Buffer    = 2m (fixed, #215)
t_rot     = readyTimeout + tGP + Buffer      = 5m + 5m + 2m  = 12m
t_rot_est = provisioningEstimate + drainEstimate = 5m + 5m   = 10m
leadTime  = K·P + t_rot                      = 2·30m + 12m   = 1h12m
A         = E − leadTime                     = 2h12m − 1h12m = 1h     (ageThreshold: auto)
C         = ceil(D / (t_rot_est + cooldownAfter)) = ceil(28m/12m)     = 3 (per window, 6/h)
G         = K                                                         = 2
fallback drain bound = tGP + Buffer          = 5m + 2m        = 7m    (no readyTimeout/provisioning on the surge-less path)
```

**Expected findings — exactly `{RotationSpansNextWindow}` (warn), nothing else.**
The 2m idle gap between window occurrences is shorter than
`t_rot_est + cooldownAfter = 12m`, so this warn is structurally unavoidable at
any `C=3, P=30m` shape (Scenario O logged the same warn for the same timings);
it is a forecast-honesty note ("K·C is an upper bound"), not a defect.
`ThroughputBelowArrival` must **not** fire (`C·A = 180m ≥ N·P = 150m`, 83%
forecast utilization — a deliberate 17% headroom) and `ThroughputBurstShortfall`
must **not** fire (`N=5 ≤ K·C=6`). Confirm this with the wasm policy
simulator against both generated RotationPolicy YAMLs before spending a cent.

#### Setup

Run this on its own dedicated cluster (a fresh `terraform apply`, [§3.2](#32-install-the-controller)'s
install step, with `nrc-poc` and any other scenario left untouched) so the 12h
run isn't sharing capacity or a controller pod with anything else. Install the
controller from the same `scenarios/controller-values.yaml` overlay [§3.2](#32-install-the-controller)
uses — it already carries the off-pool affinity, `replicaCount: 1`,
`priorityClassName: system-cluster-critical` (the latter closes #270's
priority-0 preemption hole, load-bearing for the unattended 12h
`restartCount==0` criterion), and production **JSON logging**
(`logging.development: false`) so the `rotation complete` lines in
`$run/controller.log` stay machine-parseable evidence — `soak-analyze.py`
builds the per-rotation ledger from their `msg`/`ts` JSON fields, which the
`--zap-devel` console format does not carry:

```bash
helm install node-rotation-controller ../../../charts/node-rotation-controller \
  --namespace node-rotation-system --create-namespace \
  -f scenarios/controller-values.yaml \
  --set image.repository="$REPO" --set image.tag=poc \
  --wait --timeout 8m
```

> If admission rejects `system-cluster-critical` outside `kube-system` (cluster
> policy varies), create a dedicated high-value PriorityClass instead and point
> the overlay at it:
> ```bash
> kubectl apply -f - <<'EOF'
> apiVersion: scheduling.k8s.io/v1
> kind: PriorityClass
> metadata:
>   name: soak-controller-critical
> value: 100000
> globalDefault: false
> description: Scenario P (#118) — keeps the controller off the node-pressure eviction path for the unattended 12h run.
> EOF
> helm upgrade node-rotation-controller ../../../charts/node-rotation-controller \
>   --namespace node-rotation-system \
>   -f scenarios/controller-values.yaml \
>   --set image.repository="$REPO" --set image.tag=poc \
>   --set priorityClassName=soak-controller-critical --wait --timeout 8m
> ```

Apply the main pool, policy, and workload (the epilogue trio is applied later,
attended, in [§Epilogue](#epilogue)):

```bash
kubectl apply -f scenarios/nodepool-soak.yaml
kubectl apply -f scenarios/nrc-soak-policy.yaml     # generated; re-run scenarios/gen-soak-windows.sh first if stale
kubectl apply -f scenarios/workload-soak.yaml       # ships replicas: 0 — the ramp below scales it up
```

Start the in-cluster scraper (the recorder of record) and the local recorder
(secondary evidence; survives the scraper being lost, and vice versa):

```bash
kubectl apply -f scenarios/soak-scraper.yaml
run=soak-$(date -u +%Y%m%dT%H%M%SZ); mkdir -p "$run"
nohup scenarios/soak-record.sh "$run" > "$run/record.out" 2>&1 &
nohup scenarios/soak-watchdog.sh "$run" nodepool-soak > "$run/watchdog.out" 2>&1 &
```

**Isolation assert** (before T0 — the two pools/policies use mutually
exclusive dedicated labels, and each workload's `nodeSelector` names its exact
pool template label; confirm before trusting any downstream metric):

```bash
kubectl get rotationpolicy nrc-soak -o jsonpath='{.status.matchedNodePools}{"\n"}'   # 1
kubectl port-forward -n node-rotation-system svc/node-rotation-controller-metrics 8080:8080 &
curl -s localhost:8080/metrics | grep 'noderotation_policy_conflict'                 # nodepool="nodepool-soak" 0
```

(Repeat the `matchedNodePools` and `policy_conflict` checks for `nrc-soak-epi` /
`nodepool-soak-epi` once the epilogue trio is applied — same assertions, same
values.) `workload-soak.yaml` ships `replicas: 0`, so the third assert —
workload pods landing only on their own pool — has nothing to check yet; it
runs once the ramp below has scaled past 0.

#### Ramp and T0

Scale the workload one replica every 12m — this spreads birth phases across
the `A=1h` cycle instead of a synchronized batch, and doubles as the pre-flight
shakedown of the recorders and the first rotation:

```bash
for i in 1 2 3 4 5; do
  kubectl scale deployment/soak-workload --replicas=$i
  sleep 720
done
```

Once the first replica is `Ready`, close out the deferred third isolation
assert — its pod must land only on `nodepool-soak`:

```bash
kubectl get pods -n default -o wide -l app=soak-workload   # NODE column: only nodepool-soak nodes
```

Watch the first rotation end-to-end (`kubectl logs -n node-rotation-system deploy/node-rotation-controller -f`,
or tail `$run/controller.log`) before declaring **T0**:

> **T0** = the instant all 5 replicas are `Ready` on 5 distinct `nodepool-soak`
> nodes **and** the first rotation has completed with `outcome="success"`. The
> 12h observation clock starts here, not at the ramp's wall-clock start.

Steady-state births re-align to the 30m window grid regardless of the ramp's
schedule (completions happen in-window); that is the natural rhythm, not a
defect. Expected node lifetime ≈ 65–95m → ~8–10 rotations per slot → **~45–55
rotations** in 12h, essentially all via the new-provision path (a 1.5-cpu pod
cannot absorb onto a peer already holding another 1.5-cpu pod on a 2-vCPU node).

#### 12h observation

Let the pool rotate unattended; the watchdog pages on any anomaly. PASS
requires all 13 criteria (1–10 govern the main soak, 11–13 the epilogue):

1. `noderotation_completed_total{outcome="expired"}` == 0 — **the headline**.
2. `outcome="success"` climbs steadily, ≈5/h, total ≥ 40.
3. `noderotation_forceful_fallback_total` == 0 on `nodepool-soak` — armed but
   never needed.
4. `noderotation_short_lead_nodes` == 0 at every scrape.
5. Controller pod `restartCount` == 0 (counters therefore monotone), and the
   scraper's `seq` is contiguous (no recorder gap > 5m).
6. **Load presence at every snapshot**: deployment desired=available=ready=5;
   all 5 pods `Running` on `Ready` main-pool nodes; exactly 5 workload-bearing
   claims (±1 documented transient surge claim); no pod `Pending` beyond a
   rotation's documented drain window.
7. **Config-under-test presence**, by evidence source:
   - at every metrics scrape: `noderotation_age_threshold_seconds=3600`,
     `noderotation_window_period_seconds=1800`, `noderotation_t_rot_bound_seconds=720`,
     `noderotation_t_rot_estimate_seconds=600`, `noderotation_throughput_capacity=3`,
     `noderotation_rotation_chances=2` (all `{nodepool="nodepool-soak"}`);
   - at every 2m object snapshot: both policies `Ready=True reason=Accepted`,
     `observedGeneration` current, `matchedNodePools=1`, `noderotation_policy_conflict=0`;
   - once per configuration generation: findings exactly
     `{RotationSpansNextWindow}` (simulator preflight + controller Event/log
     evidence), and **no unexpected finding code** appears anywhere in the
     run's logs/Events.
8. Per-rotation **margin** = (old claim's `creationTimestamp + 2h12m`) −
   (rotation completion) is **> 0 for every rotation**; report min/median/
   distribution.
9. **End census** (see [§T_end census](#t_end-census-and-tail-follow)): every
   claim ≥ A at T_end rotated with margin > 0 within `T_end + P + t_rot`;
   remaining claims are all younger than A (right-censored, documented); no
   claim in `state=failed`, no stale anchor, no orphan placeholder.
10. No unexpected Karpenter disruption on the main pool (no
    Expiration/Drift/Underutilized events; `Empty` only for vacated surge
    leftovers).

`outcome="failure"` (rollback) ⇒ **qualified PASS at best**: each failed
attempt must be followed by a successful rotation of the *same* claim before
its deadline, with fallback and expired still 0 — `K` supplies schedule
opportunities, not a guaranteed retry success, and `retryBackoff=30m` can
consume a chance. Zero failures ⇒ clean PASS; any failure is also examined as
a potential finding in its own right.

Reading the authoritative signal at any point:

```bash
curl -s localhost:8080/metrics | grep -E '^noderotation_(completed_total|forceful_fallback_total|short_lead_nodes|candidates|in_progress)'
```

#### T_end census and tail-follow

At `T0 + 12h` (**T_end**), snapshot state and apply the **tail-follow** rule
rather than stopping cold: every claim already `≥ A` at T_end must still
rotate, with margin > 0, by `T_end + P + t_rot = T_end + 42m`. Wait up to
**~45m** past T_end for that tail before moving on; claims younger than `A` at
T_end are right-censored (never became candidates in-window) and are
documented as such, not counted against PASS.

```bash
kubectl get nodeclaims -l karpenter.sh/nodepool=nodepool-soak \
  -o custom-columns=NAME:.metadata.name,CREATED:.metadata.creationTimestamp,STATE:'.metadata.annotations.noderotation\.io/state'
```

#### Epilogue

Why a separate mini-pool: the fallback is serial per pool, and each old-claim
finalization may legally consume up to the fallback drain bound
(`tGP + Buffer = 7m`) — bounding *one* release is deterministic, bounding three
inside a 12m band (Scenario O's shape) is only empirically likely. One claim,
released with more than 7m of deadline left, forces fallback initiation
(graceful cannot fit: `deadline − now < t_rot`) and its completion fits the
drain bound.

Apply the epilogue trio (the pool is created **with the freeze already set**,
Scenario E's mechanism — its single node ages frozen: it becomes a candidate,
but never starts):

```bash
kubectl apply -f scenarios/nodepool-soak-epi.yaml
kubectl apply -f scenarios/nrc-soak-epi-policy.yaml
kubectl apply -f scenarios/workload-soak-epi.yaml
kubectl scale deployment/soak-epi-workload --replicas=1
```

While frozen, confirm:

```text
noderotation_candidates{nodepool="nodepool-soak-epi"}    1
noderotation_in_progress{nodepool="nodepool-soak-epi"}   0
```

Let the node age ~2h, then release under `nohup` — the script computes the
release instant `R` by an interval search from the claim's **actual**
`creationTimestamp` (`d = creationTimestamp + E`; first in-window instant
`≥ d − 11m`, required `d − 12m < R < d − 8m`), waits for it, and removes the
freeze. It **fails closed**: if it cannot prove its chosen `R` satisfies those
bounds, or the release window is missed, it tears the epilogue down **while
still frozen** instead (exit code `3`) — never a late, unmanaged unfreeze:

```bash
nohup scenarios/soak-epi-release.sh > "$run/epi-release.out" 2>&1 &
```

**Expected, after release at R:**

11. While frozen (pre-`R`, restated): `noderotation_candidates{nodepool="nodepool-soak-epi"}=1`,
    `noderotation_in_progress{nodepool="nodepool-soak-epi"}=0`, no rotation
    starts, node untouched.
12. After release: `forceful_fallback_total{nodepool="nodepool-soak-epi"}`
    goes 0→**1**, in-window, with a `ForcefulFallback` Warning event
    (`kubectl get events --field-selector reason=ForcefulFallback`); the
    placeholder ledger (`$run/placeholders.log`, the `pods -w` stream on
    `noderotation.io/surge-for`) shows **no placeholder Pod ever existed** for
    the epi claim; the claim deletes via the voluntary path within the 7m
    drain bound; `expired` stays 0; a replacement node is provisioned for the
    rescheduled pod. **Main pool undisturbed, concretely:** during the
    epilogue the main pool logs no fallback/expired/failure, criteria 6–7
    keep holding, and no main-pool completion gap exceeds `P + t_rot = 42m`.
13. **Abort rule (missed release).** If the release script exits `3` (freeze
    not removed by `d − 8m`), do **not** unfreeze by hand — freezing never
    stopped Karpenter's own expiration controller, so a late release can
    neither guarantee fallback completion nor prevent a genuine `expired`. The
    script has already torn the epilogue down while frozen; mark the epilogue
    **inconclusive**, exclude that cleanup from all fallback/expired
    assertions, and optionally re-run on a fresh mini-pool (~2h to age into
    the band).

#### Harvest

Before tearing anything down, pull the scraper log — the recorder of record —
into the run directory (add `--previous` if `restartCount > 0` on the scraper
pod, so the pre-restart evidence isn't lost):

```bash
kubectl logs -n node-rotation-system deployment/soak-scraper > "$run/scrape.log"
# if the scraper restarted:
kubectl logs -n node-rotation-system deployment/soak-scraper --previous >> "$run/scrape.log"
```

Append order does not matter: the analyzer stable-sorts `scrape.log` lines by
their timestamp before any counter/gauge/gap processing, and a `seq` counter
that starts over is reported as an explicit scraper-restart event with its
wall-clock coverage gap.

Then run the offline analyzer against the harvested run directory:

```bash
scenarios/soak-analyze.py "$run"   # writes $run/report.md — ledger, margin distribution, PASS table
```

#### Teardown

```bash
kubectl label nodepool nodepool-soak noderotation-soak/in-scope-
kubectl label nodepool nodepool-soak-epi noderotation-soak-epi/in-scope- 2>/dev/null || true  # absent if the epilogue already tore itself down
kill %1 %2 2>/dev/null || true   # local recorder + watchdog background jobs (same shell session; use `pkill -f soak-record.sh; pkill -f soak-watchdog.sh` from a different one)
make e2e-eks-down        # terraform destroy
```

Confirm nothing lingers (Auto Mode EC2/EBS) before walking away.

**Cost.** Control plane + NAT ~20h ≈ $3; steady 5 × 2-vCPU on-demand × ~18h ≈
$7; epilogue 1 × ~2.5h plus transients ≈ $0.5 — **total ≈ $10.5**.

---

## 5. Cleanup between / after scenarios

| Action | Command |
|--------|---------|
| Halt rotation (every scenario) | `kubectl label nodepool nrc-poc noderotation-poc/in-scope-` |
| Reclaim leftover empty surge node | automatic (`consolidateAfter: 60s`); or `kubectl delete nodeclaim <empty>` |
| Make a `failed` candidate fresh | `kubectl annotate nodeclaim <c> noderotation.io/state- noderotation.io/failed-at- noderotation.io/retry-count-` |
| Clear a manual freeze (Scenario E) | `kubectl annotate nodepool nrc-poc noderotation.io/freeze-` |
| Remove a scenario's workload | `kubectl delete -f scenarios/<workload>.yaml` (`workload`, `statefulset-ebs`, `pdb-workload`, `workload-absorb`, `preemption`, `nodepool-b`) |
| Revert a Scenario N drift label | `kubectl patch nodepool nrc-poc --type json -p '[{"op":"remove","path":"/spec/template/metadata/labels/poc-drift"}]'` |
| Remove Scenario O objects | `kubectl delete -f scenarios/ff-workload.yaml -f scenarios/nrc-ff-policy.yaml -f scenarios/nodepool-ff.yaml` |
| Release a Scenario P epilogue freeze by hand | `kubectl annotate nodepool nodepool-soak-epi noderotation.io/freeze-` (normally done by `soak-epi-release.sh`, not manually) |
| Scenario P frozen teardown (abort rule, §6.13) | `kubectl scale deployment/soak-epi-workload --replicas=0 && kubectl delete rotationpolicy nrc-soak-epi --ignore-not-found && kubectl delete nodepool nodepool-soak-epi --ignore-not-found` |
| Harvest the Scenario P scraper log | `kubectl logs -n node-rotation-system deployment/soak-scraper > $run/scrape.log` (add `--previous` if it restarted) |

> **Teardown order.** When a scenario tightened the window or another knob, **drop
> the in-scope label first**, then restore the other knobs. Restoring an open
> window while the pool is still in-scope can re-open a governed window and start a
> fresh rotation; dropping the label right after would take the pool out of
> governance mid-rotation. The controller now reaps such an orphan (rolls the
> in-flight rotation back, issue #141), but out-of-scope-first avoids re-opening a
> governed window during teardown entirely.

---

## 6. Gotchas & troubleshooting

- **Controller refuses to start a rotation: `OverrideGBelowOne`.** A daily window
  has worst-case period **P=24h**. The §3.2 feasibility gate fatally rejects the
  schedule unless `expireAfter` guarantees ≥ `minRotationChances` (2) window
  occurrences between `ageThreshold` and `expireAfter` — i.e. `expireAfter` must be
  ≫ `ageThreshold + 2·24h`. `expireAfter=12h` is rejected; **`336h`** gives 13
  chances. (This is why the manifest uses 336h.) Look for
  `schedule feasibility is fatal … OverrideGBelowOne` in the logs.

- **Nothing rotates even though the node is old enough.** Two common causes:
  1. **Cooldown gates the *initial* rotation too.** `cooldownAfter` blocks a start
     when `last-rotation-at` **or** `last-failure-at` is within the cooldown — not
     just retries. Keep `cooldownAfter` small (the manifest uses 1m); a recent
     prior rotation otherwise blocks the next.
  2. **The candidate is `state=failed`.** A failed claim is not an eligible
     candidate until its escalated backoff (`retryBackoff`, here 30m) elapses, so
     `noderotation_candidates` reads 0. To retest immediately, clear its rotation
     annotations:
     ```bash
     kubectl annotate nodeclaim <c> noderotation.io/state- noderotation.io/failed-at- noderotation.io/retry-count-
     ```

- **Expected non-fatal findings.** With `ageThreshold=5m` ≪ `P=24h` the controller
  logs `AVeryAggressive` and `ThroughputBelowArrival` findings every pass. These
  are intentional for a fast test (rotating very young nodes), not errors.

- **Don't change the NodePool *template* mid-test.** `expireAfter`,
  `requirements`, etc. live in `spec.template` and changing them triggers Karpenter
  **drift**, which replaces the node outside the controller's flow and confounds
  the test. Recreate the pool cleanly instead. `spec.limits` and
  `spec.disruption.*` are NOT in the template — patch them live (Scenario B).

- **Policy changes are applied live (no restart).** The controller watches the
  RotationPolicy CRD (issue #119), so a `readyTimeout`/`ageThreshold`/window change
  is a `kubectl patch rotationpolicy nrc-poc …` that takes effect at the next
  reconcile — no `helm upgrade` and no `kubectl rollout restart`.

- **Metrics reset on restart.** The `completed_total` counters are in-memory; a
  controller restart zeroes them. Capture a baseline after each restart.

- **Keep the controller OFF every rotated pool.** Auto Mode consolidation can land
  the controller Pod on a rotated node; rotating or force-deleting that node then
  restarts the controller mid-scenario and resets the in-memory metric counters
  (this silently broke a first Scenario E run, and a first Scenario O run — see
  `VALIDATION.md`). `scenarios/controller-values.yaml` ships an `affinity` that
  **positively allowlists** the Auto Mode built-in pools
  (`karpenter.sh/nodepool In [general-purpose, system]`), which no scenario
  rotates — keep it. A `NotIn` blocklist on one scenario's own label (e.g.
  `noderotation-poc/pool` or `nodepool-ff`'s `karpenter.sh/nodepool`) is unsafe: a
  rotated node from a *different* scenario lacks that label and passes the filter,
  so the controller can still land there.

- **Reading a `/`-keyed annotation with jsonpath.** Escape the dots inside the
  bracket selector: `['karpenter\.sh/do-not-disrupt']`, not
  `['karpenter.sh/do-not-disrupt']` — the unescaped form silently returns empty.

- **EBS dynamic provisioning fails (Scenario A).** The cluster's default `gp2`
  StorageClass uses the legacy in-tree `kubernetes.io/aws-ebs` provisioner, which
  does not provision on Auto Mode. Use the provided `nrc-poc-gp3` StorageClass
  (provisioner `ebs.csi.eks.amazonaws.com`).

---

## Teardown

Remove the scenario objects, the controller, then the whole stack:

```bash
kubectl delete -f scenarios/statefulset-ebs.yaml --ignore-not-found
kubectl delete -f scenarios/workload.yaml --ignore-not-found
kubectl delete -f scenarios/nodepool.yaml --ignore-not-found
# The RotationPolicy is applied with kubectl (rotationPolicies: []), so it
# is NOT owned by the release — `helm uninstall` won't remove it. Delete it
# explicitly so reruns start from a clean, policy-free cluster.
kubectl delete -f scenarios/rotationpolicy.yaml --ignore-not-found
helm uninstall node-rotation-controller -n node-rotation-system

# then the ephemeral cluster + all AWS resources:
make e2e-eks-down          # terraform destroy
```

Confirm nothing lingers (Auto Mode EC2/EBS, load balancers) before walking away.
Record any divergence from the expected outcomes as a follow-up `fix(...)` issue
and update spec
[§7.2](../../../docs/specification/07-risks.md#72-validated-assumptions).
