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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func oomPod(finishedAgo time.Duration, now time.Time) *corev1.Pod {
	p := pod(rl("100m", "256Mi"), nil)
	p.Status.ContainerStatuses = []corev1.ContainerStatus{{
		Name: appName,
		LastTerminationState: corev1.ContainerState{
			Terminated: &corev1.ContainerStateTerminated{
				Reason:     "OOMKilled",
				FinishedAt: metav1.Time{Time: now.Add(-finishedAgo)},
			},
		},
	}}
	return p
}

func TestOOMKilledContainers(t *testing.T) {
	now := time.Now()
	if got := OOMKilledContainers(oomPod(5*time.Minute, now), now); len(got) != 1 || got[0] != appName {
		t.Errorf("recent OOMKill must be detected, got %v", got)
	}
	if got := OOMKilledContainers(oomPod(time.Hour, now), now); len(got) != 0 {
		t.Errorf("stale OOMKill outside lookback must be ignored, got %v", got)
	}
	if got := OOMKilledContainers(pod(rl("100m", "256Mi"), nil), now); len(got) != 0 {
		t.Errorf("pod without terminations must yield none, got %v", got)
	}
}

func TestPlanOOMBump(t *testing.T) {
	now := time.Now()
	p := oomPod(time.Minute, now)
	maxMem := map[string]resource.Quantity{appName: resource.MustParse("1Gi")}

	plan := PlanOOMBump(p, []string{appName}, maxMem, defaultTestPolicy())
	if plan == nil {
		t.Fatal("expected a bump plan")
	}
	mem := plan.Changes[0].Requests[corev1.ResourceMemory]
	if got, want := mem.Value(), int64(float64(256*1024*1024)*1.5); got != want {
		t.Errorf("memory = %d, want %d (1.5x bump)", got, want)
	}
	cpu := plan.Changes[0].Requests[corev1.ResourceCPU]
	if cpu.MilliValue() != 100 {
		t.Errorf("cpu = %dm, want unchanged 100m", cpu.MilliValue())
	}
	if plan.Direction != DirectionUp {
		t.Errorf("direction = %v, want Up", plan.Direction)
	}
}

func TestPlanOOMBumpCappedAtMax(t *testing.T) {
	now := time.Now()
	p := oomPod(time.Minute, now)
	// Max below current: nothing to bump.
	maxMem := map[string]resource.Quantity{appName: resource.MustParse("256Mi")}
	if plan := PlanOOMBump(p, []string{appName}, maxMem, defaultTestPolicy()); plan != nil {
		t.Error("bump already at cap must return nil")
	}
}
