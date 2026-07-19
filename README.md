# KubeRA — Kubernetes Resource Allocator

## Vision

KubeRA is a Kubernetes Operator that performs **dynamic vertical resource
allocation** for running Pods using Kubernetes **In-place Pod Resize**. It acts
as an intelligent layer **between Kubernetes Pods and the Horizontal Pod
Autoscaler (HPA)**.

Instead of immediately scaling replicas when load increases, KubeRA first
optimizes CPU and Memory requests/limits of existing Pods. Only when Pods reach
their configured maximum resources and demand continues to increase does HPA
scale the number of replicas.

This creates a **Vertical-first, Horizontal-second** autoscaling model that
maximizes node utilization and minimizes infrastructure cost.

### How KubeRA differs from VPA

VPA has an in-place mode (alpha), but it is explicitly incompatible with HPA on
CPU/Memory metrics. KubeRA's differentiation is its **HPA cooperation engine**:
it models the coupled control loops (changing requests changes the utilization
percentage HPA sees) and makes resize decisions that never destabilize HPA.

---

## Scaling Flow

    Traffic Increase
          │
          ▼
    Metrics Server / Prometheus
          │
          ▼
        KubeRA
          │
          ├── Increase Pod CPU/Memory (In-place Resize, no restart)
          ├── Continue Monitoring
          ├── Pod reaches configured Max Resources
          ▼
    Allow HPA to Scale Replicas
          │
          ▼
    Deployment Scales Out

    Traffic Drops
          │
          ▼
    HPA scales replicas back down
          │
          ▼
    KubeRA gradually reduces Pod resources toward baseline
    (headroom-checked so the reduction never re-triggers HPA)

---

## Core Responsibilities

- Monitor CPU and Memory utilization (percentile-based, decaying histograms —
  not point-in-time reads).
- Dynamically adjust Pod requests/limits per container.
- Use In-place Pod Resize (`resize` subresource, Kubernetes 1.33+) whenever
  possible.
- Optionally fall back to evict-and-recreate when resize is infeasible
  (`updateMode: InPlaceOrRecreate`), respecting PodDisruptionBudgets.
- Coordinate with HPA instead of replacing it.
- Reduce cloud cost by maximizing existing Pod utilization before replica
  scaling.

---

## Custom Resource: `DynamicResource`

**API group:** `autoscaling.kubera.io/v1alpha1`
**Scope:** `Namespaced`

The CRD is namespace-scoped by design: a `DynamicResource` only ever manages
workloads **in its own namespace**. Teams own their optimization policies,
RBAC stays simple, and there is no cluster-wide blast radius.

### Targeting

A `DynamicResource` selects workloads with a **label selector** — the same
model as Services and NetworkPolicies. It matches Deployments **in its own
namespace only**; selectors never cross namespace boundaries.

- Target one workload: use a label unique to it (e.g. `app: api`).
- Target a group: use a shared label (e.g. `app.kubernetes.io/part-of`).

If two `DynamicResource` objects match the same workload, the newer one gets a
`Conflicted` status condition and does not act; the older one wins.

### Example — single workload

```yaml
apiVersion: autoscaling.kubera.io/v1alpha1
kind: DynamicResource
metadata:
  name: api-service
  namespace: production
spec:
  selector:
    matchLabels:
      app: api

  updateMode: InPlaceOnly        # "Off" | InPlaceOnly | InPlaceOrRecreate

  resources:                     # applies to all containers
    cpu:    { min: 250m,  max: 2000m }
    memory: { min: 256Mi, max: 4Gi }
    controlledValues: RequestsAndLimits   # or RequestsOnly

  containerOverrides:            # optional, per-container exceptions
  - containerName: proxy
    mode: "Off"                  # never touch the sidecar

  metrics:
    provider: MetricsServer      # MetricsServer | Prometheus
    pollingInterval: 30s
    cpuPercentile: 90
    memoryPercentile: 95

  behavior:
    scaleUp:
      stabilizationWindow: 1m
      maxStepPercent: 50         # no sudden jumps
      cooldown: 2m
    scaleDown:
      stabilizationWindow: 10m   # peak-over-window, like HPA
      maxStepPercent: 20
      cooldown: 10m

  hpa:
    mode: Cooperative            # Cooperative | Ignore
    reserveHeadroomPercent: 10
    pauseWhileHPAScaling: true
    cooldownAfterHPA: 10m
```

