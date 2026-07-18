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
	"slices"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
)

const defaultPollingInterval = 30 * time.Second

// DynamicResourceReconciler reconciles a DynamicResource object
type DynamicResourceReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder
}

// +kubebuilder:rbac:groups=autoscaling.kubera.io,resources=dynamicresources,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=autoscaling.kubera.io,resources=dynamicresources/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=autoscaling.kubera.io,resources=dynamicresources/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/resize,verbs=patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=autoscaling,resources=horizontalpodautoscalers,verbs=get;list;watch

// Reconcile resolves the selector to Deployments in the same namespace,
// detects conflicts with other DynamicResources, and publishes the result in
// status. Recommendation and actuation stages hook in behind these guards.
func (r *DynamicResourceReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var dr autoscalingv1alpha1.DynamicResource
	if err := r.Get(ctx, req.NamespacedName, &dr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	orig := dr.DeepCopy()
	dr.Status.ObservedGeneration = dr.Generation

	if err := validateBounds(&dr.Spec); err != nil {
		r.setCondition(&dr, autoscalingv1alpha1.ConditionReady, metav1.ConditionFalse, "InvalidBounds", err.Error())
		return r.patchStatus(ctx, orig, &dr, ctrl.Result{})
	}

	matched, err := r.resolveSelector(ctx, &dr)
	if err != nil {
		r.setCondition(&dr, autoscalingv1alpha1.ConditionReady, metav1.ConditionFalse, "InvalidSelector", err.Error())
		return r.patchStatus(ctx, orig, &dr, ctrl.Result{})
	}

	dr.Status.MatchedWorkloads = workloadRefs(matched)

	conflicted, err := r.conflictedWorkloads(ctx, &dr, matched)
	if err != nil {
		return ctrl.Result{}, err
	}
	if len(conflicted) > 0 {
		msg := fmt.Sprintf("workloads already managed by an older DynamicResource: %v", conflicted)
		r.setCondition(&dr, autoscalingv1alpha1.ConditionConflicted, metav1.ConditionTrue, "OverlappingSelector", msg)
		r.setCondition(&dr, autoscalingv1alpha1.ConditionReady, metav1.ConditionFalse, "Conflicted", msg)
		r.Recorder.Eventf(&dr, nil, "Warning", "Conflicted", "Reconcile", "%s", msg)
		return r.patchStatus(ctx, orig, &dr, ctrl.Result{})
	}
	r.setCondition(&dr, autoscalingv1alpha1.ConditionConflicted, metav1.ConditionFalse, "NoConflict", "no overlapping DynamicResource")

	if len(matched) == 0 {
		r.setCondition(&dr, autoscalingv1alpha1.ConditionReady, metav1.ConditionFalse, "NoMatch", "selector matches no Deployment in namespace")
		return r.patchStatus(ctx, orig, &dr, ctrl.Result{RequeueAfter: r.pollingInterval(&dr)})
	}

	r.setCondition(&dr, autoscalingv1alpha1.ConditionReady, metav1.ConditionTrue, "Reconciled",
		fmt.Sprintf("managing %d workload(s)", len(matched)))
	if dr.Status.HPAState == "" {
		dr.Status.HPAState = autoscalingv1alpha1.HPAStateIdle
	}

	log.V(1).Info("reconciled", "matched", len(matched), "updateMode", dr.Spec.UpdateMode)
	return r.patchStatus(ctx, orig, &dr, ctrl.Result{RequeueAfter: r.pollingInterval(&dr)})
}

// resolveSelector lists Deployments in the DynamicResource's own namespace
// matching spec.selector. Selectors never cross namespaces.
func (r *DynamicResourceReconciler) resolveSelector(ctx context.Context, dr *autoscalingv1alpha1.DynamicResource) ([]appsv1.Deployment, error) {
	sel, err := metav1.LabelSelectorAsSelector(dr.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("invalid selector: %w", err)
	}
	if sel.Empty() {
		return nil, fmt.Errorf("selector must not be empty")
	}

	var deployments appsv1.DeploymentList
	if err := r.List(ctx, &deployments,
		client.InNamespace(dr.Namespace),
		client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, err
	}

	slices.SortFunc(deployments.Items, func(a, b appsv1.Deployment) int {
		return strings.Compare(a.Name, b.Name)
	})
	return deployments.Items, nil
}

// conflictedWorkloads returns names of matched workloads already claimed by an
// older DynamicResource. Oldest object wins; ties break on name.
func (r *DynamicResourceReconciler) conflictedWorkloads(ctx context.Context, dr *autoscalingv1alpha1.DynamicResource, matched []appsv1.Deployment) ([]string, error) {
	var all autoscalingv1alpha1.DynamicResourceList
	if err := r.List(ctx, &all, client.InNamespace(dr.Namespace)); err != nil {
		return nil, err
	}

	var conflicted []string
	for i := range matched {
		deploy := &matched[i]
		for j := range all.Items {
			other := &all.Items[j]
			if other.Name == dr.Name || !olderThan(other, dr) {
				continue
			}
			otherSel, err := metav1.LabelSelectorAsSelector(other.Spec.Selector)
			if err != nil || otherSel.Empty() {
				continue
			}
			if otherSel.Matches(labels.Set(deploy.Labels)) {
				conflicted = append(conflicted, deploy.Name)
				break
			}
		}
	}
	return conflicted, nil
}

// validateBounds enforces min <= max on every bounds pair; done here rather
// than CEL because quantity() on int-or-string fields blows the apiserver's
// validation cost budget.
func validateBounds(spec *autoscalingv1alpha1.DynamicResourceSpec) error {
	check := func(name string, b *autoscalingv1alpha1.ResourceBounds) error {
		if b != nil && b.Min.Cmp(b.Max) > 0 {
			return fmt.Errorf("%s: min %s exceeds max %s", name, b.Min.String(), b.Max.String())
		}
		return nil
	}
	if err := check("resources.cpu", &spec.Resources.CPU); err != nil {
		return err
	}
	if err := check("resources.memory", &spec.Resources.Memory); err != nil {
		return err
	}
	for i := range spec.ContainerOverrides {
		o := &spec.ContainerOverrides[i]
		prefix := fmt.Sprintf("containerOverrides[%s]", o.ContainerName)
		if err := check(prefix+".cpu", o.CPU); err != nil {
			return err
		}
		if err := check(prefix+".memory", o.Memory); err != nil {
			return err
		}
	}
	return nil
}

func olderThan(a, b *autoscalingv1alpha1.DynamicResource) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

func (r *DynamicResourceReconciler) pollingInterval(dr *autoscalingv1alpha1.DynamicResource) time.Duration {
	if m := dr.Spec.Metrics; m != nil && m.PollingInterval != nil && m.PollingInterval.Duration > 0 {
		return m.PollingInterval.Duration
	}
	return defaultPollingInterval
}

func (r *DynamicResourceReconciler) setCondition(dr *autoscalingv1alpha1.DynamicResource, condType string, status metav1.ConditionStatus, reason, message string) {
	meta.SetStatusCondition(&dr.Status.Conditions, metav1.Condition{
		Type:               condType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: dr.Generation,
	})
}

func (r *DynamicResourceReconciler) patchStatus(ctx context.Context, orig, dr *autoscalingv1alpha1.DynamicResource, res ctrl.Result) (ctrl.Result, error) {
	if equality.Semantic.DeepEqual(orig.Status, dr.Status) {
		return res, nil
	}
	if err := r.Status().Patch(ctx, dr, client.MergeFrom(orig)); err != nil {
		return ctrl.Result{}, err
	}
	return res, nil
}

func workloadRefs(deployments []appsv1.Deployment) []autoscalingv1alpha1.WorkloadReference {
	refs := make([]autoscalingv1alpha1.WorkloadReference, 0, len(deployments))
	for i := range deployments {
		refs = append(refs, autoscalingv1alpha1.WorkloadReference{Kind: "Deployment", Name: deployments[i].Name})
	}
	return refs
}

// mapDeploymentToDynamicResources enqueues every DynamicResource in the
// Deployment's namespace whose selector matches its labels.
func (r *DynamicResourceReconciler) mapDeploymentToDynamicResources(ctx context.Context, obj client.Object) []ctrl.Request {
	var list autoscalingv1alpha1.DynamicResourceList
	if err := r.List(ctx, &list, client.InNamespace(obj.GetNamespace())); err != nil {
		return nil
	}

	var reqs []ctrl.Request
	for i := range list.Items {
		dr := &list.Items[i]
		sel, err := metav1.LabelSelectorAsSelector(dr.Spec.Selector)
		if err != nil || sel.Empty() {
			continue
		}
		if sel.Matches(labels.Set(obj.GetLabels())) {
			reqs = append(reqs, ctrl.Request{NamespacedName: types.NamespacedName{
				Namespace: dr.Namespace, Name: dr.Name,
			}})
		}
	}
	return reqs
}

// SetupWithManager sets up the controller with the Manager.
func (r *DynamicResourceReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&autoscalingv1alpha1.DynamicResource{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapDeploymentToDynamicResources)).
		Named("dynamicresource").
		Complete(r)
}
