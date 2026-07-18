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
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
	"github.com/jineshnagori/kubera/internal/actuator"
)

const (
	defaultScaleUpCooldown   = 2 * time.Minute
	defaultScaleDownCooldown = 10 * time.Minute
)

// actuate applies recommendations to the Pods of matched workloads via
// in-place resize. Returns the number of resized Pods and the names of Pods
// whose last resize is Infeasible.
func (r *DynamicResourceReconciler) actuate(ctx context.Context, dr *autoscalingv1alpha1.DynamicResource, matched []appsv1.Deployment, recs []autoscalingv1alpha1.WorkloadRecommendation) (int, []string, error) {
	log := logf.FromContext(ctx)
	policy := actuator.DefaultPolicy(&dr.Spec)
	now := time.Now()

	recByWorkload := map[string]*autoscalingv1alpha1.WorkloadRecommendation{}
	for i := range recs {
		recByWorkload[recs[i].Workload] = &recs[i]
	}

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

		pods, err := r.listWorkloadPods(ctx, dr.Namespace, deploy)
		if err != nil {
			return resized, infeasible, err
		}

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

			plan := actuator.PlanPod(pod, targets, policy)
			if plan == nil {
				continue
			}
			if !r.Cooldowns.Allow(cooldownKey, plan.Direction, cooldownFor(&dr.Spec, plan.Direction), now) {
				continue
			}

			if err := r.Resizer.Apply(ctx, dr.Namespace, plan); err != nil {
				return resized, infeasible, err
			}
			r.Cooldowns.Record(cooldownKey, plan.Direction, now)
			resized++
			log.V(1).Info("resized pod", "pod", pod.Name, "direction", plan.Direction)
		}
	}

	if resized > 0 {
		dr.Status.LastResizeTime = &metav1.Time{Time: now}
		markApplied(dr, recByWorkload)
		r.Recorder.Eventf(dr, nil, "Normal", "PodsResized", "Reconcile",
			"resized %d pod(s) in-place", resized)
	}
	return resized, infeasible, nil
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
