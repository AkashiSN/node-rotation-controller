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

- the NodePool carries a label matched by the controller's `nodepoolSelectors`
  (here `noderotation-poc/in-scope: "true"`);
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
```

Installing onto a zero-node cluster makes Auto Mode launch a node in the
`general-purpose` pool to host the controller — expected, and **out of scope**
(only `nrc-poc` is rotated). Confirm health:

```bash
kubectl -n node-rotation-system get deploy node-rotation-controller          # 1/1
kubectl -n node-rotation-system get lease node-rotation-controller.noderotation.io
kubectl -n node-rotation-system logs deploy/node-rotation-controller | grep -i "Starting workers"
```

`scenarios/controller-values.yaml` sets the test knobs (rationale in the file):
`ageThreshold: 5m`, always-open window, `surge.readyTimeout: 15m`,
`cooldownAfter: 1m`, `retryBackoff: 30m`, single replica. **The `config.policy`
block is read once at startup** — changing it later needs
`helm upgrade … && kubectl rollout restart` (used in Scenario C).

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
normal surge times out, then restart the controller to load it and trigger:

```bash
# render a values file with readyTimeout: 15s (keep the long retryBackoff)
sed 's/      readyTimeout: 15m/      readyTimeout: 15s/' \
  scenarios/controller-values.yaml > /tmp/scenario-c-values.yaml

helm upgrade node-rotation-controller ../../../charts/node-rotation-controller \
  --namespace node-rotation-system -f /tmp/scenario-c-values.yaml \
  --set image.repository="$REPO" --set image.tag=poc
kubectl rollout restart deploy/node-rotation-controller -n node-rotation-system
kubectl rollout status  deploy/node-rotation-controller -n node-rotation-system --timeout=120s

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

**Cleanup.** Restore the normal `readyTimeout` and clear the failed marker so the
candidate is reusable:

```bash
kubectl label nodepool nrc-poc noderotation-poc/in-scope-
helm upgrade node-rotation-controller ../../../charts/node-rotation-controller \
  --namespace node-rotation-system -f scenarios/controller-values.yaml \
  --set image.repository="$REPO" --set image.tag=poc
kubectl rollout restart deploy/node-rotation-controller -n node-rotation-system
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
at ~5m, hundreds of times before a single 336h `expireAfter` could fire. A genuine
multi-hour soak is deferred.

---

## 5. Cleanup between / after scenarios

| Action | Command |
|--------|---------|
| Halt rotation (every scenario) | `kubectl label nodepool nrc-poc noderotation-poc/in-scope-` |
| Reclaim leftover empty surge node | automatic (`consolidateAfter: 60s`); or `kubectl delete nodeclaim <empty>` |
| Make a `failed` candidate fresh | `kubectl annotate nodeclaim <c> noderotation.io/state- noderotation.io/failed-at- noderotation.io/retry-count-` |
| Remove a scenario's workload | `kubectl delete -f scenarios/<workload>.yaml` |

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

- **`config.policy` changes need a restart.** The controller reads `policy.yaml`
  once at startup; `helm upgrade` alone won't apply a `readyTimeout`/`ageThreshold`
  change — follow it with `kubectl rollout restart deploy/node-rotation-controller`.

- **Metrics reset on restart.** The `completed_total` counters are in-memory; a
  controller restart zeroes them. Capture a baseline after each restart.

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
