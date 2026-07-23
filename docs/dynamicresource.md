# DynamicResource Reference

**API group:** `autoscaling.kubera.io/v1alpha1` · **Kind:** `DynamicResource`
· **Scope:** Namespaced · **Short names:** `dr`, `dynres`

A `DynamicResource` is a namespaced policy: it selects Deployments **in its
own namespace** by label and describes how their Pods' CPU and memory should
be managed. Selectors never cross namespace boundaries: teams own their
policies, and there is no cluster-wide blast radius.

## Complete example

```yaml
apiVersion: autoscaling.kubera.io/v1alpha1
kind: DynamicResource
metadata:
  name: api-policy
  namespace: production
spec:
  selector:
    matchLabels:
      app: api

  updateMode: InPlaceOnly

  resources:
    cpu:    { min: 250m,  max: 2000m }
    memory: { min: 256Mi, max: 4Gi }
    controlledValues: RequestsAndLimits

  containerOverrides:
  - containerName: istio-proxy
    mode: "Off"

  metrics:
    provider: MetricsServer
    pollingInterval: 30s
    cpuPercentile: 90
    memoryPercentile: 95

  behavior:
    scaleUp:
      maxStepPercent: 50
      cooldown: 2m
    scaleDown:
      maxStepPercent: 20
      cooldown: 10m

  hpa:
    mode: Cooperative
    reserveHeadroomPercent: 10
    pauseWhileHPAScaling: true
    cooldownAfterHPA: 10m
```

---

## `spec.selector` (required)

Label selector matching **Deployments** (their metadata labels) in the same
namespace. Same model as Services and NetworkPolicies.

```yaml
# one workload: use a label unique to it
selector:
  matchLabels: { app: api }

# a group: shared label, one policy for the team
selector:
  matchLabels: { app.kubernetes.io/part-of: payments }
```

If two DynamicResources match the same Deployment, the **older one wins**;
the newer gets condition `Conflicted` and takes no action. A workload already
managed by a VerticalPodAutoscaler also produces `Conflicted`
(`VPAConflict`): KubeRA refuses to fight VPA over the same Pods.

## `spec.updateMode`

| Value | Behaviour |
|-------|-----------|
| `"Off"` *(default)* | Compute and publish recommendations in `status` only. Pods are never touched. **The adoption path**: run for days, inspect, then switch. |
| `InPlaceOnly` | Apply recommendations via the Pod `resize` subresource. Resizes that cannot happen in-place are skipped and surfaced in conditions. |
| `InPlaceOrRecreate` | Like `InPlaceOnly`, plus: Pods whose resize is *impossible* in-place (node too small, or a Guaranteed pod that must shrink memory) are **evicted** via the Eviction API, PDB-respecting, at most one pod per workload per pass. **Requires the pod webhook** (`webhook.enable=true`); without it the replacement would inherit stale template resources, so KubeRA refuses with condition `ResizeInfeasible: RecreateRequiresWebhook`. |

!!! warning "Quote `\"Off\"` in YAML"
    Bare `Off` is YAML 1.1 boolean `false`. Always write `updateMode: "Off"`.

## `spec.resources` (required)

Bounds for **all containers** of matched workloads (per-container exceptions
via `containerOverrides`).

```yaml
resources:
  cpu:
    min: 250m      # floor, never sized below
    max: 2000m     # ceiling: reaching it signals HPA takeover (AtMaxVertical)
  memory:
    min: 256Mi
    max: 4Gi
  controlledValues: RequestsAndLimits
```

| Field | Notes |
|-------|-------|
| `cpu.min` / `cpu.max` | Required. `min <= max` enforced (condition `InvalidBounds` otherwise). |
| `memory.min` / `memory.max` | Required. Same validation. |
| `controlledValues` | `RequestsAndLimits` *(default)*: requests set to the recommendation, limits scaled to preserve the container's original limit/request ratio (Guaranteed pods keep requests == limits). `RequestsOnly`: limits never touched; requests capped at existing limits. |

