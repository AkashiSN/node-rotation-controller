# Reproducible PoC validation scenarios (issue #93)

This runbook lets a third party **re-run the spec
[§7.2](../../../docs/specification.md#72-validated-assumptions) PoC validation**
against a real EKS Auto Mode cluster and reach the same outcomes. It is the
"observe rotations" half that the infra
[`README.md`](README.md) (steps 4) only sketches: here are the exact NodePool,
workloads, controller config, trigger/observe commands, **expected output**, and
the non-obvious gotchas.

Every manifest referenced lives in [`scenarios/`](scenarios/). All commands are
copy-pasteable; cluster-specific values come from `terraform output`, so nothing
account-specific is hard-coded.

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

Versions this was last validated on (2026-06-22): **EKS Auto Mode, K8s 1.33,
`karpenter.sh/v1`**, controller image tag `poc`, region `us-west-2` (2 AZs:
`us-west-2a`, `us-west-2b`).

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
# first, and controller-values.yaml set rotationPolicy.create=false, so this is
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
quotes the values actually observed on 2026-06-22; node/claim IDs and exact
seconds will differ, the shape will not.

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

**Cleanup.** Restore the open window (live patch) and halt:

```bash
kubectl patch rotationpolicy nrc-poc --type merge \
  -p '{"spec":{"maintenanceWindows":[{"timezone":"UTC","days":["Mon","Tue","Wed","Thu","Fri","Sat","Sun"],"start":"00:00","end":"23:59"}]}}'
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
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

## 5. Cleanup between / after scenarios

| Action | Command |
|--------|---------|
| Halt rotation (every scenario) | `kubectl label nodepool nrc-poc noderotation-poc/in-scope-` |
| Reclaim leftover empty surge node | automatic (`consolidateAfter: 60s`); or `kubectl delete nodeclaim <empty>` |
| Make a `failed` candidate fresh | `kubectl annotate nodeclaim <c> noderotation.io/state- noderotation.io/failed-at- noderotation.io/retry-count-` |
| Clear a manual freeze (Scenario E) | `kubectl annotate nodepool nrc-poc noderotation.io/freeze-` |
| Remove a scenario's workload | `kubectl delete -f scenarios/<workload>.yaml` (`workload`, `statefulset-ebs`, `pdb-workload`, `workload-absorb`, `preemption`, `nodepool-b`) |
| Revert a Scenario N drift label | `kubectl patch nodepool nrc-poc --type json -p '[{"op":"remove","path":"/spec/template/metadata/labels/poc-drift"}]'` |

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
  reconcile — no `helm upgrade` and no `kubectl rollout restart`. (Pre-#119 the
  policy lived in a `policy.yaml` ConfigMap read once at startup; that path is gone.)

- **Metrics reset on restart.** The `completed_total` counters are in-memory; a
  controller restart zeroes them. Capture a baseline after each restart.

- **Keep the controller OFF the nrc-poc pool.** Auto Mode consolidation can land
  the controller Pod on an nrc-poc node; rotating or force-deleting that node then
  restarts the controller mid-scenario and resets the metric counters (this
  silently broke a first Scenario E run). `scenarios/controller-values.yaml` ships
  an `affinity` (`noderotation-poc/pool NotIn [poc]`) that pins it to the
  general-purpose/system pools — keep it.

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
helm uninstall node-rotation-controller -n node-rotation-system

# then the ephemeral cluster + all AWS resources:
make e2e-eks-down          # terraform destroy
```

Confirm nothing lingers (Auto Mode EC2/EBS, load balancers) before walking away.
Record any divergence from the expected outcomes as a follow-up `fix(...)` issue
and update spec
[§7.2](../../../docs/specification.md#72-validated-assumptions).
