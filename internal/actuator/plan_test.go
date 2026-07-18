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

package actuator

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
)

const appName = "app"

func rl(cpu, mem string) corev1.ResourceList {
	return corev1.ResourceList{
		corev1.ResourceCPU:    resource.MustParse(cpu),
		corev1.ResourceMemory: resource.MustParse(mem),
	}
}

func pod(requests, limits corev1.ResourceList) *corev1.Pod {
	c := corev1.Container{Name: appName}
	c.Resources.Requests = requests
	c.Resources.Limits = limits
	return &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{c}}}
}

func defaultTestPolicy() Policy {
	return Policy{
		ControlledValues:   autoscalingv1alpha1.ControlledValuesRequestsAndLimits,
		Tolerance:          0.10,
		MaxStepUpPercent:   50,
		MaxStepDownPercent: 20,
	}
}

func TestPlanSkipsBestEffort(t *testing.T) {
	p := pod(nil, nil)
	if plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("200m", "256Mi")}, defaultTestPolicy()); plan != nil {
		t.Error("BestEffort pod must not be resized: adding requests changes QoS class")
	}
}

func TestPlanSkipsWithinTolerance(t *testing.T) {
	p := pod(rl("100m", "128Mi"), nil)
	// 105m / 130Mi: both within ±10%.
	if plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("105m", "130Mi")}, defaultTestPolicy()); plan != nil {
		t.Error("change within tolerance band must be skipped")
	}
}

func TestPlanStepCapUp(t *testing.T) {
	p := pod(rl("100m", "128Mi"), nil)
	plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("1000m", "128Mi")}, defaultTestPolicy())
	if plan == nil {
		t.Fatal("expected a plan")
	}
	cpu := plan.Changes[0].Requests[corev1.ResourceCPU]
	if cpu.MilliValue() != 150 {
		t.Errorf("cpu = %vm, want 150m (50%% step cap from 100m)", cpu.MilliValue())
	}
	if plan.Direction != DirectionUp {
		t.Errorf("direction = %v, want Up", plan.Direction)
	}
}

func TestPlanStepCapDown(t *testing.T) {
	p := pod(rl("1000m", "128Mi"), nil)
	plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("100m", "128Mi")}, defaultTestPolicy())
	if plan == nil {
		t.Fatal("expected a plan")
	}
	cpu := plan.Changes[0].Requests[corev1.ResourceCPU]
	if cpu.MilliValue() != 800 {
		t.Errorf("cpu = %vm, want 800m (20%% step cap from 1000m)", cpu.MilliValue())
	}
	if plan.Direction != DirectionDown {
		t.Errorf("direction = %v, want Down", plan.Direction)
	}
}

func TestPlanGuaranteedKeepsRequestsEqualLimits(t *testing.T) {
	p := pod(rl("100m", "128Mi"), rl("100m", "128Mi"))
	p.Status.QOSClass = corev1.PodQOSGuaranteed
	plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("140m", "160Mi")}, defaultTestPolicy())
	if plan == nil {
		t.Fatal("expected a plan")
	}
	ch := plan.Changes[0]
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		req, lim := ch.Requests[name], ch.Limits[name]
		if req.Cmp(lim) != 0 {
			t.Errorf("%s: request %v != limit %v; QoS class would change", name, req.String(), lim.String())
		}
	}
}

func TestPlanGuaranteedMemoryNeverShrinks(t *testing.T) {
	p := pod(rl("500m", "512Mi"), rl("500m", "512Mi"))
	p.Status.QOSClass = corev1.PodQOSGuaranteed
	plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("300m", "128Mi")}, defaultTestPolicy())
	if plan == nil {
		t.Fatal("expected a plan (cpu shrinks)")
	}
	mem := plan.Changes[0].Requests[corev1.ResourceMemory]
	if mem.String() != "512Mi" {
		t.Errorf("memory = %v, want unchanged 512Mi: guaranteed memory limit cannot decrease in-place", mem.String())
	}
}

func TestPlanBurstableScalesLimitsByRatio(t *testing.T) {
	// limits = 2x requests.
	p := pod(rl("100m", "128Mi"), rl("200m", "256Mi"))
	plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("140m", "128Mi")}, defaultTestPolicy())
	if plan == nil {
		t.Fatal("expected a plan")
	}
	cpuLim := plan.Changes[0].Limits[corev1.ResourceCPU]
	if cpuLim.MilliValue() != 280 {
		t.Errorf("cpu limit = %vm, want 280m (2x ratio preserved)", cpuLim.MilliValue())
	}
}

func TestPlanBurstableMemoryLimitNeverDecreases(t *testing.T) {
	p := pod(rl("100m", "512Mi"), rl("200m", "1Gi"))
	plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("100m", "256Mi")}, defaultTestPolicy())
	if plan == nil {
		t.Fatal("expected a plan (memory request shrinks)")
	}
	memLim := plan.Changes[0].Limits[corev1.ResourceMemory]
	if memLim.String() != "1Gi" {
		t.Errorf("memory limit = %v, want unchanged 1Gi", memLim.String())
	}
	memReq := plan.Changes[0].Requests[corev1.ResourceMemory]
	if memReq.Value() >= 512*1024*1024 {
		t.Errorf("memory request = %v, want decreased below 512Mi", memReq.String())
	}
}

func TestPlanRequestsOnlyCapsAtLimits(t *testing.T) {
	policy := defaultTestPolicy()
	policy.ControlledValues = autoscalingv1alpha1.ControlledValuesRequestsOnly
	p := pod(rl("100m", "128Mi"), rl("120m", "150Mi"))
	plan := PlanPod(p, map[string]corev1.ResourceList{appName: rl("150m", "200Mi")}, policy)
	if plan == nil {
		t.Fatal("expected a plan")
	}
	ch := plan.Changes[0]
	if ch.Limits != nil {
		t.Error("RequestsOnly must not touch limits")
	}
	cpu := ch.Requests[corev1.ResourceCPU]
	if cpu.MilliValue() != 120 {
		t.Errorf("cpu = %vm, want capped at limit 120m", cpu.MilliValue())
	}
	mem := ch.Requests[corev1.ResourceMemory]
	if mem.String() != "150Mi" {
		t.Errorf("memory = %v, want capped at limit 150Mi", mem.String())
	}
}

func TestCooldownTracker(t *testing.T) {
	tr := NewCooldownTracker()
	now := time.Now()
	if !tr.Allow("ns/api", DirectionUp, time.Minute, now) {
		t.Error("first resize must be allowed")
	}
	tr.Record("ns/api", DirectionUp, now)
	if tr.Allow("ns/api", DirectionUp, time.Minute, now.Add(30*time.Second)) {
		t.Error("resize within cooldown must be blocked")
	}
	if !tr.Allow("ns/api", DirectionDown, time.Minute, now.Add(30*time.Second)) {
		t.Error("other direction has its own cooldown")
	}
	if !tr.Allow("ns/api", DirectionUp, time.Minute, now.Add(2*time.Minute)) {
		t.Error("resize after cooldown must be allowed")
	}
}
