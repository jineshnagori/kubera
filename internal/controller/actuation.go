/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
	"github.com/jineshnagori/kubera/internal/actuator"
	"github.com/jineshnagori/kubera/internal/coordination"
)

const (
	defaultScaleUpCooldown        = 2 * time.Minute
	defaultScaleDownCooldown      = 10 * time.Minute
	defaultCooldownAfterHPA       = 10 * time.Minute
	defaultReserveHeadroomPercent = int32(10)
)

// actuate applies recommendations to the Pods of matched workloads via
// in-place resize, honouring HPA coordination. Returns the number of resized
// Pods and the names of Pods whose last resize is Infeasible. Sets
// status.hpaState as a side effect.
func (r *DynamicResourceReconciler) actuate(ctx context.Context, dr *autoscalingv1alpha1.DynamicResource, matched []appsv1.Deployment, recs []autoscalingv1alpha1.WorkloadRecommendation, usage map[string]map[string]ContainerUsageSnapshot) (int, []string, error) {
	log := logf.FromContext(ctx)
	policy := actuator.DefaultPolicy(&dr.Spec)
	now := time.Now()

	recByWorkload := map[string]*autoscalingv1alpha1.WorkloadRecommendation{}
	for i := range recs {
		recByWorkload[recs[i].Workload] = &recs[i]
	}

	hpaState := autoscalingv1alpha1.HPAStateIdle
	resized := 0
	var infeasible []string
	for i := range matched {
		deploy := &matched[i]
		rec, ok := recByWorkload[deploy.Name]
		if !ok {
			continue
		}

		targets := map[string]corev1.ResourceList{}
		for _, c := range rec.Containers {
			targets[c.ContainerName] = c.Target
		}

		state, err := r.coordinateHPA(ctx, dr, deploy.Name, targets, usage[deploy.Name], now)
		if err != nil {
			return resized, infeasible, err
		}
		hpaState = worseHPAState(hpaState, state)
		if state == autoscalingv1alpha1.HPAStateScaling {
			log.V(1).Info("HPA actively scaling; vertical actuation paused", "workload", deploy.Name)
			continue
		}

		pods, err := r.listWorkloadPods(ctx, dr.Namespace, deploy)
		if err != nil {
			return resized, infeasible, err
		}

		publishReclaimableMetrics(dr, deploy.Name, pods, targets)

		cooldownKey := dr.Namespace + "/" + deploy.Name
		for j := range pods {
			pod := &pods[j]
			if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
				continue
			}
			if podResizeInfeasible(pod) {
				infeasible = append(infeasible, pod.Name)
				continue
			}

			// OOM fast path: bump memory immediately, bypassing tolerance,
			// step caps and cooldowns — waiting means repeated OOMKills.
			if oomContainers := actuator.OOMKilledContainers(pod, now); len(oomContainers) > 0 {
				if plan := actuator.PlanOOMBump(pod, oomContainers, maxMemoryBounds(&dr.Spec, oomContainers), policy); plan != nil {
					if err := r.Resizer.Apply(ctx, dr.Namespace, plan); err != nil {
						if errors.Is(err, actuator.ErrRateLimited) {
							continue
						}
						return resized, infeasible, err
					}
					resized++
					resizesTotal.WithLabelValues(dr.Namespace, deploy.Name, string(actuator.DirectionUp)).Inc()
					r.Recorder.Eventf(dr, nil, "Warning", "OOMFastPath", "Reconcile",
						"bumped memory of pod %s after OOMKill (containers: %v)", pod.Name, oomContainers)
					continue
				}
			}

			plan := actuator.PlanPod(pod, targets, policy)
			if plan == nil {
				continue
			}
			if !r.Cooldowns.Allow(cooldownKey, plan.Direction, cooldownFor(&dr.Spec, plan.Direction), now) {
				continue
			}

			if err := r.Resizer.Apply(ctx, dr.Namespace, plan); err != nil {
				if errors.Is(err, actuator.ErrRateLimited) {
					log.V(1).Info("resize skipped: cluster-wide rate limit", "pod", pod.Name)
					continue
				}
				return resized, infeasible, err
			}
			r.Cooldowns.Record(cooldownKey, plan.Direction, now)
			resized++
			resizesTotal.WithLabelValues(dr.Namespace, deploy.Name, string(plan.Direction)).Inc()
			log.V(1).Info("resized pod", "pod", pod.Name, "direction", plan.Direction)
		}
	}

	dr.Status.HPAState = hpaState
	if resized > 0 {
		dr.Status.LastResizeTime = &metav1.Time{Time: now}
		markApplied(dr, recByWorkload)
		r.Recorder.Eventf(dr, nil, "Normal", "PodsResized", "Reconcile",
			"resized %d pod(s) in-place", resized)
	}
	return resized, infeasible, nil
}

