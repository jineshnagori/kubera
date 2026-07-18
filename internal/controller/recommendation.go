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
	"math"
	"slices"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
	"github.com/jineshnagori/kubera/internal/recommender"
)

const (
	// safetyMarginFactor is applied on top of the percentile readout so the
	// recommendation leaves headroom above observed usage (VPA uses 15%).
	safetyMarginFactor = 1.10

	defaultCPUPercentile = 90
	defaultMemPercentile = 95

	// Rounding granularity keeps status stable across reconciles: histogram
	// buckets are ~5% wide, so 10m / 1Mi steps never oscillate.
	cpuRoundingMilli = 10
	memRoundingBytes = 1024 * 1024
)

// computeRecommendations feeds current usage samples into the recommender and
// builds the per-workload recommendation list for status.
func (r *DynamicResourceReconciler) computeRecommendations(ctx context.Context, dr *autoscalingv1alpha1.DynamicResource, matched []appsv1.Deployment) ([]autoscalingv1alpha1.WorkloadRecommendation, error) {
	cpuPct, memPct := percentiles(dr.Spec.Metrics)

	var out []autoscalingv1alpha1.WorkloadRecommendation
	for i := range matched {
		deploy := &matched[i]

		podSelector, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
		if err != nil {
			continue
		}
		usages, err := r.Metrics.ListPodUsage(ctx, dr.Namespace, podSelector)
		if err != nil {
			return nil, fmt.Errorf("workload %s: %w", deploy.Name, err)
		}

		containers := map[string]bool{}
		for _, pod := range usages {
			for _, cu := range pod.Containers {
				key := recommender.ContainerKey{Namespace: dr.Namespace, Workload: deploy.Name, Container: cu.Container}
				r.Recommender.Observe(key, pod.Pod, float64(cu.CPU.MilliValue()), float64(cu.Memory.Value()), pod.Timestamp)
				containers[cu.Container] = true
			}
		}

		rec := autoscalingv1alpha1.WorkloadRecommendation{Workload: deploy.Name}
		for _, name := range sortedKeys(containers) {
			override := findOverride(dr.Spec.ContainerOverrides, name)
			if override != nil && override.Mode == autoscalingv1alpha1.ContainerModeOff {
				continue
			}

			key := recommender.ContainerKey{Namespace: dr.Namespace, Workload: deploy.Name, Container: name}
			raw, ok := r.Recommender.Recommend(key, cpuPct, memPct)
			if !ok {
				continue
			}

			cpuBounds, memBounds := boundsFor(&dr.Spec.Resources, override)
			cpu := clampQuantity(
				roundUpMilli(raw.CPUMilli*safetyMarginFactor, cpuRoundingMilli),
				cpuBounds,
			)
			mem := clampQuantity(
				roundUpBytes(raw.MemBytes*safetyMarginFactor, memRoundingBytes),
				memBounds,
			)

			rec.Containers = append(rec.Containers, autoscalingv1alpha1.ContainerRecommendation{
				ContainerName: name,
				Target: corev1.ResourceList{
					corev1.ResourceCPU:    cpu,
					corev1.ResourceMemory: mem,
				},
			})
		}

		if len(rec.Containers) > 0 {
			out = append(out, rec)
		}
	}

	slices.SortFunc(out, func(a, b autoscalingv1alpha1.WorkloadRecommendation) int {
		return strings.Compare(a.Workload, b.Workload)
	})
	return out, nil
}

func percentiles(m *autoscalingv1alpha1.MetricsConfig) (cpu, mem float64) {
	cpu, mem = defaultCPUPercentile/100.0, defaultMemPercentile/100.0
	if m == nil {
		return cpu, mem
	}
	if m.CPUPercentile > 0 {
		cpu = float64(m.CPUPercentile) / 100.0
	}
	if m.MemoryPercentile > 0 {
		mem = float64(m.MemoryPercentile) / 100.0
	}
	return cpu, mem
}

func findOverride(overrides []autoscalingv1alpha1.ContainerOverride, name string) *autoscalingv1alpha1.ContainerOverride {
	for i := range overrides {
		if overrides[i].ContainerName == name {
			return &overrides[i]
		}
	}
	return nil
}

func boundsFor(policy *autoscalingv1alpha1.ResourcePolicy, override *autoscalingv1alpha1.ContainerOverride) (cpu, mem *autoscalingv1alpha1.ResourceBounds) {
	cpu, mem = &policy.CPU, &policy.Memory
	if override != nil {
		if override.CPU != nil {
			cpu = override.CPU
		}
		if override.Memory != nil {
			mem = override.Memory
		}
	}
	return cpu, mem
}

func roundUpMilli(milli float64, step int64) resource.Quantity {
	rounded := int64(math.Ceil(milli/float64(step))) * step
	return *resource.NewMilliQuantity(rounded, resource.DecimalSI)
}

func roundUpBytes(bytes float64, step int64) resource.Quantity {
	rounded := int64(math.Ceil(bytes/float64(step))) * step
	return *resource.NewQuantity(rounded, resource.BinarySI)
}

func clampQuantity(q resource.Quantity, bounds *autoscalingv1alpha1.ResourceBounds) resource.Quantity {
	if q.Cmp(bounds.Min) < 0 {
		return bounds.Min.DeepCopy()
	}
	if q.Cmp(bounds.Max) > 0 {
		return bounds.Max.DeepCopy()
	}
	return q
}

func sortedKeys(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
