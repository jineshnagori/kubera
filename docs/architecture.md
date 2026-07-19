# Architecture

Single binary, one reconciler, three cooperating stages plus an optional
admission webhook.

```
Reconcile(DynamicResource)
  1. Guards        validate bounds; resolve selector (same-namespace
                   Deployments); DynamicResource overlap (oldest wins);
                   VPA conflict (refuse to fight VPA)
  2. Recommend     provider (Metrics Server | Prometheus) → decaying
                   histograms → P90 CPU / P95 memory → +10% margin →
                   round → clamp to bounds → status.recommendations
  3. Actuate       (updateMode != Off, cluster ≥1.33)
                   HPA coordination → per-pod plan (QoS, tolerance,
                   step caps, memory-decrease rule) → cooldowns →
                   cluster-wide rate limit → resize subresource PATCH
```

## HPA coordination (coupled-loop math)

HPA's resource metric is `usage / requests` — KubeRA changing requests changes
what HPA sees:

- **Up**: raising requests lowers observed utilization; HPA stays quiet.
  That is the vertical-first gating.
- **Down**: lowering requests raises observed utilization. Before any downward
  resize KubeRA computes the post-resize utilization and floors the new
  request at `usage × 100 / (target − headroom)` so HPA is never re-triggered
  by a KubeRA action.
- While the HPA is actively scaling (`desiredReplicas != currentReplicas` or
  a recent `lastScaleTime`), all vertical actuation pauses.
- When recommendations pin at the configured CPU max, `status.hpaState`
  becomes `AtMaxVertical`: HPA takes over replica scaling.

## In-place resize safety

- Resizes go through the Pod `resize` subresource only (1.33+, detected at
  startup via the discovery API).
- QoS class never changes: BestEffort Pods are skipped, Guaranteed Pods keep
  requests == limits, Burstable limit/request ratios are preserved.
- Memory limits never decrease in-place (kubelet requires a container restart
  for that); Guaranteed Pods therefore cannot shrink memory at all.
- Tolerance band (±10%) and per-direction step caps (50% up / 20% down)
  prevent flapping; per-direction cooldowns (2m up / 10m down) space resizes.
- OOMKilled containers get an immediate 1.5× memory bump that bypasses all
  of the above (bounded by the configured max).
- `Infeasible` resizes (node too small) are surfaced as a status condition
  and not retried until topology changes.
- `InPlaceOrRecreate` falls back to eviction (PDB-respecting, one pod per
  workload per pass) for resizes in-place can never do — Infeasible, or
  Guaranteed memory shrink. Gated on the pod webhook: replacements must be
  born with the recommendation or eviction is pointless.
- A cluster-wide token bucket (`--max-resizes-per-minute`) prevents
  thundering herds.

## Spec drift & the pod webhook

KubeRA patches Pods, not Deployment templates. New Pods (rollouts, HPA
scale-out) are born with template values. With `--enable-pod-webhook`, a
mutating webhook (failurePolicy=Ignore) injects the current recommendation at
Pod admission, so new Pods start right-sized. Without it, new Pods are
re-optimized within a few polling intervals.

## State

All recommender state (histograms) is in-memory and rebuilds within a few
polling intervals after a restart. Cooldowns also reset on restart — worst
case one early resize. Leader election guarantees a single actuator.
