package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackendType selects a forecasting implementation.
// +kubebuilder:validation:Enum=TimesFM;SeasonalNaive
type BackendType string

const (
	// BackendTimesFM calls out to a presage-forecaster sidecar or Deployment
	// running Google Research's TimesFM. Zero-shot: no per-cluster training.
	BackendTimesFM BackendType = "TimesFM"

	// BackendSeasonalNaive is an in-process seasonal-naive forecaster with an
	// empirical residual distribution for quantiles. It needs no model server
	// and no GPU, and it is a genuinely hard baseline on clean weekly-seasonal
	// workloads. It exists so that (a) presage is useful with zero extra
	// infrastructure and (b) there is always an honest baseline to measure a
	// foundation model against.
	BackendSeasonalNaive BackendType = "SeasonalNaive"
)

// TimesFMBackend configures the TimesFM model server.
type TimesFMBackend struct {
	// Endpoint is the base URL of a presage-forecaster instance,
	// e.g. "http://presage-forecaster.presage-system:8080".
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// Model is the checkpoint the server should load. Changing this requires
	// restarting the forecaster; presage only reports what the server tells it.
	// +kubebuilder:default="google/timesfm-2.5-200m-pytorch"
	// +optional
	Model string `json:"model,omitempty"`

	// MaxContext is the number of input points the model was compiled for.
	// TimesFM 2.5 supports up to 16384. Together with the signal Resolution
	// this decides how far back the model can see: 4096 points at 5m
	// resolution is ~14 days.
	// +kubebuilder:validation:Minimum=32
	// +kubebuilder:default=4096
	// +optional
	MaxContext int32 `json:"maxContext,omitempty"`

	// MaxHorizon is the number of output points the model was compiled for.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=64
	// +optional
	MaxHorizon int32 `json:"maxHorizon,omitempty"`

	// UseQuantileHead enables TimesFM 2.5's continuous quantile head. Without
	// it you get a point forecast only, and the asymmetric up/down quantile
	// policy degenerates to a single number.
	// +kubebuilder:default=true
	// +optional
	UseQuantileHead *bool `json:"useQuantileHead,omitempty"`

	// Timeout for a single forecast request.
	// +kubebuilder:default="30s"
	// +optional
	Timeout *metav1.Duration `json:"timeout,omitempty"`
}

// SeasonalNaiveBackend configures the built-in baseline forecaster.
type SeasonalNaiveBackend struct {
	// Season is the periodicity to repeat, typically 24h or 168h.
	// +kubebuilder:default="168h"
	// +optional
	Season *metav1.Duration `json:"season,omitempty"`

	// Cycles is how many past seasons to average over. Quantiles come from the
	// empirical distribution of residuals across these cycles.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=3
	// +optional
	Cycles int32 `json:"cycles,omitempty"`
}

// ForecastBackendSpec defines a forecasting backend usable by any
// PredictiveScaler in the cluster.
type ForecastBackendSpec struct {
	// +kubebuilder:default=SeasonalNaive
	Type BackendType `json:"type"`

	// +optional
	TimesFM *TimesFMBackend `json:"timesFM,omitempty"`
	// +optional
	SeasonalNaive *SeasonalNaiveBackend `json:"seasonalNaive,omitempty"`
}

// ForecastBackendStatus reports backend reachability.
type ForecastBackendStatus struct {
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// Model reported by the backend on its last successful probe.
	// +optional
	Model string `json:"model,omitempty"`
	// +optional
	LastProbeTime *metav1.Time `json:"lastProbeTime,omitempty"`
	// +optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fbackend,categories=autoscaling
// +kubebuilder:printcolumn:name="Type",type=string,JSONPath=`.spec.type`
// +kubebuilder:printcolumn:name="Model",type=string,JSONPath=`.status.model`
// +kubebuilder:printcolumn:name="Available",type=string,JSONPath=`.status.conditions[?(@.type=="Available")].status`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ForecastBackend is a cluster-scoped forecasting engine.
type ForecastBackend struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ForecastBackendSpec   `json:"spec,omitempty"`
	Status ForecastBackendStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ForecastBackendList contains a list of ForecastBackend.
type ForecastBackendList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ForecastBackend `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ForecastBackend{}, &ForecastBackendList{})
}
