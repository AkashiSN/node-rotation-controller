---
layout: home
hero:
  name: node-rotation-controller
  text: Make-before-break node rotation for Karpenter
  tagline: Proactively rotate Karpenter-managed nodes within a maintenance window, before expireAfter fires.
  actions:
    - theme: brand
      text: Get Started
      link: /getting-started
    - theme: alt
      text: Specification
      link: /specification
    - theme: alt
      text: Validation
      link: /validation/forceful-fallback
features:
  - title: Surge-first
    details: Induces NodePool-owned replacement capacity via a low-priority placeholder Pod — never bypasses Karpenter, never creates a standalone NodeClaim.
  - title: Window-bounded
    details: Derives an ageThreshold that stays below expireAfter, kept as a backstop rather than removed.
  - title: Validated on real EKS
    details: The real-EKS validation scenario (Scenario O) proves the graceful→forceful fallback split, earliest-deadline ordering, and do-not-disrupt exclusion on a shared deadline.
---
