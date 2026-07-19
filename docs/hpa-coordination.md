# HPA Coordination

KubeRA does not replace the HPA — it delays HPA activation by scaling Pods
vertically first, and it is careful never to destabilize the HPA's control
loop. This page explains the mechanics.

## The coupled-loop problem

HPA's resource metric is:

```text
utilization = usage / requests
```

KubeRA changes `requests`, so **every resize changes what the HPA sees**:

- **Raising requests lowers observed utilization** — the HPA backs off. This
  is the vertical-first gating, and it is intentional: as long as a Pod can
  grow, replica count stays flat.
- **Lowering requests raises observed utilization** — done naively, shrinking
  resources after a traffic drop makes the HPA scale **out**: traffic falls,
  requests shrink, HPA sees 95% utilization, adds replicas, per-pod usage
  falls further, requests shrink again… a runaway oscillation. This is
  exactly why VPA + HPA on the same metric is forbidden.

## The headroom floor

Before any downward resize, KubeRA reads the HPA spec and computes the
utilization the HPA will observe *after* the change. The new request is
floored at:

```text
floor = usage × 100 / (cpuTargetUtilization − reserveHeadroomPercent)
```

**Worked example.** Usage 130m, request 800m, HPA target 75%, headroom 10%:

- Raw recommendation: ~150m (P90 × 1.1 margin).
- Floor: `130 × 100 / (75 − 10)` = **200m**. At a 200m request the HPA
  observes 65% — safely under its 75% target.
- KubeRA resizes to 200m, not 150m. The HPA never fires.

The floor applies per resource: CPU and memory targets are floored
independently when the HPA has a utilization target for them (both
`Resource` and `containerResource` metric types are read).

## Pause while scaling

While the HPA is actively scaling — `status.desiredReplicas !=
status.currentReplicas`, or `lastScaleTime` within `cooldownAfterHPA` — all
vertical actuation for that workload pauses and `status.hpaState` reports
`Scaling`. Two controllers changing Pod resources and replica counts at the
same moment is how you get feedback loops; one at a time.

## HPA takeover

When a workload's CPU recommendation pins at `resources.cpu.max` and demand
continues, `status.hpaState` becomes `AtMaxVertical`. Vertical scaling is
exhausted; replica scaling is the correct next move, and the HPA does it
naturally — Pods at max requests under sustained load show high utilization,
which is precisely the HPA's trigger.

```text
2 Pods @ 500m  →  2 Pods @ 2000m (max)  →  HPA: 3, 4, 5 replicas
traffic drops  →  HPA: back to 2        →  KubeRA: 2000m → … → baseline
```

## Lifecycle state

`status.hpaState`:

| State | Meaning |
|-------|---------|
| `Idle` | No HPA activity; KubeRA optimizes freely |
| `Scaling` | HPA actively scaling; vertical actuation paused |
| `AtMaxVertical` | Recommendation pinned at max; HPA owns scaling now |

## Best practice: decouple when you can

The cleanest setup puts the HPA on **custom or external metrics** (requests
per second, queue depth, latency). Then the loops are fully independent —
KubeRA owns resources, HPA owns replicas, and neither observes the other.
Set `hpa.mode: Ignore` in that case to skip the coordination overhead.

For resource-metric HPAs, `Cooperative` mode (the default) applies the
headroom floor and pause logic automatically.
