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

// Package actuator turns recommendations into safe in-place Pod resizes:
// planning applies QoS, tolerance, step and memory-decrease guards; the
// Resizer patches the Pod resize subresource.
package actuator

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
)

// Direction of a resize, for per-direction cooldowns and step caps.
type Direction string

const (
	DirectionUp   Direction = "Up"
	DirectionDown Direction = "Down"
)

// Policy carries the guard parameters derived from a DynamicResource spec.
type Policy struct {
	ControlledValues autoscalingv1alpha1.ControlledValues
	// Tolerance is the relative band around the current value inside which no
	// resize happens (anti-flap), e.g. 0.10 for ±10%.
	Tolerance float64
	// MaxStepUpPercent / MaxStepDownPercent cap how far one resize may move a
	// value from its current one, per direction.
	MaxStepUpPercent   int32
	MaxStepDownPercent int32
}

// DefaultPolicy returns guard parameters from spec.behavior with README
// defaults filled in: up fast (50%/step), down slow (20%/step), ±10% band.
func DefaultPolicy(spec *autoscalingv1alpha1.DynamicResourceSpec) Policy {
	p := Policy{
		ControlledValues:   autoscalingv1alpha1.ControlledValuesRequestsAndLimits,
		Tolerance:          0.10,
		MaxStepUpPercent:   50,
		MaxStepDownPercent: 20,
	}
	if spec.Resources.ControlledValues != "" {
		p.ControlledValues = spec.Resources.ControlledValues
	}
	if b := spec.Behavior; b != nil {
		if b.ScaleUp != nil && b.ScaleUp.MaxStepPercent > 0 {
			p.MaxStepUpPercent = b.ScaleUp.MaxStepPercent
		}
		if b.ScaleDown != nil && b.ScaleDown.MaxStepPercent > 0 {
			p.MaxStepDownPercent = b.ScaleDown.MaxStepPercent
		}
	}
	return p
}

// ContainerChange is the planned new resources for one container.
type ContainerChange struct {
	Name     string
	Requests corev1.ResourceList
	// Limits is nil when limits are left untouched (RequestsOnly, or no
	// limits set on the container).
	Limits corev1.ResourceList
}

// PodPlan is the set of container changes to apply to one Pod in a single
// resize patch.
type PodPlan struct {
	Pod       string
	Changes   []ContainerChange
	Direction Direction
}

// PlanPod computes the resize for one Pod given per-container recommendation
// targets (requests). Returns nil when nothing should change.
//
// Guards, in order:
//   - BestEffort Pods are skipped entirely: adding requests would change the
//     QoS class, which in-place resize forbids.
//   - Tolerance: a container inside the ±band on both resources is skipped.
//   - Step caps: the new value moves at most maxStep% from the current one.
//   - Memory decrease: memory LIMITS never decrease in-place (requires
//     resizePolicy RestartContainer); on Guaranteed Pods (limits==requests)
//     that means memory cannot shrink at all.
//   - QoS preservation: Guaranteed keeps requests==limits; Burstable keeps
//     the original limit/request ratio (RequestsAndLimits) or caps requests
//     at the existing limit (RequestsOnly).
func PlanPod(pod *corev1.Pod, targets map[string]corev1.ResourceList, policy Policy) *PodPlan {
	if qosClass(pod) == corev1.PodQOSBestEffort {
		return nil
	}
	guaranteed := qosClass(pod) == corev1.PodQOSGuaranteed

	plan := &PodPlan{Pod: pod.Name, Direction: DirectionDown}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		target, ok := targets[c.Name]
		if !ok {
			continue
		}
		change, up := planContainer(c, target, policy, guaranteed)
		if change == nil {
			continue
		}
		plan.Changes = append(plan.Changes, *change)
		if up {
			plan.Direction = DirectionUp
		}
	}
	if len(plan.Changes) == 0 {
		return nil
	}
	return plan
}

