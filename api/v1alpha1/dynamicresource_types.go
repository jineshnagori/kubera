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

package v1alpha1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// UpdateMode controls whether KubeRA acts on its recommendations.
// +kubebuilder:validation:Enum=Off;InPlaceOnly;InPlaceOrRecreate
type UpdateMode string

const (
	// UpdateModeOff computes and publishes recommendations without touching Pods.
	UpdateModeOff UpdateMode = "Off"
	// UpdateModeInPlaceOnly applies recommendations via the Pod resize
	// subresource only; infeasible resizes are surfaced but never actuated
	// through eviction.
	UpdateModeInPlaceOnly UpdateMode = "InPlaceOnly"
	// UpdateModeInPlaceOrRecreate falls back to evict-and-recreate (respecting
	// PodDisruptionBudgets) when an in-place resize is infeasible.
	UpdateModeInPlaceOrRecreate UpdateMode = "InPlaceOrRecreate"
)

// ControlledValues selects which resource fields KubeRA manages.
// +kubebuilder:validation:Enum=RequestsAndLimits;RequestsOnly
type ControlledValues string

const (
	ControlledValuesRequestsAndLimits ControlledValues = "RequestsAndLimits"
	ControlledValuesRequestsOnly      ControlledValues = "RequestsOnly"
)

// ContainerMode enables or disables management of a single container.
// +kubebuilder:validation:Enum=Auto;Off
type ContainerMode string

const (
	ContainerModeAuto ContainerMode = "Auto"
	ContainerModeOff  ContainerMode = "Off"
)

// MetricsProvider identifies the utilization metrics source.
// +kubebuilder:validation:Enum=MetricsServer;Prometheus
type MetricsProvider string

const (
	MetricsProviderMetricsServer MetricsProvider = "MetricsServer"
	MetricsProviderPrometheus    MetricsProvider = "Prometheus"
)

// HPAMode controls coordination with a HorizontalPodAutoscaler targeting the
// same workload.
// +kubebuilder:validation:Enum=Cooperative;Ignore
type HPAMode string

const (
	// HPAModeCooperative models the coupled control loops: resizes are
	// headroom-checked against the HPA target and paused while HPA is scaling.
	HPAModeCooperative HPAMode = "Cooperative"
	// HPAModeIgnore disables HPA awareness.
	HPAModeIgnore HPAMode = "Ignore"
)

// HPAState describes the current coordination state with HPA.
type HPAState string

const (
	HPAStateIdle          HPAState = "Idle"
	HPAStateScaling       HPAState = "Scaling"
	HPAStateAtMaxVertical HPAState = "AtMaxVertical"
)

// ResourceBounds bounds a single resource dimension. The controller rejects
// (Ready=False, reason InvalidBounds) specs where min exceeds max; a CEL rule
// is not used because quantity() on int-or-string fields exceeds the
// apiserver's validation cost budget.
type ResourceBounds struct {
	// min is the floor KubeRA will never size below.
	// +required
	Min resource.Quantity `json:"min"`

	// max is the ceiling KubeRA will never size above. Reaching max makes the
	// workload eligible for HPA takeover (hpaState: AtMaxVertical).
	// +required
	Max resource.Quantity `json:"max"`
}

// ResourcePolicy bounds CPU and Memory for all containers of the matched
// workloads unless overridden per container.
type ResourcePolicy struct {
	// cpu bounds CPU requests (and limits, per controlledValues).
	// +required
	CPU ResourceBounds `json:"cpu"`

	// memory bounds Memory requests (and limits, per controlledValues).
	// +required
	Memory ResourceBounds `json:"memory"`

	// controlledValues selects whether KubeRA manages requests and limits or
	// requests only (limits left untouched).
	// +kubebuilder:default=RequestsAndLimits
	// +optional
	ControlledValues ControlledValues `json:"controlledValues,omitempty"`
}

