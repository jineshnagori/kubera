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

// Package metrics abstracts the utilization metrics source (Metrics Server
// now, Prometheus later) behind a Provider interface so the recommender and
// tests never depend on a concrete backend.
package metrics

import (
	"context"
	"time"

	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/labels"
)

// ContainerUsage is one container's instantaneous usage inside a Pod sample.
type ContainerUsage struct {
	Container string
	CPU       resource.Quantity
	Memory    resource.Quantity
}

// PodUsage is one usage sample for a Pod.
type PodUsage struct {
	Pod        string
	Timestamp  time.Time
	Containers []ContainerUsage
}

// Provider lists current usage samples for Pods matching a label selector in
// one namespace.
type Provider interface {
	ListPodUsage(ctx context.Context, namespace string, selector labels.Selector) ([]PodUsage, error)
}
