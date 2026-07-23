# Why KubeRA (and not just VPA)

A technical comparison against the Vertical Pod Autoscaler, for teams
deciding whether KubeRA solves a problem VPA doesn't already solve for them.

## "VPA already does vertical scaling. Why another tool?"

VPA and KubeRA overlap on one component: a recommender using decaying
histograms over usage. Everything around it differs:

| | VPA | KubeRA |
|---|-----|--------|
| **Primary actuation** | Evict and recreate (`Recreate`/`Auto` modes). An in-place mode (`InPlaceOrRecreate`) exists but is alpha, off by default, and falls back to eviction whenever in-place isn't immediately possible | In-place resize (`resize` subresource, beta, enabled by default since Kubernetes 1.33) is the primary path. Eviction is an explicit opt-in fallback (`InPlaceOrRecreate`), gated behind a mutating webhook so replacements are born right-sized |
| **HPA on CPU/memory** | The [VPA documentation](https://kubernetes.io/docs/concepts/workloads/autoscaling/vertical-pod-autoscale/) advises against running VPA and HPA on the same CPU/memory metric: both would react to each other's changes | Built around this case: downward resizes are floored so the utilization HPA observes doesn't cross a configured target minus headroom; actuation pauses while HPA is actively scaling; a status field signals when vertical scaling is exhausted and HPA should take over |
| **Targeting** | One `VerticalPodAutoscaler` object per workload (`targetRef`) | A namespaced policy per label selector: one `DynamicResource` can cover several Deployments sharing a label |
| **Behavior control** | The updater evicts within PodDisruptionBudget limits | Per-direction step caps, cooldowns, a tolerance band, and a cluster-wide resize rate limit, configurable per policy |
| **Disruption on resize** | Each adjustment (outside the alpha in-place mode) recreates the pod | In-place resizes don't restart the container; this is what the `resize` subresource is for |
| **Cost visibility** | Not built in | Prometheus metrics for recommended vs. currently-requested resources, and a `kubectl kubera diff` command |
| **Architecture** | Three components (recommender, updater, admission controller) | One controller binary, plus an optional webhook |

## "The disruption doesn't matter, VPA respects PDBs"

PDBs bound how many pods are evicted at once, not whether a given pod is
evicted. For workloads with meaningful startup cost (warm caches, JIT
compilation, long-lived connections, leader-elected singletons), an eviction
is a real disruption regardless of how many other replicas stay up. In-place
resize changes the container's cgroup limits without restarting it, so those
workloads don't pay that cost on every adjustment. For workloads that don't
have this cost, this distinction doesn't matter much.

## "We'll enable VPA's in-place mode then"

Two differences remain even with VPA's `InPlaceOrRecreate` mode enabled:

1. It's alpha and feature-gated, and falls back to eviction in situations
   KubeRA is designed to handle differently: waiting out a cooldown, or
   flooring a downward resize against HPA headroom instead of evicting.
2. The HPA guidance above still applies to it: VPA has no model of how its
   own request changes affect the utilization HPA observes. KubeRA's HPA
   coordination (pause-while-scaling, headroom floors) is not something VPA
   does today, in any mode.

Separately, VPA has no per-direction step caps, cooldowns, or tolerance
band on its resizes; these exist in KubeRA to bound how much a single
resize changes a running pod's resources.

## "So when should we just use VPA?"

- Workloads without an HPA on CPU/memory, where a pod restart per
  adjustment is acceptable: VPA is mature and well-established for this.
- StatefulSets, DaemonSets, and Jobs: VPA supports these; KubeRA currently
  targets Deployments only.
- Workloads where checkpointed recommendation history across restarts
  matters today: VPA persists its histograms in a CRD; KubeRA's recommender
  state is in-memory and rebuilds after a controller restart (a checkpoint
  CRD is on the roadmap, see below).
- Where longer production track record matters more than the differences
  above: VPA has years of it; KubeRA doesn't yet.

Running KubeRA in `updateMode: "Off"` alongside an existing VPA costs
nothing: it only publishes recommendations to `status`, and comparing the
two costs no more than reading both.

## What KubeRA's recommend-only mode shows you

Before touching any Pod, KubeRA computes what it would request and publishes
that in `status.recommendations` and in Prometheus metrics
(`kubera_reclaimable_cpu_millicores`, `kubera_reclaimable_memory_bytes`,
the gap between what's currently requested and what's recommended). Compare
it against your own workloads directly:

```sh
kubectl kubera diff            # requested vs recommended, workload by workload
```

That comparison, on your own cluster with your own workloads, is a more
useful answer than any general estimate.

---

# Roadmap: predictive autoscaling

Everything above is reactive: observe usage, then converge toward it. The
recommender smooths noise but has no model of the future. This is the
planned direction for changing that: each phase keeps the same CRD and
actuation guardrails; only the recommender changes.

## Phase 1: Durable state (checkpointing)

Persist per-container histograms in a checkpoint CRD, VPA-style, so a
controller restart doesn't reset recommendation history.

## Phase 2: Historic backfill

Seed histograms from Prometheus range queries
(`container_cpu_usage_seconds_total[30d]`) the first time a workload is
seen, instead of starting from nothing. Requires the already-supported
`--prometheus-url`.

## Phase 3: Seasonality profiles

Maintain per-window histograms (hour-of-day × day-of-week) for workloads
with cyclical load, and resize ahead of a learned peak instead of reacting
after it starts. Prospective spec addition:

```yaml
prediction:
  mode: Seasonal          # Off | Seasonal | Forecast
  preScaleLead: 15m       # act this far ahead of the learned peak
```

Predictions would still move targets only within the configured min/max
bounds, through the same step caps and HPA headroom floors described above.

## Phase 4: Forecasting

A pluggable prediction path behind the existing recommender interface,
starting with classical time-series models (Holt-Winters or similar), with
forecast accuracy scored per workload and a fallback to the reactive
percentile when a forecast underperforms it.

## Phase 5: Fleet-level views

Recommendations today are per-workload. The same underlying data could
support cluster-level views: which workloads could be co-located, awareness
of node consolidation, and a what-if projection of applying all current
recommendations at once.

These are directional, not committed dates; each phase depends on the
previous one landing and being useful in practice.
