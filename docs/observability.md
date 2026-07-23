# Observability

## Prometheus metrics

Exposed on the manager's metrics endpoint (`:8443`, authenticated by
default). Recommendation gauges are published in **every** updateMode
including `"Off"`: cost visibility before any actuation.

| Metric | Type | Labels | Meaning |
|--------|------|--------|---------|
| `kubera_recommended_cpu_millicores` | gauge | namespace, dynamicresource, workload, container | Current recommended CPU request |
| `kubera_recommended_memory_bytes` | gauge | namespace, dynamicresource, workload, container | Current recommended memory request |
| `kubera_reclaimable_cpu_millicores` | gauge | namespace, dynamicresource, workload | CPU requested by running Pods **above** the recommendation: direct savings input |
| `kubera_reclaimable_memory_bytes` | gauge | namespace, dynamicresource, workload | Same for memory |
| `kubera_pod_resizes_total` | counter | namespace, workload, direction | In-place resizes (and recreates) performed |

Estimated monthly savings, assuming a $/core-month price:

```promql
sum(kubera_reclaimable_cpu_millicores) / 1000 * 24.0   # cores * your $/core-month
```

## Grafana dashboard

Ready-made dashboard at
[`dist/grafana/kubera-dashboard.json`](https://github.com/jineshnagori/kubera/blob/main/dist/grafana/kubera-dashboard.json):
reclaimable CPU/memory stats, per-workload recommendation trends, resize rate
by direction. Import it and point the datasource variable at your Prometheus.

## Events

All on the DynamicResource object:

```sh
kubectl describe dynamicresource api-policy
```

| Event | Type | When |
|-------|------|------|
| `RecommendationUpdated` | Normal | Recommendations changed |
| `PodsResized` | Normal | One or more Pods actuated this pass |
| `PodEvictedForResize` | Normal | InPlaceOrRecreate evicted a Pod |
| `OOMFastPath` | Warning | Memory bumped immediately after an OOMKill |
| `Conflicted` | Warning | Overlapping DynamicResource |
| `VPAConflict` | Warning | VPA targets the same workload |

## kubectl plugin

```sh
make build-plugin && sudo mv bin/kubectl-kubera /usr/local/bin/
kubectl kubera diff                # all namespaces
kubectl kubera diff -n production
```

```text
NAMESPACE   WORKLOAD  CONTAINER  CURRENT CPU  RECOMMENDED CPU  CURRENT MEMORY  RECOMMENDED MEMORY
production  api       app        1000m        480m             2Gi             610Mi
```

The gap between the columns is your money.

## Status at a glance

```sh
kubectl get dynamicresources -A
```

```text
NAMESPACE    NAME         MODE          HPA STATE  READY  AGE
production   api-policy   InPlaceOnly   Idle       True   3d
payments     team-policy  Off           Idle       True   12d
```