// ContainerOverride customizes or disables management for one container.
type ContainerOverride struct {
	// containerName is the name of the container this override applies to.
	// +required
	// +kubebuilder:validation:MinLength=1
	ContainerName string `json:"containerName"`

	// mode disables management of this container when set to Off.
	// +kubebuilder:default=Auto
	// +optional
	Mode ContainerMode `json:"mode,omitempty"`

	// cpu overrides the spec-level CPU bounds for this container.
	// +optional
	CPU *ResourceBounds `json:"cpu,omitempty"`

	// memory overrides the spec-level Memory bounds for this container.
	// +optional
	Memory *ResourceBounds `json:"memory,omitempty"`
}

// MetricsConfig configures the utilization metrics source and sampling.
type MetricsConfig struct {
	// provider is the metrics source.
	// +kubebuilder:default=MetricsServer
	// +optional
	Provider MetricsProvider `json:"provider,omitempty"`

	// pollingInterval is how often utilization samples are collected.
	// +kubebuilder:default="30s"
	// +optional
	PollingInterval *metav1.Duration `json:"pollingInterval,omitempty"`

	// cpuPercentile is the histogram percentile used for CPU recommendations.
	// +kubebuilder:default=90
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=100
	// +optional
	CPUPercentile int32 `json:"cpuPercentile,omitempty"`

	// memoryPercentile is the histogram percentile used for Memory
	// recommendations.
	// +kubebuilder:default=95
	// +kubebuilder:validation:Minimum=50
	// +kubebuilder:validation:Maximum=100
	// +optional
	MemoryPercentile int32 `json:"memoryPercentile,omitempty"`

	// prometheusURL is the base URL of the Prometheus API. Required when
	// provider is Prometheus.
	// +optional
	PrometheusURL string `json:"prometheusURL,omitempty"`
}

// ScalingPolicy shapes resize behaviour in one direction.
type ScalingPolicy struct {
	// stabilizationWindow is the lookback window over which recommendations
	// are aggregated (peak-over-window for scale down) before acting.
	// +optional
	StabilizationWindow *metav1.Duration `json:"stabilizationWindow,omitempty"`

	// maxStepPercent caps how much a single resize may change a resource,
	// as a percentage of its current value.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxStepPercent int32 `json:"maxStepPercent,omitempty"`

	// cooldown is the minimum time between consecutive resizes in this
	// direction.
	// +optional
	Cooldown *metav1.Duration `json:"cooldown,omitempty"`
}

// BehaviorSpec configures scale-up and scale-down policies independently.
// Asymmetric on purpose: scale up fast, scale down slow.
type BehaviorSpec struct {
	// +optional
	ScaleUp *ScalingPolicy `json:"scaleUp,omitempty"`

	// +optional
	ScaleDown *ScalingPolicy `json:"scaleDown,omitempty"`
}

// HPACoordination configures cooperation with an HPA targeting the same
// workload.
type HPACoordination struct {
	// mode selects Cooperative (coupled-loop aware) or Ignore.
	// +kubebuilder:default=Cooperative
	// +optional
	Mode HPAMode `json:"mode,omitempty"`

	// reserveHeadroomPercent keeps post-resize utilization at least this far
	// below the HPA target so a downward resize never re-triggers HPA.
	// +kubebuilder:default=10
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=50
	// +optional
	ReserveHeadroomPercent int32 `json:"reserveHeadroomPercent,omitempty"`

	// pauseWhileHPAScaling pauses all resizes while the HPA is actively
	// scaling replicas.
	// +kubebuilder:default=true
	// +optional
	PauseWhileHPAScaling *bool `json:"pauseWhileHPAScaling,omitempty"`

	// cooldownAfterHPA is how long to wait after the HPA's last scale event
	// before resuming vertical optimization.
	// +kubebuilder:default="10m"
	// +optional
	CooldownAfterHPA *metav1.Duration `json:"cooldownAfterHPA,omitempty"`
}

