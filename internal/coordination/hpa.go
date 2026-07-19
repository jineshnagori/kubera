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

// Package coordination models the coupled control loops between KubeRA and
// the HorizontalPodAutoscaler. HPA's resource metric is usage/requests: every
// resize KubeRA makes changes what HPA sees. Raising requests suppresses HPA
// (intended: vertical-first). Lowering requests naively would inflate the
// utilization HPA observes and trigger spurious scale-out — the reason
// VPA+HPA on the same metric is forbidden. The floor math here prevents that.
package coordination

import (
	"context"
	"math"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// FindHPA returns the first HPA in the namespace whose scaleTargetRef points
// at the named Deployment, or nil.
func FindHPA(ctx context.Context, c client.Client, namespace, deployment string) (*autoscalingv2.HorizontalPodAutoscaler, error) {
	var list autoscalingv2.HorizontalPodAutoscalerList
	if err := c.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range list.Items {
		ref := list.Items[i].Spec.ScaleTargetRef
		if ref.Kind == "Deployment" && ref.Name == deployment {
			return &list.Items[i], nil
		}
	}
	return nil, nil
}

// IsScaling reports whether the HPA is actively scaling: replica counts
// disagree, or the last scale event is within the settle window.
func IsScaling(hpa *autoscalingv2.HorizontalPodAutoscaler, settle time.Duration, now time.Time) bool {
	if hpa == nil {
		return false
	}
	if hpa.Status.DesiredReplicas != hpa.Status.CurrentReplicas {
		return true
	}
	if t := hpa.Status.LastScaleTime; t != nil && now.Sub(t.Time) < settle {
		return true
	}
	return false
}

// TargetUtilization extracts the average-utilization targets for CPU and
// memory Resource (or ContainerResource) metrics; 0 means the HPA does not
// scale on that resource.
func TargetUtilization(hpa *autoscalingv2.HorizontalPodAutoscaler) (cpu, mem int32) {
	if hpa == nil {
		return 0, 0
	}
	for _, m := range hpa.Spec.Metrics {
		var name corev1.ResourceName
		var target *autoscalingv2.MetricTarget
		switch {
		case m.Type == autoscalingv2.ResourceMetricSourceType && m.Resource != nil:
			name, target = m.Resource.Name, &m.Resource.Target
		case m.Type == autoscalingv2.ContainerResourceMetricSourceType && m.ContainerResource != nil:
			name, target = m.ContainerResource.Name, &m.ContainerResource.Target
		default:
			continue
		}
		if target.Type != autoscalingv2.UtilizationMetricType || target.AverageUtilization == nil {
			continue
		}
		switch name {
		case corev1.ResourceCPU:
			cpu = *target.AverageUtilization
		case corev1.ResourceMemory:
			mem = *target.AverageUtilization
		}
	}
	return cpu, mem
}

// RequestFloor returns the smallest request (same unit as usage) that keeps
// the utilization HPA observes at or below targetUtilization−headroom after a
// downward resize. Returns 0 when the HPA does not scale on this resource.
//
//	usage / floor * 100 == target − headroom
func RequestFloor(usage float64, targetUtilization, headroomPercent int32) int64 {
	effective := targetUtilization - headroomPercent
	if targetUtilization <= 0 || effective <= 0 {
		return 0
	}
	return int64(math.Ceil(usage * 100.0 / float64(effective)))
}
