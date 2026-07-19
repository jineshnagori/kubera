# KubeRA — Kubernetes Resource Allocator

**Vertical-first, horizontal-second autoscaling.** KubeRA continuously
right-sizes Pod CPU and memory using Kubernetes **in-place Pod resize** — no
restarts — and cooperates with the Horizontal Pod Autoscaler instead of
fighting it.

```text
Traffic rises
  → KubeRA grows Pod resources in place (no restart, no new replicas)
  → Pods reach their configured max
  → HPA takes over and scales replicas

Traffic falls
  → HPA scales replicas back down
  → KubeRA releases resources gradually (never re-triggering HPA)
```

## Why

Most teams over-provision:

```yaml
requests: { cpu: 1000m, memory: 2Gi }   # reserved
# actual usage: 120m CPU, 400Mi memory  # used
```

The scheduler reserves what is requested, not what is used. The result is
half-empty nodes and a cloud bill for idle capacity. Under-provisioning is
worse: throttling and OOMKills.

KubeRA moves every workload toward its real usage — fewer nodes at higher
utilization, without sacrificing performance.

## How it differs from VPA

| | VPA | KubeRA |
|---|-----|--------|
| Resize method | Evict-and-recreate (in-place is alpha) | **In-place first**, eviction only as opt-in fallback |
| HPA on CPU/memory | Explicitly forbidden | **Designed for it** — coupled-loop math keeps HPA stable |
| Adoption path | `updateMode: Off` | Same, plus `kubectl kubera diff` and cost metrics |
| Sidecars | containerPolicies | `containerOverrides`, injected sidecars included |

## Quick start

```sh
helm install kubera oci://ghcr.io/jineshnagori/charts/kubera \
  --version 0.1.0 -n kubera-system --create-namespace
```

```yaml
apiVersion: autoscaling.kubera.io/v1alpha1
kind: DynamicResource
metadata:
  name: api-policy
  namespace: production
spec:
  selector:
    matchLabels: { app: api }
  updateMode: "Off"        # recommend-only: watch before you trust
  resources:
    cpu:    { min: 100m,  max: 2000m }
    memory: { min: 128Mi, max: 4Gi }
```

```sh
kubectl get dynamicresource api-policy -o jsonpath='{.status.recommendations}' | jq
```

When the numbers look right, switch `updateMode` to `InPlaceOnly` and watch
requests converge with `restartCount` staying at 0.

Continue with [Installation](installation.md) and the
[DynamicResource reference](dynamicresource.md).