### Example — multiple workloads (shared label, same namespace)

```yaml
apiVersion: autoscaling.kubera.io/v1alpha1
kind: DynamicResource
metadata:
  name: team-payments-policy
  namespace: payments
spec:
  selector:
    matchLabels:
      app.kubernetes.io/part-of: payments

  updateMode: "Off"              # recommend-only: publish, don't act
                                 # (quoted: bare Off is YAML 1.1 boolean false)

  resources:
    cpu:    { min: 100m, max: 1000m }
    memory: { min: 128Mi, max: 2Gi }
```

### Status

```yaml
status:
  conditions:                    # Ready, Conflicted, ResizeInfeasible, ...
  matchedWorkloads:              # workloads resolved from selector
  - name: api
    kind: Deployment
  recommendation:                # per container — published even in Off mode
    api:
      cpu: 480m
      memory: 610Mi
  appliedResources: { ... }
  lastResizeTime: "2026-07-19T10:00:00Z"
  hpaState: Idle                 # Idle | Scaling | AtMaxVertical
```

Key design points:

- **`updateMode: Off` (recommend-only)** — KubeRA computes and publishes
  recommendations in `status` without touching Pods. Run it for weeks in
  dry-run before trusting actuation. This is the default adoption path.
- **Flat `resources` block, optional `containerOverrides`** — the common case
  stays clean; sidecars are excluded or bounded independently when needed.
- **`behavior` block mirrors HPA UX** — stabilization windows, step limits and
  cooldowns per direction; asymmetric on purpose (scale up fast, down slow).
- **Percentile-based recommendations** — P90 CPU, P95 memory by default.

---

## HPA Coordination (Key Feature)

KubeRA does **not replace HPA** — it delays HPA activation by vertically
scaling Pods first, and it is careful never to destabilize HPA's control loop.

### The coupled-loop problem

HPA computes resource utilization as `usage / requests`. KubeRA changes
requests, so every resize changes what HPA sees:

- **Raising requests lowers utilization %** — HPA backs off. This is the
  vertical-first gating mechanism, and it is intentional.
- **Lowering requests raises utilization %** — done naively, shrinking
  resources after a traffic drop would make HPA scale *out*, causing a runaway
  oscillation (the exact reason VPA + HPA on the same metric is forbidden).

KubeRA solves this with **coupled-loop math**: before any downward resize it
reads the HPA spec and computes the utilization HPA will observe *after* the
change. It never lowers requests if the resulting utilization would exceed
`cpuTargetUtilization − reserveHeadroomPercent`. Resources are released
gradually, only when actual usage leaves enough headroom.

### Cooperative mode behaviour

- KubeRA owns vertical scaling; HPA owns horizontal scaling.
- KubeRA pauses resizing while HPA is actively scaling (detected via
  `lastScaleTime`, `desiredReplicas != currentReplicas`, and HPA conditions).
- After HPA stabilizes (`cooldownAfterHPA`), KubeRA resumes optimization.
- When Pods reach configured max resources, KubeRA stops resizing, sets
  `hpaState: AtMaxVertical`, and lets HPA take over.
- Recommended best practice: put HPA on custom/external metrics (RPS, latency,
  queue depth) to fully decouple the loops. KubeRA supports resource-metric
  HPAs via headroom math, and also understands `containerResource` metrics.

### Example

Initial Deployment: 2 replicas, CPU request 500m, limit 1000m.

Traffic rises: KubeRA steps CPU 500m → 700m → 900m → 1200m → 2000m (configured
max). Usage still above target → `hpaState: AtMaxVertical` → HPA scales
2 → 3 → 4 → 5 replicas.

