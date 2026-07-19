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
	"context"

	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BlockedMemoryShrink reports whether a memory decrease beyond the tolerance
// band is wanted but cannot be done in-place: Guaranteed pods pin
// requests==limits and the kubelet forbids in-place memory limit decrease.
// Such pods only converge through evict-and-recreate (updateMode
// InPlaceOrRecreate + the pod webhook injecting the recommendation at
// admission).
func BlockedMemoryShrink(pod *corev1.Pod, targets map[string]corev1.ResourceList, policy Policy) bool {
	if qosClass(pod) != corev1.PodQOSGuaranteed {
		return false
	}
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		target, ok := targets[c.Name]
		if !ok {
			continue
		}
		curMem := c.Resources.Requests[corev1.ResourceMemory]
		wantMem := target[corev1.ResourceMemory]
		if curMem.IsZero() || wantMem.IsZero() {
			continue
		}
		if !withinTolerance(curMem.Value(), wantMem.Value(), policy.Tolerance) && wantMem.Value() < curMem.Value() {
			return true
		}
	}
	return false
}

// Evict evicts one pod through the Eviction API, which respects
// PodDisruptionBudgets — the pod's controller recreates it, and the pod
// webhook injects the current recommendation at admission.
func (r *Resizer) Evict(ctx context.Context, namespace, pod string) error {
	if r.Limiter != nil && !r.Limiter.Allow() {
		return ErrRateLimited
	}
	p := &corev1.Pod{}
	p.Name = pod
	p.Namespace = namespace
	eviction := &policyv1.Eviction{
		ObjectMeta: metav1.ObjectMeta{Name: pod, Namespace: namespace},
	}
	return r.Client.SubResource("eviction").Create(ctx, p, eviction)
}
