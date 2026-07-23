# Troubleshooting

First stops, in order:

```sh
kubectl describe dynamicresource <name>          # conditions + events
kubectl -n kubera-system logs deploy/kubera-controller-manager
kubectl get pod <pod> -o jsonpath='{.status.qosClass}'
```

## No recommendations appear

- **Metrics Server missing or unready**: `kubectl top pods` must work.
  Condition `Ready: MetricsUnavailable` confirms.
- **Selector mismatch**: condition `Ready: NoMatch`. The selector matches
  **Deployment labels** (metadata), not Pod labels. Same namespace only.
- **Fresh pods**: metrics-server needs ~60s to report new Pods; during the
  gap KubeRA keeps the last known recommendations.

## Recommendations exist but Pods don't resize

| Symptom | Cause | Fix |
|---------|-------|-----|
| `updateMode: "Off"` | Working as designed | Switch to `InPlaceOnly` |
| Condition `ResizeInfeasible: ClusterUnsupported` | Cluster < 1.33 | Upgrade; recommendations still work |
| Container restarts on every resize | Missing `resizePolicy: NotRequired` | Add it to the workload template (one-time rollout) |
| Change within ±10% of current | Tolerance band (anti-flap) | Working as designed |
| Nothing for minutes after a resize | Per-direction cooldown (2m up / 10m down default) | `spec.behavior.*.cooldown` |
| `hpaState: Scaling` | HPA actively scaling; vertical actuation paused | Working as designed |
| BestEffort Pods never touched | Adding requests would change QoS class, forbidden in-place | Set initial requests in the template |

## Memory never shrinks (CPU does)

The most common surprise. Check QoS:

```sh
kubectl get pod <pod> -o jsonpath='{.status.qosClass}'
```

**`Guaranteed`** (requests == limits): the kubelet forbids in-place memory
*limit* decrease, and Guaranteed pins limit to request; memory cannot shrink
in-place at all. Options:

1. Run the workload **Burstable** (memory limit > request): requests then
   shrink freely, the limit stays.
2. `updateMode: InPlaceOrRecreate` + webhook: Pods are evicted one at a
   time and replacements start right-sized.

## `ResizeInfeasible: RecreateRequiresWebhook`

`InPlaceOrRecreate` wants to evict, but the pod webhook is disabled.
Recreating without it is pointless: the replacement inherits the stale
template resources. Enable:

```sh
helm upgrade kubera ... --set webhook.enable=true --set certmanager.enable=true
```

cert-manager must be installed.

## Manager crash-loops: `open /tmp/k8s-webhook-server/serving-certs/tls.crt`

Webhook flag on but serving certificate absent. Both `webhook.enable=true`
**and** `certmanager.enable=true` are required, and cert-manager takes a few
seconds to issue on first install; one pod restart during that window is
normal.

## `events.k8s.io is forbidden` in logs

RBAC from a chart older than v0.1.1. Upgrade the chart; the role needs
`events.k8s.io` create/patch.

## Pods OOMKill after a resize down

Should not happen from v0.1.1 on: memory recommendations are floored at
**32Mi** regardless of `spec.resources.memory.min`, precisely because on
Guaranteed pods the recommendation becomes the memory limit. If you see it:

- Check for an `OOMFastPath` event: KubeRA bumps memory 1.5× immediately
  after an OOMKill, bypassing cooldowns.
- Raise `memory.min` to an honest baseline for the workload.
- JVM/fixed-heap workloads: pin memory (`min == max`) and let KubeRA manage
  CPU only.

## Prometheus provider not working

- Operator must run with `--prometheus-url`; otherwise the policy gets
  `Ready: UnsupportedProvider`.
- kube-state-metrics must be scraped (the pod-label join uses
  `kube_pod_labels`).
- Only `matchLabels` selectors work with Prometheus: `matchExpressions`
  cannot be expressed in the PromQL join.

## Two policies fighting

Condition `Conflicted: OverlappingSelector`: two DynamicResources match the
same Deployment. Oldest wins; the newer takes no action. Tighten the
selectors.

`Conflicted: VPAConflict`: a VerticalPodAutoscaler targets the workload.
Remove one of the two; KubeRA will not fight VPA over the same Pods.
