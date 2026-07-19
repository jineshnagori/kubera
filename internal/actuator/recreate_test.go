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

	corev1 "k8s.io/api/core/v1"
)

func TestBlockedMemoryShrink(t *testing.T) {
	policy := defaultTestPolicy()

	guaranteed := pod(rl("500m", "512Mi"), rl("500m", "512Mi"))
	guaranteed.Status.QOSClass = corev1.PodQOSGuaranteed
	if !BlockedMemoryShrink(guaranteed, map[string]corev1.ResourceList{appName: rl("500m", "128Mi")}, policy) {
		t.Error("Guaranteed pod with memory shrink wanted must report blocked")
	}
	if BlockedMemoryShrink(guaranteed, map[string]corev1.ResourceList{appName: rl("500m", "500Mi")}, policy) {
		t.Error("shrink within tolerance must not report blocked")
	}
	if BlockedMemoryShrink(guaranteed, map[string]corev1.ResourceList{appName: rl("500m", "1Gi")}, policy) {
		t.Error("memory increase must not report blocked")
	}

	burstable := pod(rl("500m", "512Mi"), rl("1", "1Gi"))
	if BlockedMemoryShrink(burstable, map[string]corev1.ResourceList{appName: rl("500m", "128Mi")}, policy) {
		t.Error("Burstable pod shrinks requests in-place; never blocked")
	}
}
