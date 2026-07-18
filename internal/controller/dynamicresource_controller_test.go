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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
	"github.com/jineshnagori/kubera/internal/metrics"
	"github.com/jineshnagori/kubera/internal/recommender"
)

const (
	appLabelKey = "app"
	appAPI      = "api"
)

var fakeMetrics = metrics.NewFakeProvider()

func newReconciler() *DynamicResourceReconciler {
	return &DynamicResourceReconciler{
		Client:      k8sClient,
		Scheme:      k8sClient.Scheme(),
		Recorder:    events.NewFakeRecorder(16),
		Metrics:     fakeMetrics,
		Recommender: recommender.New(),
	}
}

func newDynamicResource(name, namespace string, matchLabels map[string]string) *autoscalingv1alpha1.DynamicResource {
	return &autoscalingv1alpha1.DynamicResource{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: autoscalingv1alpha1.DynamicResourceSpec{
			Selector: &metav1.LabelSelector{MatchLabels: matchLabels},
			Resources: autoscalingv1alpha1.ResourcePolicy{
				CPU: autoscalingv1alpha1.ResourceBounds{
					Min: resource.MustParse("100m"),
					Max: resource.MustParse("1000m"),
				},
				Memory: autoscalingv1alpha1.ResourceBounds{
					Min: resource.MustParse("128Mi"),
					Max: resource.MustParse("1Gi"),
				},
			},
		},
	}
}

func newDeployment(name, namespace string, podLabels map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Labels: podLabels},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: podLabels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: podLabels},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: appAPI, Image: "nginx"}},
				},
			},
		},
	}
}

func reconcileOnce(ctx context.Context, name, namespace string) *autoscalingv1alpha1.DynamicResource {
	GinkgoHelper()
	nn := types.NamespacedName{Name: name, Namespace: namespace}
	_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nn})
	Expect(err).NotTo(HaveOccurred())

	var dr autoscalingv1alpha1.DynamicResource
	Expect(k8sClient.Get(ctx, nn, &dr)).To(Succeed())
	return &dr
}

