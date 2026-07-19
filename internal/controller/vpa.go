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

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// +kubebuilder:rbac:groups=autoscaling.k8s.io,resources=verticalpodautoscalers,verbs=get;list;watch

var vpaListGVK = schema.GroupVersionKind{
	Group:   "autoscaling.k8s.io",
	Version: "v1",
	Kind:    "VerticalPodAutoscalerList",
}

// vpaConflicts returns the names of matched Deployments already targeted by a
// VerticalPodAutoscaler. KubeRA refuses to act on those rather than fight VPA
// over the same Pods. Uses unstructured access so the VPA API is optional: a
// cluster without the VPA CRD yields no conflicts.
func (r *DynamicResourceReconciler) vpaConflicts(ctx context.Context, namespace string, matched []appsv1.Deployment) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vpaListGVK)
	if err := r.List(ctx, list, client.InNamespace(namespace)); err != nil {
		if meta.IsNoMatchError(err) {
			return nil, nil // VPA CRD not installed
		}
		return nil, err
	}

	targeted := map[string]bool{}
	for i := range list.Items {
		kind, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "targetRef", "kind")
		name, _, _ := unstructured.NestedString(list.Items[i].Object, "spec", "targetRef", "name")
		if kind == kindDeployment && name != "" {
			targeted[name] = true
		}
	}

	var conflicts []string
	for i := range matched {
		if targeted[matched[i].Name] {
			conflicts = append(conflicts, matched[i].Name)
		}
	}
	return conflicts, nil
}