!!! note "32Mi memory floor"
    Recommendations are floored at **32Mi** regardless of `memory.min`. On
    Guaranteed pods the recommendation becomes the memory *limit*; a limit
    sized from a tiny idle working set OOMKills the container on any spike.
    (VPA applies the same guard with a 250Mi default.)

## `spec.containerOverrides`

Per-container exceptions. Matches containers by name: including **injected
sidecars** (e.g. `istio-proxy`) that exist only in Pods, not in the
Deployment template.

```yaml
containerOverrides:
- containerName: istio-proxy
  mode: "Off"                    # never touch the sidecar
- containerName: worker
  cpu:    { min: 500m, max: 4000m }   # own bounds for this container
  memory: { min: 1Gi,  max: 8Gi }
```

| Field | Notes |
|-------|-------|
| `containerName` | Required, unique per entry. |
| `mode` | `Auto` *(default)* or `Off` (exclude from management). |
| `cpu` / `memory` | Optional bounds overriding `spec.resources` for this container. |

## `spec.metrics`

```yaml
metrics:
  provider: MetricsServer     # MetricsServer (default) | Prometheus
  pollingInterval: 30s        # default 30s
  cpuPercentile: 90           # default 90, range 50-100
  memoryPercentile: 95        # default 95, range 50-100
  prometheusURL: ""           # informational; the operator-level --prometheus-url wins
```

Samples feed **decaying histograms** (~5% bucket resolution, 12h half-life):
recommendations are percentiles over history, not point-in-time reads, plus a
10% safety margin. Duplicate samples from reconcile bursts are deduplicated
per pod timestamp.

**Prometheus provider** requires the operator flag `--prometheus-url` and
kube-state-metrics in the cluster (pod-label join via `kube_pod_labels`).
Only `matchLabels` selectors are supported with Prometheus:
`matchExpressions` cannot be translated to a PromQL vector match. Without the
flag, a policy requesting Prometheus gets condition
`Ready: UnsupportedProvider`.

## `spec.behavior`

Asymmetric on purpose: scale up fast, scale down slow.

```yaml
behavior:
  scaleUp:
    maxStepPercent: 50     # one resize moves a value at most +50% (default 50)
    cooldown: 2m           # min time between up-resizes per workload (default 2m)
    stabilizationWindow: 1m
  scaleDown:
    maxStepPercent: 20     # at most -20% per resize (default 20)
    cooldown: 10m          # default 10m
    stabilizationWindow: 10m
```

Additional built-in guards (not configurable per policy):

- **±10% tolerance band**: no resize when the recommendation is within 10%
  of the current value (anti-flap).
- **QoS preservation**: a Pod's QoS class never changes: BestEffort pods are
  skipped entirely, Guaranteed keeps requests == limits, Burstable keeps its
  limit/request ratio.
- **Memory limits never decrease in-place**: kubelet constraint. On
  Guaranteed pods memory therefore cannot shrink in-place at all; use
  `InPlaceOrRecreate` + webhook, or run the workload Burstable.
- **OOM fast path**: a container OOMKilled within the last 15 minutes gets
  an immediate 1.5× memory bump that bypasses tolerance, step caps and
  cooldowns (capped at `memory.max`).
- **Cluster-wide rate limit**: `--max-resizes-per-minute` (default 30).

## `spec.hpa`

Coordination with an HPA whose `scaleTargetRef` points at a matched
Deployment. See [HPA Coordination](hpa-coordination.md) for the theory.

```yaml
hpa:
  mode: Cooperative            # Cooperative (default) | Ignore
  reserveHeadroomPercent: 10   # default 10, range 0-50
  pauseWhileHPAScaling: true   # default true
  cooldownAfterHPA: 10m        # default 10m
```