Traffic drops: HPA scales replicas back down first; KubeRA then gradually
restores CPU/Memory toward baseline, headroom-checked at every step.

---

## In-place Resize: Constraints & Safety

Kubernetes 1.33+ realities KubeRA is designed around:

- Resizes go through the Pod **`resize` subresource** (`InPlacePodVerticalScaling`,
  beta and on by default in 1.33).
- **Memory limit decrease** requires `resizePolicy: RestartContainer`;
  disallowed for `NotRequired`. CPU decrease is fine.
- **QoS class is immutable** — resize math always preserves the Pod's original
  QoS class (Guaranteed stays requests == limits).
- Resizes can be **`Deferred`** (retried with backoff) or **`Infeasible`**
  (node too small — surfaced as a status condition; optional fallback to
  evict-and-recreate, which also wakes the cluster autoscaler).
- **`updateMode: InPlaceOrRecreate`** evicts pods (via the Eviction API,
  respecting PodDisruptionBudgets, one pod per workload per pass) when a
  resize is impossible in-place — an Infeasible resize, or a Guaranteed pod
  that must shrink memory. Requires `--enable-pod-webhook`: the webhook
  injects the recommendation into the replacement pod; without it the
  replacement would inherit the stale template resources, so KubeRA refuses
  and sets condition `ResizeInfeasible: RecreateRequiresWebhook`.
- Containers need `resizePolicy` set — a **one-time rollout at onboarding**.
- **OOMKill fast path** — on OOM, memory is bumped immediately, bypassing
  cooldowns.
- Memory shrink only with a wide margin over P99 working set; runtimes with
  fixed heaps (JVM `-Xmx`) should opt out per container.
- **Spec drift**: KubeRA patches Pods, not the Deployment template. New Pods
  (rollouts, scale-out) are born with template values and re-optimized; an
  optional **mutating admission webhook** (planned) injects the current
  recommendation at Pod creation so new Pods are born right-sized.
- Cluster-wide resize rate limiting and a per-node concurrency budget prevent
  thundering herds.
- KubeRA detects a **VPA targeting the same workload** and refuses to act
  (`Conflicted` condition) rather than fight over Pods.

---

## Architecture

Single binary, two cooperating loops (controller-runtime, leader-elected):

```
DynamicResource reconciler
  resolve selector → matched Deployments → list Pods
  guards: VPA conflict, HPA scaling, cooldowns, tolerance band (±10%)
  clamp: min/max, QoS-preserving, step limits, HPA headroom
  actuate: PATCH resize subresource (rate-limited)
  observe: PodResizePending / PodResizeInProgress conditions
  update status, emit Events, RequeueAfter(pollingInterval)

Recommender (background)
  scrape metrics provider → decaying histograms → per-container percentiles
  OOM watcher → immediate memory-bump signal
```

Project layout (Kubebuilder):

```
api/v1alpha1/            CRD types, CEL validation, webhooks
internal/controller/     DynamicResource reconciler
internal/recommender/    histograms, percentiles, OOM fast path
internal/metrics/        Provider interface: metricsserver/, prometheus/
internal/actuator/       resize client, feature detection, rate limiting
internal/coordination/   HPA reader, coupled-loop math, headroom calc
internal/webhook/        Pod mutating webhook (phase 2)
test/e2e/                kind 1.33+, real in-place resize assertions
charts/kubera/           Helm chart
```

---

## Installation

### Prerequisites

- Kubernetes **1.33+** — in-place resize needs the Pod `resize` subresource
  (`kubectl version` to check the server). On older clusters KubeRA still
  runs in recommend-only mode and reports `ResizeInfeasible: ClusterUnsupported`.
- **Metrics Server** installed (most managed clusters ship it; kind does not):

  ```sh
  kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
  # kind / self-signed kubelets only:
  kubectl -n kube-system patch deployment metrics-server --type=json \
    -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
  ```

### 1. Install from the GitHub registry (recommended)