var _ = Describe("DynamicResource Controller", func() {
	ctx := context.Background()

	var namespace string
	var nsCounter int

	BeforeEach(func() {
		nsCounter++
		namespace = fmt.Sprintf("kubera-test-%d", nsCounter)
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: namespace},
		})).To(Succeed())
	})

	It("matches a Deployment by label and becomes Ready", func() {
		labels := map[string]string{appLabelKey: appAPI}
		Expect(k8sClient.Create(ctx, newDeployment(appAPI, namespace, labels))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDynamicResource("api-policy", namespace, labels))).To(Succeed())

		dr := reconcileOnce(ctx, "api-policy", namespace)

		Expect(dr.Status.MatchedWorkloads).To(ConsistOf(
			autoscalingv1alpha1.WorkloadReference{Kind: "Deployment", Name: appAPI},
		))
		ready := meta.FindStatusCondition(dr.Status.Conditions, autoscalingv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))
		Expect(dr.Status.HPAState).To(Equal(autoscalingv1alpha1.HPAStateIdle))
		Expect(dr.Status.ObservedGeneration).To(Equal(dr.Generation))
	})

	It("matches multiple Deployments sharing a label", func() {
		shared := map[string]string{"team": "payments"}
		Expect(k8sClient.Create(ctx, newDeployment("checkout", namespace, shared))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDeployment("billing", namespace, shared))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDynamicResource("team-policy", namespace, shared))).To(Succeed())

		dr := reconcileOnce(ctx, "team-policy", namespace)

		Expect(dr.Status.MatchedWorkloads).To(HaveLen(2))
		Expect(dr.Status.MatchedWorkloads[0].Name).To(Equal("billing"))
		Expect(dr.Status.MatchedWorkloads[1].Name).To(Equal("checkout"))
	})

	It("is not Ready when the selector matches nothing", func() {
		Expect(k8sClient.Create(ctx, newDynamicResource("orphan", namespace, map[string]string{appLabelKey: "ghost"}))).To(Succeed())

		dr := reconcileOnce(ctx, "orphan", namespace)

		Expect(dr.Status.MatchedWorkloads).To(BeEmpty())
		ready := meta.FindStatusCondition(dr.Status.Conditions, autoscalingv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("NoMatch"))
	})

	It("does not match a Deployment in another namespace", func() {
		otherNS := namespace + "-other"
		Expect(k8sClient.Create(ctx, &corev1.Namespace{
			ObjectMeta: metav1.ObjectMeta{Name: otherNS},
		})).To(Succeed())

		labels := map[string]string{appLabelKey: appAPI}
		Expect(k8sClient.Create(ctx, newDeployment(appAPI, otherNS, labels))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDynamicResource("api-policy", namespace, labels))).To(Succeed())

		dr := reconcileOnce(ctx, "api-policy", namespace)
		Expect(dr.Status.MatchedWorkloads).To(BeEmpty())
	})

	It("marks the newer DynamicResource Conflicted when selectors overlap", func() {
		labels := map[string]string{appLabelKey: appAPI}
		Expect(k8sClient.Create(ctx, newDeployment(appAPI, namespace, labels))).To(Succeed())

		older := newDynamicResource("older-policy", namespace, labels)
		Expect(k8sClient.Create(ctx, older)).To(Succeed())
		newer := newDynamicResource("z-newer-policy", namespace, labels)
		Expect(k8sClient.Create(ctx, newer)).To(Succeed())

		olderGot := reconcileOnce(ctx, "older-policy", namespace)
		ready := meta.FindStatusCondition(olderGot.Status.Conditions, autoscalingv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionTrue))

		newerGot := reconcileOnce(ctx, "z-newer-policy", namespace)
		conflicted := meta.FindStatusCondition(newerGot.Status.Conditions, autoscalingv1alpha1.ConditionConflicted)
		Expect(conflicted).NotTo(BeNil())
		Expect(conflicted.Status).To(Equal(metav1.ConditionTrue))
		newerReady := meta.FindStatusCondition(newerGot.Status.Conditions, autoscalingv1alpha1.ConditionReady)
		Expect(newerReady.Status).To(Equal(metav1.ConditionFalse))
	})

	It("rejects a spec whose min exceeds max", func() {
		dr := newDynamicResource("bad-bounds", namespace, map[string]string{appLabelKey: appAPI})
		dr.Spec.Resources.CPU.Min = resource.MustParse("2000m")
		dr.Spec.Resources.CPU.Max = resource.MustParse("500m")
		Expect(k8sClient.Create(ctx, dr)).To(Succeed())

		got := reconcileOnce(ctx, "bad-bounds", namespace)
		ready := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionReady)
		Expect(ready).NotTo(BeNil())
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("InvalidBounds"))
	})

	It("publishes clamped recommendations from usage samples", func() {
		labels := map[string]string{appLabelKey: appAPI}
		Expect(k8sClient.Create(ctx, newDeployment(appAPI, namespace, labels))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDynamicResource("api-policy", namespace, labels))).To(Succeed())

		now := metav1.Now().Time
		fakeMetrics.SetPodUsage(namespace, metrics.PodUsage{
			Pod: "api-abc", Timestamp: now,
			Containers: []metrics.ContainerUsage{{
				Container: appAPI,
				CPU:       resource.MustParse("120m"),
				Memory:    resource.MustParse("400Mi"),
			}},
		})
		DeferCleanup(func() { fakeMetrics.SetPodUsage(namespace) })

		dr := reconcileOnce(ctx, "api-policy", namespace)

		Expect(dr.Status.Recommendations).To(HaveLen(1))
		Expect(dr.Status.Recommendations[0].Workload).To(Equal(appAPI))
		Expect(dr.Status.Recommendations[0].Containers).To(HaveLen(1))
		target := dr.Status.Recommendations[0].Containers[0].Target

		// ~120m * 1.10 margin, one 5% bucket up, rounded to 10m: within [130m, 160m].
		cpu := target[corev1.ResourceCPU]
		Expect(cpu.MilliValue()).To(BeNumerically(">=", 130))
		Expect(cpu.MilliValue()).To(BeNumerically("<=", 160))
		// Memory clamped: bounds max is 1Gi, raw ~400Mi*1.10 stays below.
		mem := target[corev1.ResourceMemory]
		Expect(mem.Value()).To(BeNumerically(">=", 400*1024*1024))
		Expect(mem.Value()).To(BeNumerically("<=", 1024*1024*1024))
	})

	It("clamps recommendations to configured max", func() {
		labels := map[string]string{appLabelKey: "hungry"}
		Expect(k8sClient.Create(ctx, newDeployment("hungry", namespace, labels))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDynamicResource("hungry-policy", namespace, labels))).To(Succeed())

		fakeMetrics.SetPodUsage(namespace, metrics.PodUsage{
			Pod: "hungry-abc", Timestamp: metav1.Now().Time,
			Containers: []metrics.ContainerUsage{{
				Container: appAPI,
				CPU:       resource.MustParse("5000m"), // way above max 1000m
				Memory:    resource.MustParse("8Gi"),   // way above max 1Gi
			}},
		})
		DeferCleanup(func() { fakeMetrics.SetPodUsage(namespace) })

		dr := reconcileOnce(ctx, "hungry-policy", namespace)

		target := dr.Status.Recommendations[0].Containers[0].Target
		cpu := target[corev1.ResourceCPU]
		mem := target[corev1.ResourceMemory]
		Expect(cpu.MilliValue()).To(Equal(int64(1000)))
		Expect(mem.Value()).To(Equal(int64(1024 * 1024 * 1024)))
	})

	It("skips containers with an Off override", func() {
		labels := map[string]string{appLabelKey: appAPI}
		Expect(k8sClient.Create(ctx, newDeployment(appAPI, namespace, labels))).To(Succeed())
		dr := newDynamicResource("api-policy", namespace, labels)
		dr.Spec.ContainerOverrides = []autoscalingv1alpha1.ContainerOverride{{
			ContainerName: "istio-proxy",
			Mode:          autoscalingv1alpha1.ContainerModeOff,
		}}
		Expect(k8sClient.Create(ctx, dr)).To(Succeed())

		fakeMetrics.SetPodUsage(namespace, metrics.PodUsage{
			Pod: "api-abc", Timestamp: metav1.Now().Time,
			Containers: []metrics.ContainerUsage{
				{Container: appAPI, CPU: resource.MustParse("120m"), Memory: resource.MustParse("400Mi")},
				{Container: "istio-proxy", CPU: resource.MustParse("50m"), Memory: resource.MustParse("64Mi")},
			},
		})
		DeferCleanup(func() { fakeMetrics.SetPodUsage(namespace) })

		got := reconcileOnce(ctx, "api-policy", namespace)

		Expect(got.Status.Recommendations[0].Containers).To(HaveLen(1))
		Expect(got.Status.Recommendations[0].Containers[0].ContainerName).To(Equal(appAPI))
	})

	It("reports MetricsUnavailable when the provider fails", func() {
		labels := map[string]string{appLabelKey: appAPI}
		Expect(k8sClient.Create(ctx, newDeployment(appAPI, namespace, labels))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDynamicResource("api-policy", namespace, labels))).To(Succeed())

		fakeMetrics.SetError(fmt.Errorf("metrics-server down"))
		DeferCleanup(func() { fakeMetrics.SetError(nil) })

		nn := types.NamespacedName{Name: "api-policy", Namespace: namespace}
		_, err := newReconciler().Reconcile(ctx, reconcile.Request{NamespacedName: nn})
		Expect(err).To(HaveOccurred())

		var got autoscalingv1alpha1.DynamicResource
		Expect(k8sClient.Get(ctx, nn, &got)).To(Succeed())
		ready := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionReady)
		Expect(ready.Reason).To(Equal("MetricsUnavailable"))
	})

	It("rejects the Prometheus provider until implemented", func() {
		labels := map[string]string{appLabelKey: appAPI}
		Expect(k8sClient.Create(ctx, newDeployment(appAPI, namespace, labels))).To(Succeed())
		dr := newDynamicResource("api-policy", namespace, labels)
		dr.Spec.Metrics = &autoscalingv1alpha1.MetricsConfig{
			Provider: autoscalingv1alpha1.MetricsProviderPrometheus,
		}
		Expect(k8sClient.Create(ctx, dr)).To(Succeed())

		got := reconcileOnce(ctx, "api-policy", namespace)
		ready := meta.FindStatusCondition(got.Status.Conditions, autoscalingv1alpha1.ConditionReady)
		Expect(ready.Status).To(Equal(metav1.ConditionFalse))
		Expect(ready.Reason).To(Equal("UnsupportedProvider"))
	})

	It("maps a Deployment event to the DynamicResources selecting it", func() {
		labels := map[string]string{appLabelKey: appAPI}
		Expect(k8sClient.Create(ctx, newDynamicResource("api-policy", namespace, labels))).To(Succeed())
		Expect(k8sClient.Create(ctx, newDynamicResource("unrelated", namespace, map[string]string{appLabelKey: "other"}))).To(Succeed())

		deploy := newDeployment(appAPI, namespace, labels)
		Expect(k8sClient.Create(ctx, deploy)).To(Succeed())

		reqs := newReconciler().mapDeploymentToDynamicResources(ctx, deploy)
		Expect(reqs).To(ConsistOf(reconcile.Request{
			NamespacedName: types.NamespacedName{Name: "api-policy", Namespace: namespace},
		}))
	})
})
