---
layout: home
hero:
  name: node-rotation-controller
  text: Graceful node rotation for Karpenter
  tagline: "Karpenter's expireAfter drains nodes at unpredictable times — this controller rotates them gracefully, inside your maintenance window, before that happens."
  actions:
    - theme: brand
      text: Get Started
      link: /getting-started
    - theme: alt
      text: Specification
      link: /specification
    - theme: alt
      text: Runbook
      link: /runbook
features:
  - title: Zero-downtime surge
    details: A replacement node is ready before the old one is drained. The controller induces NodePool-owned capacity via a temporary placeholder Pod — Karpenter provisions the node, PDBs govern the drain.
  - title: Window-bounded rotation
    details: Rotation starts only inside your configured maintenance window. An automatically derived age threshold ensures every node gets multiple chances to rotate before its expireAfter deadline.
  - title: Safe by default
    details: expireAfter is kept as a backstop, never removed. If the controller is absent or a rotation fails, nodes still expire exactly as they would without it — never worse than the status quo.
  - title: Validated on EKS Auto Mode
    details: End-to-end tested on real EKS Auto Mode clusters — including a 12-hour unattended soak, zonal-PV rebind, leader failover, and the graceful-to-forceful-fallback boundary.
---