func planContainer(c *corev1.Container, target corev1.ResourceList, policy Policy, guaranteed bool) (*ContainerChange, bool) {
	curCPU := c.Resources.Requests[corev1.ResourceCPU]
	curMem := c.Resources.Requests[corev1.ResourceMemory]
	if curCPU.IsZero() || curMem.IsZero() {
		// No requests on this container: setting them could change QoS.
		return nil, false
	}

	wantCPU := target[corev1.ResourceCPU]
	wantMem := target[corev1.ResourceMemory]
	if wantCPU.IsZero() || wantMem.IsZero() {
		return nil, false
	}

	if withinTolerance(curCPU.MilliValue(), wantCPU.MilliValue(), policy.Tolerance) &&
		withinTolerance(curMem.Value(), wantMem.Value(), policy.Tolerance) {
		return nil, false
	}

	newCPUMilli := stepClamp(curCPU.MilliValue(), wantCPU.MilliValue(), policy)
	newMemBytes := stepClamp(curMem.Value(), wantMem.Value(), policy)

	// Memory limits never decrease in-place. Guaranteed pods have
	// limit==request, so their memory request cannot shrink either.
	if guaranteed && newMemBytes < curMem.Value() {
		newMemBytes = curMem.Value()
	}

	limits := planLimits(c, &newCPUMilli, &newMemBytes, policy, guaranteed)

	if newCPUMilli == curCPU.MilliValue() && newMemBytes == curMem.Value() {
		return nil, false
	}

	change := &ContainerChange{
		Name: c.Name,
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(newCPUMilli, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(newMemBytes, resource.BinarySI),
		},
		Limits: limits,
	}
	up := newCPUMilli > curCPU.MilliValue() || newMemBytes > curMem.Value()
	return change, up
}

// planLimits computes the new limits (nil = leave untouched); it may cap the
// new requests in place so they stay under untouched limits (RequestsOnly)
// and to honour the no-memory-limit-decrease rule.
func planLimits(c *corev1.Container, newCPUMilli, newMemBytes *int64, policy Policy, guaranteed bool) corev1.ResourceList {
	curCPULim, hasCPULim := c.Resources.Limits[corev1.ResourceCPU]
	curMemLim, hasMemLim := c.Resources.Limits[corev1.ResourceMemory]

	if policy.ControlledValues == autoscalingv1alpha1.ControlledValuesRequestsOnly && !guaranteed {
		// Requests must stay under existing limits.
		if hasCPULim && *newCPUMilli > curCPULim.MilliValue() {
			*newCPUMilli = curCPULim.MilliValue()
		}
		if hasMemLim && *newMemBytes > curMemLim.Value() {
			*newMemBytes = curMemLim.Value()
		}
		return nil
	}

	if guaranteed {
		return corev1.ResourceList{
			corev1.ResourceCPU:    *resource.NewMilliQuantity(*newCPUMilli, resource.DecimalSI),
			corev1.ResourceMemory: *resource.NewQuantity(*newMemBytes, resource.BinarySI),
		}
	}

	if !hasCPULim && !hasMemLim {
		return nil
	}

	limits := corev1.ResourceList{}
	if hasCPULim {
		curCPUReq := c.Resources.Requests[corev1.ResourceCPU]
		ratio := float64(curCPULim.MilliValue()) / float64(curCPUReq.MilliValue())
		limits[corev1.ResourceCPU] = *resource.NewMilliQuantity(int64(float64(*newCPUMilli)*ratio), resource.DecimalSI)
	}
	if hasMemLim {
		curMemReq := c.Resources.Requests[corev1.ResourceMemory]
		ratio := float64(curMemLim.Value()) / float64(curMemReq.Value())
		// Never decrease a memory limit in-place.
		newLim := max(int64(float64(*newMemBytes)*ratio), curMemLim.Value())
		limits[corev1.ResourceMemory] = *resource.NewQuantity(newLim, resource.BinarySI)
	}
	return limits
}

func withinTolerance(current, desired int64, tolerance float64) bool {
	if current == 0 {
		return desired == 0
	}
	diff := float64(desired-current) / float64(current)
	if diff < 0 {
		diff = -diff
	}
	return diff <= tolerance
}

// stepClamp moves current toward desired, at most maxStep% away per resize.
func stepClamp(current, desired int64, policy Policy) int64 {
	if desired > current {
		maxUp := current + current*int64(policy.MaxStepUpPercent)/100
		if desired > maxUp {
			return maxUp
		}
		return desired
	}
	minDown := current - current*int64(policy.MaxStepDownPercent)/100
	if desired < minDown {
		return minDown
	}
	return desired
}

// qosClass mirrors the kubelet's QoS derivation; pod.Status.QOSClass is
// authoritative when set.
func qosClass(pod *corev1.Pod) corev1.PodQOSClass {
	if pod.Status.QOSClass != "" {
		return pod.Status.QOSClass
	}
	requestsSet, allGuaranteed := false, true
	for i := range pod.Spec.Containers {
		res := &pod.Spec.Containers[i].Resources
		if len(res.Requests) > 0 {
			requestsSet = true
		}
		for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
			req, hasReq := res.Requests[name]
			lim, hasLim := res.Limits[name]
			if !hasReq || !hasLim || req.Cmp(lim) != 0 {
				allGuaranteed = false
			}
		}
	}
	switch {
	case !requestsSet:
		return corev1.PodQOSBestEffort
	case allGuaranteed:
		return corev1.PodQOSGuaranteed
	default:
		return corev1.PodQOSBurstable
	}
}
