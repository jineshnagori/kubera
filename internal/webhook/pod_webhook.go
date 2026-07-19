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

// Package webhook injects the current KubeRA recommendation into Pods at
// creation time. Without it, Pods born from rollouts or HPA scale-out start
// with stale template resources and must be re-resized after the fact — at
// exactly the moment (a load spike) when that is most disruptive.
package webhook

import (
	"context"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate--v1-pod,mutating=true,failurePolicy=ignore,sideEffects=None,groups="",resources=pods,verbs=create,versions=v1,name=pods.kubera.io,admissionReviewVersions=v1

// PodResourceDefaulter mutates new Pods whose owning Deployment is managed by
// a DynamicResource, setting container requests/limits to the current
// recommendation. failurePolicy=ignore: pod creation must never be blocked by
// KubeRA being down.
type PodResourceDefaulter struct {
	Client client.Client
}

var _ admission.Defaulter[*corev1.Pod] = (*PodResourceDefaulter)(nil)

func (d *PodResourceDefaulter) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &corev1.Pod{}).
		WithDefaulter(d).
		Complete()
}

func (d *PodResourceDefaulter) Default(ctx context.Context, pod *corev1.Pod) error {
	deployName, err := d.owningDeployment(ctx, pod)
	if err != nil || deployName == "" {
		return nil // not deployment-owned, or owners not resolvable: leave untouched
	}

	rec := d.findRecommendation(ctx, pod.Namespace, deployName)
	if rec == nil {
		return nil
	}

	log := logf.FromContext(ctx)
	for i := range pod.Spec.Containers {
		c := &pod.Spec.Containers[i]
		target := rec[c.Name]
		if target == nil {
			continue
		}
		if c.Resources.Requests == nil {
			continue // never add requests to a BestEffort container (QoS)
		}
		applyTarget(c, target)
		log.V(1).Info("injected recommendation into new pod",
			"pod", pod.GenerateName, "container", c.Name)
	}
	return nil
}

// owningDeployment resolves Pod -> ReplicaSet -> Deployment.
func (d *PodResourceDefaulter) owningDeployment(ctx context.Context, pod *corev1.Pod) (string, error) {
	for _, ref := range pod.OwnerReferences {
		if ref.Kind != "ReplicaSet" {
			continue
		}
		var rs appsv1.ReplicaSet
		if err := d.Client.Get(ctx, types.NamespacedName{Namespace: pod.Namespace, Name: ref.Name}, &rs); err != nil {
			// Fallback: a ReplicaSet is named <deployment>-<hash>.
			if idx := strings.LastIndex(ref.Name, "-"); idx > 0 {
				return ref.Name[:idx], nil
			}
			return "", err
		}
		for _, rsRef := range rs.OwnerReferences {
			if rsRef.Kind == "Deployment" {
				return rsRef.Name, nil
			}
		}
	}
	return "", nil
}

// findRecommendation returns the per-container targets of the DynamicResource
// managing the deployment, or nil.
func (d *PodResourceDefaulter) findRecommendation(ctx context.Context, namespace, deployment string) map[string]corev1.ResourceList {
	var list autoscalingv1alpha1.DynamicResourceList
	if err := d.Client.List(ctx, &list, client.InNamespace(namespace)); err != nil {
		return nil
	}
	for i := range list.Items {
		dr := &list.Items[i]
		if dr.Spec.UpdateMode == autoscalingv1alpha1.UpdateModeOff || dr.Spec.UpdateMode == "" {
			continue // recommend-only: do not mutate pods
		}
		for j := range dr.Status.Recommendations {
			rec := &dr.Status.Recommendations[j]
			if rec.Workload != deployment {
				continue
			}
			out := map[string]corev1.ResourceList{}
			for _, c := range rec.Containers {
				if len(c.Target) > 0 {
					out[c.ContainerName] = c.Target
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	return nil
}

// applyTarget sets requests to the target, scaling limits to preserve the
// container's original limit/request ratio (or pinning limits==requests for
// Guaranteed-style containers).
func applyTarget(c *corev1.Container, target corev1.ResourceList) {
	for _, name := range []corev1.ResourceName{corev1.ResourceCPU, corev1.ResourceMemory} {
		newReq, ok := target[name]
		if !ok {
			continue
		}
		curReq, hasReq := c.Resources.Requests[name]
		if !hasReq || curReq.IsZero() {
			continue
		}
		if curLim, hasLim := c.Resources.Limits[name]; hasLim {
			if curLim.Cmp(curReq) == 0 {
				c.Resources.Limits[name] = newReq.DeepCopy()
			} else {
				ratio := curLim.AsApproximateFloat64() / curReq.AsApproximateFloat64()
				scaledMilli := int64(newReq.AsApproximateFloat64() * ratio * 1000)
				c.Resources.Limits[name] = *resource.NewMilliQuantity(scaledMilli, curLim.Format)
			}
		}
		c.Resources.Requests[name] = newReq.DeepCopy()
	}
}
