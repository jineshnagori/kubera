# Getting Started

## Prerequisites

- Kubernetes **1.33+** (in-place Pod resize via the `resize` subresource)
- Metrics Server (or Prometheus + kube-state-metrics)
- Helm 3

## Install

From the GitHub registry (published on every tagged release):

```sh
helm install kubera oci://ghcr.io/jineshnagori/charts/kubera \
  --version 0.1.0 -n kubera-system --create-namespace
```

From source:

```sh
helm install kubera ./dist/chart -n kubera-system --create-namespace
```

Operator flags (set via Helm values / manager args):

| Flag | Default | Purpose |
|------|---------|---------|
| `--prometheus-url` | (empty) | Enables `provider: Prometheus`; needs kube-state-metrics for the `kube_pod_labels` join |
| `--max-resizes-per-minute` | `30` | Cluster-wide resize rate cap |
| `--enable-pod-webhook` | `false` | Inject recommendations into new Pods at admission (needs cert-manager) |

## First DynamicResource — recommend-only

Start every workload in `updateMode: "Off"`. KubeRA publishes recommendations
in status without touching Pods:

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
  updateMode: "Off"
  resources:
    cpu:    { min: 100m,  max: 2000m }
    memory: { min: 128Mi, max: 4Gi }
```

Inspect what KubeRA would do:

```sh
kubectl get dynamicresource api-policy -o yaml   # status.recommendations
kubectl kubera diff -n production                # current vs recommended
```

## Enable actuation

Once the recommendations look sane for a week or two:

1. Add `resizePolicy` to the workload's containers (one-time rollout):

   ```yaml
   resizePolicy:
   - resourceName: cpu
     restartPolicy: NotRequired
   - resourceName: memory
     restartPolicy: NotRequired
   ```

2. Switch the DynamicResource to `updateMode: InPlaceOnly`.

KubeRA now resizes Pods in place — step-capped, cooldown-limited, QoS-safe,
and HPA-aware.

## Observability

- Prometheus metrics on the manager's metrics endpoint:
  `kubera_recommended_cpu_millicores`, `kubera_recommended_memory_bytes`,
  `kubera_reclaimable_cpu_millicores`, `kubera_reclaimable_memory_bytes`,
  `kubera_pod_resizes_total`
- Grafana dashboard: `dist/grafana/kubera-dashboard.json`
- Events on the DynamicResource: `RecommendationUpdated`, `PodsResized`,
  `OOMFastPath`, `Conflicted`, `VPAConflict`
