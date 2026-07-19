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

package coordination

import (
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func hpaWithCPUTarget(target int32) *autoscalingv2.HorizontalPodAutoscaler {
	return &autoscalingv2.HorizontalPodAutoscaler{
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &target,
					},
				},
			}},
		},
	}
}

func TestIsScaling(t *testing.T) {
	now := time.Now()

	if IsScaling(nil, time.Minute, now) {
		t.Error("nil HPA is never scaling")
	}

	hpa := hpaWithCPUTarget(75)
	hpa.Status.CurrentReplicas = 3
	hpa.Status.DesiredReplicas = 3
	if IsScaling(hpa, time.Minute, now) {
		t.Error("settled HPA must not report scaling")
	}

	hpa.Status.DesiredReplicas = 5
	if !IsScaling(hpa, time.Minute, now) {
		t.Error("desired != current means actively scaling")
	}

	hpa.Status.DesiredReplicas = 3
	hpa.Status.LastScaleTime = &metav1.Time{Time: now.Add(-30 * time.Second)}
	if !IsScaling(hpa, time.Minute, now) {
		t.Error("recent lastScaleTime within settle window means scaling")
	}
	if IsScaling(hpa, time.Minute, now.Add(2*time.Minute)) {
		t.Error("old lastScaleTime outside settle window is settled")
	}
}

func TestTargetUtilization(t *testing.T) {
	cpu, mem := TargetUtilization(hpaWithCPUTarget(75))
	if cpu != 75 || mem != 0 {
		t.Errorf("got cpu=%d mem=%d, want cpu=75 mem=0", cpu, mem)
	}
	if cpu, mem := TargetUtilization(nil); cpu != 0 || mem != 0 {
		t.Errorf("nil HPA must yield zero targets, got %d/%d", cpu, mem)
	}
}

func TestRequestFloor(t *testing.T) {
	// usage 130m, HPA target 75%, headroom 10% -> effective 65%.
	// floor = 130 * 100 / 65 = 200m: at a 200m request HPA sees 65%.
	if got := RequestFloor(130, 75, 10); got != 200 {
		t.Errorf("floor = %d, want 200", got)
	}
	// No HPA metric on this resource: no floor.
	if got := RequestFloor(130, 0, 10); got != 0 {
		t.Errorf("floor without target = %d, want 0", got)
	}
	// Degenerate config (headroom >= target): no floor rather than divide by zero.
	if got := RequestFloor(130, 10, 10); got != 0 {
		t.Errorf("floor with headroom==target = %d, want 0", got)
	}
}