// coordinateHPA applies the coupled-loop guards for one workload:
//   - pause vertical actuation while the HPA is actively scaling;
//   - floor downward CPU/memory targets so the utilization the HPA observes
//     after the resize stays below target − headroom (never re-trigger HPA
//     on scale-down);
//   - report AtMaxVertical when the CPU recommendation is pinned at the
//     configured max, signalling HPA takeover.
//
// targets is mutated in place.
func (r *DynamicResourceReconciler) coordinateHPA(ctx context.Context, dr *autoscalingv1alpha1.DynamicResource, workload string, targets map[string]corev1.ResourceList, usage map[string]ContainerUsageSnapshot, now time.Time) (autoscalingv1alpha1.HPAState, error) {
	cfg := dr.Spec.HPA
	if cfg != nil && cfg.Mode == autoscalingv1alpha1.HPAModeIgnore {
		return autoscalingv1alpha1.HPAStateIdle, nil
	}

	hpa, err := coordination.FindHPA(ctx, r.Client, dr.Namespace, workload)
	if err != nil {
		return autoscalingv1alpha1.HPAStateIdle, err
	}
	if hpa == nil {
		return autoscalingv1alpha1.HPAStateIdle, nil
	}

	settle := defaultCooldownAfterHPA
	pause := true
	headroom := defaultReserveHeadroomPercent
	if cfg != nil {
		if cfg.CooldownAfterHPA != nil {
			settle = cfg.CooldownAfterHPA.Duration
		}
		if cfg.PauseWhileHPAScaling != nil {
			pause = *cfg.PauseWhileHPAScaling
		}
		if cfg.ReserveHeadroomPercent > 0 {
			headroom = cfg.ReserveHeadroomPercent
		}
	}

	if pause && coordination.IsScaling(hpa, settle, now) {
		return autoscalingv1alpha1.HPAStateScaling, nil
	}

	cpuTarget, memTarget := coordination.TargetUtilization(hpa)
	atMax := len(targets) > 0
	for name, target := range targets {
		u, ok := usage[name]
		if ok {
			if floor := coordination.RequestFloor(u.CPUMilli, cpuTarget, headroom); floor > 0 {
				cur := target[corev1.ResourceCPU]
				if cur.MilliValue() < floor {
					target[corev1.ResourceCPU] = *resource.NewMilliQuantity(floor, resource.DecimalSI)
				}
			}
			if floor := coordination.RequestFloor(u.MemBytes, memTarget, headroom); floor > 0 {
				cur := target[corev1.ResourceMemory]
				if cur.Value() < floor {
					target[corev1.ResourceMemory] = *resource.NewQuantity(floor, resource.BinarySI)
				}
			}
		}

		_, maxCPU := containerBounds(&dr.Spec, name)
		cpu := target[corev1.ResourceCPU]
		if maxCPU.IsZero() || cpu.Cmp(maxCPU) < 0 {
			atMax = false
		}
	}
	if atMax {
		return autoscalingv1alpha1.HPAStateAtMaxVertical, nil
	}
	return autoscalingv1alpha1.HPAStateIdle, nil
}

// maxMemoryBounds returns the effective memory max per container.
func maxMemoryBounds(spec *autoscalingv1alpha1.DynamicResourceSpec, containers []string) map[string]resource.Quantity {
	out := map[string]resource.Quantity{}
	for _, name := range containers {
		bounds := &spec.Resources.Memory
		if o := findOverride(spec.ContainerOverrides, name); o != nil && o.Memory != nil {
			bounds = o.Memory
		}
		out[name] = bounds.Max
	}
	return out
}

// containerBounds returns the effective CPU min/max for a container.
func containerBounds(spec *autoscalingv1alpha1.DynamicResourceSpec, container string) (minCPU, maxCPU resource.Quantity) {
	bounds := &spec.Resources.CPU
	if o := findOverride(spec.ContainerOverrides, container); o != nil && o.CPU != nil {
		bounds = o.CPU
	}
	return bounds.Min, bounds.Max
}

func worseHPAState(a, b autoscalingv1alpha1.HPAState) autoscalingv1alpha1.HPAState {
	rank := map[autoscalingv1alpha1.HPAState]int{
		autoscalingv1alpha1.HPAStateIdle:          0,
		autoscalingv1alpha1.HPAStateAtMaxVertical: 1,
		autoscalingv1alpha1.HPAStateScaling:       2,
	}
	if rank[b] > rank[a] {
		return b
	}
	return a
}

// listWorkloadPods lists the Pods belonging to a Deployment via its pod
// selector, namespace-scoped.
func (r *DynamicResourceReconciler) listWorkloadPods(ctx context.Context, namespace string, deploy *appsv1.Deployment) ([]corev1.Pod, error) {
	sel, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("workload %s: invalid pod selector: %w", deploy.Name, err)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}
	return pods.Items, nil
}

// podResizeInfeasible reports whether the Pod's pending resize was rejected
// as Infeasible (node cannot fit it); retrying without a topology change is
// pointless.
func podResizeInfeasible(pod *corev1.Pod) bool {
	for _, cond := range pod.Status.Conditions {
		if cond.Type == corev1.PodResizePending && cond.Reason == "Infeasible" {
			return true
		}
	}
	return false
}

func cooldownFor(spec *autoscalingv1alpha1.DynamicResourceSpec, d actuator.Direction) time.Duration {
	if b := spec.Behavior; b != nil {
		if d == actuator.DirectionUp && b.ScaleUp != nil && b.ScaleUp.Cooldown != nil {
			return b.ScaleUp.Cooldown.Duration
		}
		if d == actuator.DirectionDown && b.ScaleDown != nil && b.ScaleDown.Cooldown != nil {
			return b.ScaleDown.Cooldown.Duration
		}
	}
	if d == actuator.DirectionUp {
		return defaultScaleUpCooldown
	}
	return defaultScaleDownCooldown
}

// markApplied records the actuated targets in status.
func markApplied(dr *autoscalingv1alpha1.DynamicResource, recByWorkload map[string]*autoscalingv1alpha1.WorkloadRecommendation) {
	for wi := range dr.Status.Recommendations {
		w := &dr.Status.Recommendations[wi]
		if _, ok := recByWorkload[w.Workload]; !ok {
			continue
		}
		for ci := range w.Containers {
			w.Containers[ci].Applied = w.Containers[ci].Target
		}
	}
}
