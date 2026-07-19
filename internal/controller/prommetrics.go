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
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/metrics"

	autoscalingv1alpha1 "github.com/jineshnagori/kubera/api/v1alpha1"
)

// Operator metrics for dashboards and cost reporting. Reclaimable gauges are
// the gap between what workloads currently request and what they need — the
// direct input for savings estimates.
const (
	labelNamespace       = "namespace"
	labelDynamicResource = "dynamicresource"
	labelWorkload        = "workload"
)

var (
	recommendedCPU = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubera_recommended_cpu_millicores",
		Help: "Current recommended CPU request per container.",
	}, []string{labelNamespace, labelDynamicResource, labelWorkload, "container"})

	recommendedMemory = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubera_recommended_memory_bytes",
		Help: "Current recommended memory request per container.",
	}, []string{labelNamespace, labelDynamicResource, labelWorkload, "container"})

	reclaimableCPU = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubera_reclaimable_cpu_millicores",
		Help: "CPU currently requested by running pods above the recommendation (0 when under-provisioned).",
	}, []string{labelNamespace, labelDynamicResource, labelWorkload})

	reclaimableMemory = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "kubera_reclaimable_memory_bytes",
		Help: "Memory currently requested by running pods above the recommendation (0 when under-provisioned).",
	}, []string{labelNamespace, labelDynamicResource, labelWorkload})

	resizesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "kubera_pod_resizes_total",
		Help: "In-place pod resizes performed.",
	}, []string{labelNamespace, labelWorkload, "direction"})
)

func init() {
	metrics.Registry.MustRegister(
		recommendedCPU, recommendedMemory, reclaimableCPU, reclaimableMemory, resizesTotal)
}

// publishRecommendationMetrics exports recommendation gauges; works in every
// updateMode including Off (recommend-only cost visibility).
func publishRecommendationMetrics(dr *autoscalingv1alpha1.DynamicResource, recs []autoscalingv1alpha1.WorkloadRecommendation) {
	for _, w := range recs {
		for _, c := range w.Containers {
			cpu := c.Target[corev1.ResourceCPU]
			mem := c.Target[corev1.ResourceMemory]
			labels := []string{dr.Namespace, dr.Name, w.Workload, c.ContainerName}
			recommendedCPU.WithLabelValues(labels...).Set(float64(cpu.MilliValue()))
			recommendedMemory.WithLabelValues(labels...).Set(float64(mem.Value()))
		}
	}
}

// publishReclaimableMetrics exports the positive gap between running pods'
// requests and the recommendation for one workload.
func publishReclaimableMetrics(dr *autoscalingv1alpha1.DynamicResource, workload string, pods []corev1.Pod, targets map[string]corev1.ResourceList) {
	var cpuGapMilli, memGapBytes float64
	for i := range pods {
		pod := &pods[i]
		if pod.DeletionTimestamp != nil || pod.Status.Phase != corev1.PodRunning {
			continue
		}
		for j := range pod.Spec.Containers {
			c := &pod.Spec.Containers[j]
			target, ok := targets[c.Name]
			if !ok {
				continue
			}
			curCPU := c.Resources.Requests[corev1.ResourceCPU]
			curMem := c.Resources.Requests[corev1.ResourceMemory]
			tgtCPU := target[corev1.ResourceCPU]
			tgtMem := target[corev1.ResourceMemory]
			if d := curCPU.MilliValue() - tgtCPU.MilliValue(); d > 0 {
				cpuGapMilli += float64(d)
			}
			if d := curMem.Value() - tgtMem.Value(); d > 0 {
				memGapBytes += float64(d)
			}
		}
	}
	reclaimableCPU.WithLabelValues(dr.Namespace, dr.Name, workload).Set(cpuGapMilli)
	reclaimableMemory.WithLabelValues(dr.Namespace, dr.Name, workload).Set(memGapBytes)
}
