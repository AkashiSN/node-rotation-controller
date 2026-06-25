# Examples

Ready-to-adapt `RotationPolicy` manifests for `node-rotation-controller`. Policy
is configured through cluster-scoped `RotationPolicy` objects (spec
[§5.4](../docs/specification.md#54-configuration-schema)); the controller
resolves each NodePool's governing policy at reconcile time. A NodePool matched
by **no** policy is never rotated — its `expireAfter` backstop still applies.

These manifests are illustrative: edit the `nodePoolSelector` labels, windows,
and timeouts for your cluster before applying. The full field reference is in the
[Helm chart README](../charts/node-rotation-controller/README.md#configuring-rotation-rotationpolicy).

| File | Shows |
|------|-------|
| [`single-policy.yaml`](single-policy.yaml) | The simplest setup — one catch-all policy governing every NodePool. Start here. |
| [`multi-nodepool.yaml`](multi-nodepool.yaml) | Divergent policy per NodePool (issue #119): different cadence/timeouts for `api` vs `batch` pools, selected by label. |
| [`specificity-resolution.yaml`](specificity-resolution.yaml) | How a broad default and a narrower override coexist — most-specific selector wins; an equal-specificity tie is a hard error. |
| [`maintenance-windows.yaml`](maintenance-windows.yaml) | Composing `maintenanceWindows`: the union of entries, the no-overnight-wrap split at midnight, and multiple timezones. |

## Applying

```sh
# CRD ships with the Helm chart; install it first if you have not already:
kubectl apply -f ../charts/node-rotation-controller/crds/

# Then apply an example (after editing the selectors/windows for your cluster):
kubectl apply -f single-policy.yaml

# Inspect resolved policies (short name: rotpol):
kubectl get rotpol -o wide
```

`kubectl get rotpol -o wide` shows each policy's `Age-Threshold` and the
`Matched` / `Rotating` NodePool counts from its status. A policy that ties with
another for some pool, or fails validation, reports it on the pool via a
`PolicyConflict` Warning event and the `noderotation_policy_conflict` metric —
see the [production runbook](../docs/runbook.md) for symptom-based triage.

## Notes

- **`surge.maxUnavailable` is fixed at `1`** in v1 (serial per NodePool); other
  values are rejected.
- **`prePull` is reserved for v2** and must stay `enabled: false` (the default);
  the schema rejects enabling it.
- **`ageThreshold: auto`** is recommended — it derives the threshold from the
  window cadence so `K` rotation chances stay below `expireAfter` (spec §3.2). An
  explicit duration is allowed but still validated.
