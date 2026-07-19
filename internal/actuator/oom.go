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
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

const (
	// oomLookback is how recent an OOMKill must be to trigger the fast path.
	oomLookback = 15 * time.Minute
	// oomBumpFactor multiplies the current memory request after an OOMKill.
	oomBumpFactor = 1.5
)

// OOMKilledContainers returns the names of containers whose last termination
// was an OOMKill within the lookback window. These get an immediate memory
// bump that bypasses tolerance, step caps and cooldowns — waiting for the
// histogram to catch up would mean repeated OOMKills.
func OOMKilledContainers(pod *corev1.Pod, now time.Time) []string {
	var names []string
	for _, cs := range pod.Status.ContainerStatuses {
		t := cs.LastTerminationState.Terminated
		if t == nil || t.Reason != "OOMKilled" {
			continue
		}
		if now.Sub(t.FinishedAt.Time) <= oomLookback {
			names = append(names, cs.Name)
		}
	}
	return names
}

// PlanOOMBump builds a plan bumping memory for the OOMKilled containers to
// current*oomBumpFactor, capped at maxMemory. CPU is left unchanged. Returns
// nil when every bump is already at the cap.
func PlanOOMBump(pod *corev1.Pod, oomContainers []string, maxMemory map[string]resource.Quantity, policy Policy) *PodPlan {
	oom := map[string]bool{}
	for _, n := range oomContainers {
		oom[n] = true
	}
	guaranteed := qosClass(pod) == corev1.PodQOSGuaranteed

	plan := &PodPlan{Pod: pod.Name, Direction: DirectionUp}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		if !oom[c.Name] {
			continue
		}
		curMem := c.Resources.Requests[corev1.ResourceMemory]
		curCPU := c.Resources.Requests[corev1.ResourceCPU]
		if curMem.IsZero() || curCPU.IsZero() {
			continue
		}

		newMem := int64(float64(curMem.Value()) * oomBumpFactor)
		if maxQ, ok := maxMemory[c.Name]; ok && newMem > maxQ.Value() {
			newMem = maxQ.Value()
		}
		if newMem <= curMem.Value() {
			continue // already at cap
		}

		newCPUMilli := curCPU.MilliValue()
		limits := planLimits(c, &newCPUMilli, &newMem, policy, guaranteed)
		plan.Changes = append(plan.Changes, ContainerChange{
			Name: c.Name,
			Requests: corev1.ResourceList{
				corev1.ResourceCPU:    *resource.NewMilliQuantity(newCPUMilli, resource.DecimalSI),
				corev1.ResourceMemory: *resource.NewQuantity(newMem, resource.BinarySI),
			},
			Limits: limits,
		})
	}
	if len(plan.Changes) == 0 {
		return nil
	}
	return plan
}
