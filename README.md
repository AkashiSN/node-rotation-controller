# node-rotation-controller

[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)
[![Status](https://img.shields.io/badge/status-specification-orange.svg)](docs/specification.md)

A Kubernetes controller that proactively rotates Karpenter-managed nodes within a defined maintenance window, using **make-before-break (surge)** semantics, before Karpenter's forceful `expireAfter` triggers.

Designed for EKS Auto Mode and any Karpenter v1+ environment where node expiration is forceful and disruption budgets do not apply.

## Status

**Specification phase.** Implementation has not started. See [docs/specification.md](docs/specification.md) for the full design.

日本語版: [README.ja.md](README.ja.md) / [docs/ja/specification.md](docs/ja/specification.md)

## Why

Karpenter classifies node disruption into two categories:

| Category | Examples | NodePool Disruption Budgets | Pre-provisioned replacement |
|----------|----------|------------------------------|------------------------------|
| Graceful | Drift, Consolidation | Applied | Yes (make-before-break) |
| **Forceful** | **Expiration**, Spot Interruption | **Not applied** | **No** |

Expiration is intentionally forceful (see the upstream [forceful-expiration design](https://github.com/kubernetes-sigs/karpenter/blob/main/designs/forceful-expiration.md)) so that AMI patches and security updates cannot be indefinitely delayed by misconfigured budgets. The upstream design explicitly lists "operators implement their own graceful rotation" as one acceptable path. EKS Auto Mode further enforces a 21-day hard cap on node lifetime that cannot be lifted.

The practical consequence: nodes **will be force-drained** at some point within 21 days, regardless of PDBs, and Karpenter will only provision a replacement *after* the drain begins. This can land in peak business hours.

This controller closes that gap by:

1. Watching `NodeClaim` resources approaching expiration
2. Restricting rotation to a configurable **maintenance window** (e.g., Saturday 02:00–06:00)
3. Creating a replacement `NodeClaim` first, waiting until it is `Ready`, then deleting the old one (**surge**)
4. Letting Karpenter's standard termination controller graceful-drain the old node, where PDBs *do* apply

## What it is not

- **Not** a replacement for Karpenter Consolidation, Drift, or Disruption Budgets — it composes with them
- **Not** a Spot interruption handler (use [AWS Node Termination Handler](https://github.com/aws/aws-node-termination-handler))
- **Not** an OS-patch reboot tool (use [kured](https://github.com/kubereboot/kured))
- **Not** a pod descheduler (use [descheduler](https://github.com/kubernetes-sigs/descheduler))
- **Not** a replacement for application-side warm-up (`readinessProbe`, `readinessGate`, `slow_start`) — surge places nodes, applications must place themselves

## Project layout

```
.
├── docs/
│   ├── specification.md       Full design specification (English)
│   └── ja/specification.md    Japanese translation
├── charts/                    Helm chart (planned)
├── cmd/                       Controller entry point (planned)
├── api/                       CRD types (planned, if needed beyond ConfigMap)
└── internal/                  Reconciler implementation (planned)
```

## Getting involved

This project is in the specification phase. Feedback on the design is welcome via GitHub Issues. Implementation contributions will be accepted once v1 scope is locked.

## License

Apache 2.0 — see [LICENSE](LICENSE).
