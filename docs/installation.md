# Installation

## Prerequisites

| Requirement | Why | Check |
|-------------|-----|-------|
| Kubernetes **1.33+** | In-place resize goes through the Pod `resize` subresource (`InPlacePodVerticalScaling`, on by default since 1.33) | `kubectl version` |
| **Metrics Server** | Default usage metrics source | `kubectl top pods` |
| Helm 3.8+ | OCI registry support | `helm version` |
| cert-manager *(optional)* | Only for the pod mutating webhook | `kubectl get crd certificates.cert-manager.io` |

!!! note "Older clusters"
    On < 1.33 KubeRA still runs: recommendations are computed and published,
    actuation is disabled with condition
    `ResizeInfeasible: ClusterUnsupported`.

Metrics Server missing (kind clusters, some bare-metal):

```sh
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
# kind / self-signed kubelet certs only:
kubectl -n kube-system patch deployment metrics-server --type=json \
  -p '[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
```

## Install from the GitHub registry (recommended)

Every release publishes a multi-arch image (amd64 + arm64) and an OCI Helm
chart:

```sh
helm install kubera oci://ghcr.io/jineshnagori/charts/kubera \
  --version 0.1.1 \
  -n kubera-system --create-namespace
```

Verify:

```sh
kubectl -n kubera-system get pods
kubectl get crd dynamicresources.autoscaling.kubera.io
```

## Enable the pod webhook (optional, recommended for production)

The mutating webhook injects the current recommendation into **new** Pods at
admission, so replicas created by rollouts or HPA scale-out are born
right-sized. It is also **required** for `updateMode: InPlaceOrRecreate`.

Needs cert-manager installed, then:

```sh
helm upgrade kubera oci://ghcr.io/jineshnagori/charts/kubera \
  --version 0.1.1 -n kubera-system \
  --set webhook.enable=true \
  --set certmanager.enable=true
```

This deploys the `MutatingWebhookConfiguration`, webhook Service, serving
certificate, and passes `--enable-pod-webhook` to the manager. The webhook
uses `failurePolicy: Ignore`: Pod creation is never blocked by KubeRA being
down.

## Operator flags

| Flag | Default | Purpose |
|------|---------|---------|
| `--prometheus-url` | (empty) | Enables `provider: Prometheus`; requires kube-state-metrics (the `kube_pod_labels` join) |
| `--max-resizes-per-minute` | `30` | Cluster-wide resize rate cap (thundering-herd protection) |
| `--enable-pod-webhook` | `false` | Register the pod mutating webhook (set automatically by `webhook.enable=true`) |

Set extra flags via Helm:

```sh
--set 'controllerManager.container.args={--leader-elect,--metrics-bind-address=:8443,--health-probe-bind-address=:8081,--prometheus-url=http://prometheus.monitoring:9090}'
```

## Build from source

```sh
git clone https://github.com/jineshnagori/kubera && cd kubera
make docker-build docker-push IMG=<your-registry>/kubera:dev
helm install kubera dist/chart -n kubera-system --create-namespace \
  --set controllerManager.container.image.repository=<your-registry>/kubera \
  --set controllerManager.container.image.tag=dev
```

kind shortcut (no registry):

```sh
kind create cluster --name kubera
make docker-build IMG=kubera:dev
kind load docker-image kubera:dev --name kubera
helm install kubera dist/chart -n kubera-system --create-namespace \
  --set controllerManager.container.image.repository=kubera \
  --set controllerManager.container.image.tag=dev
```

## First workload: safe adoption path

**1.** Add `resizePolicy` to the workload (one-time rollout; without it the
kubelet restarts containers on resize):

```yaml
containers:
- name: app
  resizePolicy:
  - { resourceName: cpu,    restartPolicy: NotRequired }
  - { resourceName: memory, restartPolicy: NotRequired }
```

**2.** Create a recommend-only policy:

```yaml
apiVersion: autoscaling.kubera.io/v1alpha1
kind: DynamicResource
metadata:
  name: api-policy
  namespace: production
spec:
  selector:
    matchLabels: { app: api }
  updateMode: "Off"
  resources:
    cpu:    { min: 100m,  max: 2000m }
    memory: { min: 128Mi, max: 4Gi }
```

**3.** Watch for a few days:

```sh
kubectl get dynamicresource api-policy -o yaml   # status.recommendations
kubectl kubera diff -n production                # current vs recommended
```

**4.** Actuate:

```sh
kubectl patch dynamicresource api-policy --type=merge \
  -p '{"spec":{"updateMode":"InPlaceOnly"}}'
```

Watch it live: requests converge, `RESTARTS` stays 0:

```sh
kubectl get pods -l app=api -w \
  -o custom-columns='POD:.metadata.name,CPU:.spec.containers[0].resources.requests.cpu,MEM:.spec.containers[0].resources.requests.memory,RESTARTS:.status.containerStatuses[0].restartCount'
```

## Uninstall

```sh
helm uninstall kubera -n kubera-system
kubectl delete crd dynamicresources.autoscaling.kubera.io   # removes all policies
```