| Field | Notes |
|-------|-------|
| `mode` | `Cooperative`: headroom floors + pause-on-scaling. `Ignore`: no HPA awareness (use when HPA scales on custom/external metrics). |
| `reserveHeadroomPercent` | A downward resize never brings the utilization HPA observes above `target − headroom`. |
| `pauseWhileHPAScaling` | All vertical actuation pauses while `desiredReplicas != currentReplicas` or within `cooldownAfterHPA` of the last scale event. |
| `cooldownAfterHPA` | Settle window after an HPA scale event. |

## `status`

```yaml
status:
  conditions:
  - type: Ready               # policy resolved and operating
  - type: Conflicted          # overlapping DynamicResource or VPA
  - type: ResizeInfeasible    # see reasons below
  matchedWorkloads:
  - { kind: Deployment, name: api }
  recommendations:            # published even in "Off" mode
  - workload: api
    containers:
    - containerName: app
      target:  { cpu: 480m, memory: 610Mi }   # what KubeRA wants
      applied: { cpu: 480m, memory: 610Mi }   # what it last actuated
  lastResizeTime: "2026-07-19T10:00:00Z"
  hpaState: Idle              # Idle | Scaling | AtMaxVertical
  observedGeneration: 3
```

### Condition reasons

| Condition | Reason | Meaning |
|-----------|--------|---------|
| `Ready=False` | `NoMatch` | Selector matches no Deployment in the namespace |
| `Ready=False` | `InvalidBounds` | A `min` exceeds its `max` |
| `Ready=False` | `MetricsUnavailable` | Metrics provider failing; retried with backoff |
| `Ready=False` | `UnsupportedProvider` | `provider: Prometheus` without `--prometheus-url` |
| `Conflicted=True` | `OverlappingSelector` | Older DynamicResource already owns a matched workload |
| `Conflicted=True` | `VPAConflict` | A VerticalPodAutoscaler targets a matched workload |
| `ResizeInfeasible=True` | `ClusterUnsupported` | Cluster < 1.33, no resize subresource |
| `ResizeInfeasible=True` | `NodeCapacity` | Node cannot fit a pending resize |
| `ResizeInfeasible=True` | `RecreateRequiresWebhook` | `InPlaceOrRecreate` needs `webhook.enable=true` |

### Events

`RecommendationUpdated`, `PodsResized`, `PodEvictedForResize`, `OOMFastPath`,
`Conflicted`, `VPAConflict`: all on the DynamicResource object
(`kubectl describe dynamicresource <name>`).

---

## Recipes

### Recommend-only for a whole team namespace

```yaml
apiVersion: autoscaling.kubera.io/v1alpha1
kind: DynamicResource
metadata:
  name: team-payments-policy
  namespace: payments
spec:
  selector:
    matchLabels: { app.kubernetes.io/part-of: payments }
  updateMode: "Off"
  resources:
    cpu:    { min: 100m, max: 1000m }
    memory: { min: 128Mi, max: 2Gi }
```

### Web service behind an HPA

```yaml
spec:
  selector:
    matchLabels: { app: web }
  updateMode: InPlaceOnly
  resources:
    cpu:    { min: 250m,  max: 2000m }   # at 2000m, hpaState -> AtMaxVertical
    memory: { min: 256Mi, max: 2Gi }
  hpa:
    mode: Cooperative
    reserveHeadroomPercent: 15           # extra safety against scale-out flapping
```

### JVM workload: manage CPU only

Fixed heap (`-Xmx`) makes memory resize useless upward and lethal downward.

```yaml
spec:
  selector:
    matchLabels: { app: java-api }
  updateMode: InPlaceOnly
  resources:
    cpu:    { min: 500m, max: 4000m }
    memory: { min: 2Gi,  max: 2Gi }      # pin memory: min == max, never moves
```

### Guaranteed pods that must shrink memory

```yaml
spec:
  selector:
    matchLabels: { app: batch }
  updateMode: InPlaceOrRecreate          # webhook.enable=true required
  resources:
    cpu:    { min: 100m, max: 2000m }
    memory: { min: 128Mi, max: 4Gi }
```

One pod per pass is evicted (PDB-respecting); the webhook injects the
recommendation into the replacement, which starts right-sized.