Every tagged release publishes a multi-arch image (amd64 + arm64) and an OCI
Helm chart to GitHub Container Registry — nothing to build locally:

```sh
helm install kubera oci://ghcr.io/jineshnagori/charts/kubera \
  --version 0.1.0 \
  -n kubera-system --create-namespace
```

The chart's default image is `ghcr.io/jineshnagori/kubera`; pin a version with
`--set controllerManager.container.image.tag=v0.1.0`.

### 1b. Alternative: build from source

```sh
make docker-build docker-push IMG=<your-registry>/kubera:dev
helm install kubera dist/chart -n kubera-system --create-namespace \
  --set controllerManager.container.image.repository=<your-registry>/kubera \
  --set controllerManager.container.image.tag=dev
```

kind shortcut (no registry needed):

```sh
kind create cluster --name kubera
make docker-build IMG=kubera:dev
kind load docker-image kubera:dev --name kubera
helm install kubera dist/chart -n kubera-system --create-namespace \
  --set controllerManager.container.image.repository=kubera \
  --set controllerManager.container.image.tag=dev
```

Or kustomize: `make deploy IMG=<your-registry>/kubera:dev`.

### 2. Verify

```sh
kubectl -n kubera-system get pods
kubectl get crd dynamicresources.autoscaling.kubera.io
```

### 3. Test on a real workload

Deploy a deliberately oversized demo app:

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: demo-api
  labels: { app: demo-api }
spec:
  replicas: 2
  selector: { matchLabels: { app: demo-api } }
  template:
    metadata: { labels: { app: demo-api } }
    spec:
      containers:
      - name: app
        image: nginx:1.27
        resizePolicy:                     # required for in-place resize
        - { resourceName: cpu,    restartPolicy: NotRequired }
        - { resourceName: memory, restartPolicy: NotRequired }
        resources:
          requests: { cpu: 500m, memory: 512Mi }   # idle nginx needs ~5m
          limits:   { cpu: 1,    memory: 1Gi }
```

Start recommend-only (safe, touches nothing):

```yaml
apiVersion: autoscaling.kubera.io/v1alpha1
kind: DynamicResource
metadata:
  name: demo-api-policy
spec:
  selector:
    matchLabels: { app: demo-api }
  updateMode: "Off"
  resources:
    cpu:    { min: 50m,  max: 1000m }
    memory: { min: 64Mi, max: 1Gi }
```

Watch recommendations appear (1–2 polling intervals):

```sh
kubectl get dynamicresource demo-api-policy -o jsonpath='{.status.recommendations}' | jq
kubectl kubera diff          # or: make build-plugin && ./bin/kubectl-kubera diff
```

Then enable actuation and watch the live resize:

```sh
kubectl patch dynamicresource demo-api-policy --type=merge \
  -p '{"spec":{"updateMode":"InPlaceOnly"}}'

kubectl get pods -l app=demo-api \
  -o custom-columns='POD:.metadata.name,CPU:.spec.containers[0].resources.requests.cpu,MEM:.spec.containers[0].resources.requests.memory,RESTARTS:.status.containerStatuses[0].restartCount' -w
```

Requests step down toward usage (max 20%/step, 10m cooldown between
down-steps) with **RESTARTS staying 0** — that is the in-place resize.
Events tell the story:

```sh
kubectl describe dynamicresource demo-api-policy   # RecommendationUpdated, PodsResized
```

### Operator flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--prometheus-url` | (empty) | Enable `provider: Prometheus` (needs kube-state-metrics) |
| `--max-resizes-per-minute` | `30` | Cluster-wide resize rate cap |
| `--enable-pod-webhook` | `false` | Inject recommendations into new Pods (needs cert-manager; `--set webhook.enable=true --set certmanager.enable=true` in Helm) |

Full documentation: **https://kubera.jineshnagori.in** (installation,
DynamicResource reference, HPA coordination, troubleshooting). Grafana
dashboard: `dist/grafana/kubera-dashboard.json`.

### Uninstall

```sh
helm uninstall kubera -n kubera-system    # or: make undeploy
```
