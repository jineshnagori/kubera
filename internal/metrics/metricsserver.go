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

package metrics

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	metricsclient "k8s.io/metrics/pkg/client/clientset/versioned"
)

// MetricsServerProvider reads Pod usage from the metrics.k8s.io API
// (Metrics Server).
type MetricsServerProvider struct {
	client metricsclient.Interface
}

var _ Provider = (*MetricsServerProvider)(nil)

func NewMetricsServerProvider(client metricsclient.Interface) *MetricsServerProvider {
	return &MetricsServerProvider{client: client}
}

func (p *MetricsServerProvider) ListPodUsage(ctx context.Context, namespace string, selector labels.Selector) ([]PodUsage, error) {
	podMetrics, err := p.client.MetricsV1beta1().PodMetricses(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector.String(),
	})
	if err != nil {
		return nil, fmt.Errorf("listing pod metrics: %w", err)
	}

	usages := make([]PodUsage, 0, len(podMetrics.Items))
	for i := range podMetrics.Items {
		pm := &podMetrics.Items[i]
		usage := PodUsage{Pod: pm.Name, Timestamp: pm.Timestamp.Time}
		for j := range pm.Containers {
			c := &pm.Containers[j]
			usage.Containers = append(usage.Containers, ContainerUsage{
				Container: c.Name,
				CPU:       c.Usage[corev1.ResourceCPU],
				Memory:    c.Usage[corev1.ResourceMemory],
			})
		}
		usages = append(usages, usage)
	}
	return usages, nil
}
