# Benchmarking KubeRA + HPA under load

A methodology for measuring KubeRA's behavior on a real cluster under load:
what to run, what to grep from the controller-manager log, and how to read
the numbers. Every command below works against any KubeRA installation: pair
it with
[`examples/demo.yml`](https://github.com/jineshnagori/kubera/blob/main/examples/demo.yml)
or your own workload.

## Setup

- Workload: any CPU-load-generating image works for this exercise. The
  canonical choice is `registry.k8s.io/hpa-example`, which burns CPU
  proportional to request rate: idle static content (e.g. plain nginx) won't
  move under a curl loop and can't demonstrate scaling.
- `DynamicResource`: `updateMode: InPlaceOrRecreate` (or `InPlaceOnly`),
  `cpu`/`memory` bounds sized to the workload, short cooldowns and polling
  interval for a faster feedback loop during testing (defaults are slower and
  intended for production).
- `HorizontalPodAutoscaler`: a CPU utilization target, with `cpu.max` on the
  `DynamicResource` kept under the request level that trips that target: see
  [HPA Coordination](hpa-coordination.md) for how the two interact.
- Load generator, run **inside** the cluster (a ClusterIP Service's DNS name
  doesn't resolve from outside it):
  ```sh
  kubectl run load-gen --rm -it --restart=Never --image=curlimages/curl -- \
    sh -c 'for i in $(seq 1 10000); do
      curl -s -o /dev/null -w "%{http_code}\n" http://<service>/
      sleep 0.0001
    done | sort | uniq -c'
  ```
  Run it in waves over tens of minutes to see both scale-up and scale-down
  behavior, not just one spike.

## Capture the data

```sh
kubectl apply -f examples/demo.yml
kubectl -n kubera-system logs -f deploy/kubera-controller-manager > run.log &
# drive load with the loop above, in waves
kubectl get dynamicresource <name> -w
kubectl get hpa <name> -w
```

Let it run long enough to see multiple resize cooldowns elapse in both
directions, then analyze `run.log`.

## Headline numbers to pull

```sh
grep -c "actuation pass" run.log                              # total reconcile passes
grep -oP '"resized": \K[0-9]+' run.log | sort -n | uniq -c     # how many pods per pass
grep -c "OOMFastPath" run.log                                  # OOM fast-path bumps
grep '"infeasible": [1-9]' run.log | wc -l                      # infeasible resizes
grep -c "ERROR" run.log                                         # controller errors
grep -c "forbidden\|rejected event" run.log                     # RBAC issues
grep -c "Starting manager" run.log                              # unexpected restarts
```

| Metric | What a healthy run looks like |
|--------|-------------------------------|
| Passes that resized 0 pods | The large majority (tolerance band + cooldowns absorbing noise between real changes) |
| Passes that resized ≥1 pod | Correlates with load transitions, not with every poll tick |
| OOM fast-path bumps | Only during a genuine memory spike; each should be followed by the request staying raised, not repeating indefinitely |
| Infeasible resizes | 0 unless a node is genuinely undersized for the recommendation |
| Controller errors | 0 |
| RBAC rejections | 0 (if present, the Helm chart's RBAC needs an upgrade) |
| Manager restarts | 0 during the test window |

A high no-op ratio is the number that answers "will this thrash under a
chatty poll interval?"; it shouldn't, and this is how you prove it on your
own cluster.

## Reading the CPU/memory trend

Pull the recommendation over time and compare consecutive values:

```sh
kubectl get dynamicresource <name> -o jsonpath='{.status.recommendations}'
```

Each step up should respect `behavior.scaleUp.maxStepPercent`; each step down
should respect `behavior.scaleDown.maxStepPercent` and be slower than the
climb (scale-down is deliberately more conservative by default). Cross-check
against `kubectl get pods -w` with `RESTARTS` in the output: a healthy
in-place resize never increments it.

## Checking the OOM fast path

```sh
grep "OOMFastPath" run.log
```

If a pod's memory request had shrunk during a quiet period and a sudden load
spike OOMKills it, KubeRA should bump memory 1.5× **within the same
reconcile** that observes the kill, not on the next poll tick. Confirm the
timestamp of the bump matches the OOMKill, not several intervals later.

## Checking batched resizes

When multiple pods of one workload need a resize in the same pass, they
should all move together: grep one `reconcileID` and count `"direction"`
log lines plus any `OOMFastPath` events against the pass's `"resized": N`
total; they should add up. If the first pod's cooldown were blocking its
siblings, you'd see `resized: 1` repeated across consecutive passes instead
of one batched pass.

## Checking the vertical-first handoff

Watch `status.hpaState` across the run. `AtMaxVertical` should appear only
once the CPU recommendation is pinned at `spec.resources.cpu.max` and demand
is still above target: that's the intended trigger for HPA to take over
replica scaling.

If HPA scales out *before* `AtMaxVertical` appears, the two thresholds are
tuned too close together for the load pattern: HPA's utilization target is
tripping on the still-climbing (lower) request value faster than KubeRA's
step-capped, cooldown-gated recommendation can reach `cpu.max`. This is a
tuning issue, not a coordination bug: the fix is to raise `cpu.max` relative
to the HPA target, lower the HPA target, or shorten the scale-up cooldown so
KubeRA's climb keeps pace with the load. Regardless of tuning, verify
separately that every *downward* resize respects the headroom floor: zero
cases of KubeRA's own shrink re-triggering an HPA scale-out is the property
that actually matters (see [HPA Coordination](hpa-coordination.md)).

## Interpreting the result

If your numbers show a high no-op reconcile ratio, zero infeasible resizes,
zero errors, zero RBAC rejections, resize batching that adds up, and an OOM
fast path (if exercised) that fires within the same reconcile as the kill;
the installation is healthy. Anything short of that on your own cluster is
worth investigating before relying on `InPlaceOnly` or `InPlaceOrRecreate` in
production; run `updateMode: "Off"` for longer and compare recommendations
against real usage first.
