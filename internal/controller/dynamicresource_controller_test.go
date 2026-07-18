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
)

const (
	appLabelKey = "app"
	appAPI      = "api"
)

func newReconciler() *DynamicResourceReconciler {
	return &DynamicResourceReconciler{
		Client:   k8sClient,
		Scheme:   k8sClient.Scheme(),
		Recorder: events.NewFakeRecorder(16),
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