// DynamicResourceSpec defines the desired state of DynamicResource
type DynamicResourceSpec struct {
	// selector matches Deployments in the same namespace by their labels.
	// Selectors never cross namespace boundaries. Use a label unique to one
	// Deployment to target a single workload.
	// +required
	Selector *metav1.LabelSelector `json:"selector"`

	// updateMode controls whether recommendations are applied or only
	// published in status.
	// +kubebuilder:default=Off
	// +optional
	UpdateMode UpdateMode `json:"updateMode,omitempty"`

	// resources bounds CPU and Memory for all containers of the matched
	// workloads.
	// +required
	Resources ResourcePolicy `json:"resources"`

	// containerOverrides customizes or disables management per container.
	// +optional
	// +listType=map
	// +listMapKey=containerName
	ContainerOverrides []ContainerOverride `json:"containerOverrides,omitempty"`

	// metrics configures the utilization metrics source and sampling.
	// +optional
	Metrics *MetricsConfig `json:"metrics,omitempty"`

	// behavior shapes resize step sizes, stabilization windows and cooldowns.
	// +optional
	Behavior *BehaviorSpec `json:"behavior,omitempty"`

	// hpa configures coordination with an HPA targeting the same workload.
	// +optional
	HPA *HPACoordination `json:"hpa,omitempty"`
}

// WorkloadReference identifies a workload resolved from the selector.
type WorkloadReference struct {
	// +required
	Kind string `json:"kind"`

	// +required
	Name string `json:"name"`
}

// ContainerRecommendation is the recommended and currently applied resources
// for one container.
type ContainerRecommendation struct {
	// +required
	ContainerName string `json:"containerName"`

	// target is the recommended resource values (requests).
	// +optional
	Target corev1.ResourceList `json:"target,omitempty"`

	// applied is what KubeRA last actuated on the running Pods.
	// +optional
	Applied corev1.ResourceList `json:"applied,omitempty"`
}

// WorkloadRecommendation groups container recommendations per workload.
type WorkloadRecommendation struct {
	// workload is the name of the matched workload.
	// +required
	Workload string `json:"workload"`

	// +optional
	// +listType=map
	// +listMapKey=containerName
	Containers []ContainerRecommendation `json:"containers,omitempty"`
}

// DynamicResourceStatus defines the observed state of DynamicResource.
type DynamicResourceStatus struct {
	// conditions represent the current state of the DynamicResource resource.
	//
	// Condition types used by the controller:
	// - "Ready": the selector resolved and the controller is operating
	// - "Conflicted": another DynamicResource or a VPA already manages a
	//   matched workload; this object takes no action
	// - "ResizeInfeasible": the last resize could not fit on the node
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// matchedWorkloads are the workloads resolved from the selector.
	// +optional
	MatchedWorkloads []WorkloadReference `json:"matchedWorkloads,omitempty"`

	// recommendations are published even in Off (recommend-only) mode.
	// +optional
	// +listType=map
	// +listMapKey=workload
	Recommendations []WorkloadRecommendation `json:"recommendations,omitempty"`

	// lastResizeTime is when KubeRA last actuated a resize.
	// +optional
	LastResizeTime *metav1.Time `json:"lastResizeTime,omitempty"`

	// hpaState is the current coordination state with HPA.
	// +optional
	HPAState HPAState `json:"hpaState,omitempty"`

	// observedGeneration is the spec generation last processed by the
	// controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// Condition types used in DynamicResourceStatus.
const (
	ConditionReady            = "Ready"
	ConditionConflicted       = "Conflicted"
	ConditionResizeInfeasible = "ResizeInfeasible"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=dr;dynres
// +kubebuilder:printcolumn:name="Mode",type=string,JSONPath=`.spec.updateMode`
// +kubebuilder:printcolumn:name="HPA State",type=string,JSONPath=`.status.hpaState`
// +kubebuilder:printcolumn:name="Ready",type=string,JSONPath=`.status.conditions[?(@.type=="Ready")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// DynamicResource is the Schema for the dynamicresources API
type DynamicResource struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of DynamicResource
	// +required
	Spec DynamicResourceSpec `json:"spec"`

	// status defines the observed state of DynamicResource
	// +optional
	Status DynamicResourceStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// DynamicResourceList contains a list of DynamicResource
type DynamicResourceList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []DynamicResource `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &DynamicResource{}, &DynamicResourceList{})
		return nil
	})
}
