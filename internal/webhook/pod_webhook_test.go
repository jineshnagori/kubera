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

package webhook

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
)

const (
	ns            = "production"
	deployName    = "api"
	containerName = "app"
)

func scheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := autoscalingv1alpha1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

func rsOwnedPod() *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			GenerateName: deployName + "-abc123-",
			Namespace:    ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "ReplicaSet", Name: deployName + "-abc123",
			}},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: containerName,
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("100m"),
						corev1.ResourceMemory: resource.MustParse("128Mi"),
					},
					Limits: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("200m"), // 2x ratio
						corev1.ResourceMemory: resource.MustParse("256Mi"),
					},
				},
			}},
		},
	}
}

func replicaSet() *appsv1.ReplicaSet {
	return &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name: deployName + "-abc123", Namespace: ns,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "apps/v1", Kind: "Deployment", Name: deployName,
			}},
		},
	}
}

func dynamicResource(mode autoscalingv1alpha1.UpdateMode) *autoscalingv1alpha1.DynamicResource {
	return &autoscalingv1alpha1.DynamicResource{
		ObjectMeta: metav1.ObjectMeta{Name: "api-policy", Namespace: ns},
		Spec: autoscalingv1alpha1.DynamicResourceSpec{
			Selector:   &metav1.LabelSelector{MatchLabels: map[string]string{containerName: deployName}},
			UpdateMode: mode,
			Resources: autoscalingv1alpha1.ResourcePolicy{
				CPU:    autoscalingv1alpha1.ResourceBounds{Min: resource.MustParse("50m"), Max: resource.MustParse("1")},
				Memory: autoscalingv1alpha1.ResourceBounds{Min: resource.MustParse("64Mi"), Max: resource.MustParse("1Gi")},
			},
		},
		Status: autoscalingv1alpha1.DynamicResourceStatus{
			Recommendations: []autoscalingv1alpha1.WorkloadRecommendation{{
				Workload: deployName,
				Containers: []autoscalingv1alpha1.ContainerRecommendation{{
					ContainerName: containerName,
					Target: corev1.ResourceList{
						corev1.ResourceCPU:    resource.MustParse("300m"),
						corev1.ResourceMemory: resource.MustParse("400Mi"),
					},
				}},
			}},
		},
	}
}

func TestWebhookInjectsRecommendation(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).
		WithObjects(replicaSet(), dynamicResource(autoscalingv1alpha1.UpdateModeInPlaceOnly)).
		Build()
	d := &PodResourceDefaulter{Client: c}

	pod := rsOwnedPod()
	if err := d.Default(context.Background(), pod); err != nil {
		t.Fatal(err)
	}

	cpu := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpu.MilliValue() != 300 {
		t.Errorf("cpu request = %dm, want 300m", cpu.MilliValue())
	}
	cpuLim := pod.Spec.Containers[0].Resources.Limits[corev1.ResourceCPU]
	if cpuLim.MilliValue() != 600 {
		t.Errorf("cpu limit = %dm, want 600m (2x ratio preserved)", cpuLim.MilliValue())
	}
	mem := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceMemory]
	if mem.String() != "400Mi" {
		t.Errorf("memory request = %v, want 400Mi", mem.String())
	}
}

func TestWebhookIgnoresOffMode(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).
		WithObjects(replicaSet(), dynamicResource(autoscalingv1alpha1.UpdateModeOff)).
		Build()
	d := &PodResourceDefaulter{Client: c}

	pod := rsOwnedPod()
	if err := d.Default(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	cpu := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpu.MilliValue() != 100 {
		t.Errorf("cpu = %dm, want untouched 100m in Off mode", cpu.MilliValue())
	}
}

func TestWebhookIgnoresUnownedPod(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).
		WithObjects(dynamicResource(autoscalingv1alpha1.UpdateModeInPlaceOnly)).
		Build()
	d := &PodResourceDefaulter{Client: c}

	pod := rsOwnedPod()
	pod.OwnerReferences = nil
	if err := d.Default(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	cpu := pod.Spec.Containers[0].Resources.Requests[corev1.ResourceCPU]
	if cpu.MilliValue() != 100 {
		t.Errorf("cpu = %dm, want untouched for pod without deployment owner", cpu.MilliValue())
	}
}

func TestWebhookSkipsBestEffortContainer(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(scheme(t)).
		WithObjects(replicaSet(), dynamicResource(autoscalingv1alpha1.UpdateModeInPlaceOnly)).
		Build()
	d := &PodResourceDefaulter{Client: c}

	pod := rsOwnedPod()
	pod.Spec.Containers[0].Resources = corev1.ResourceRequirements{}
	if err := d.Default(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
	if len(pod.Spec.Containers[0].Resources.Requests) != 0 {
		t.Error("BestEffort container must stay BestEffort (QoS immutability)")
	}
}
